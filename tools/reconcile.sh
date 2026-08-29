#!/usr/bin/env bash
# Reconcile M2's pre-merge prediction against M4's live measurement.
#
# This is the platform's central claim, tested rather than asserted. M2 prices manifests in
# Git before a change can merge; M4 prices what the cluster actually reserved after it
# deployed. Both read the same rate table through the same code, so for reserved capacity
# the two figures should agree, and where they do not, something real has happened:
# a workload was scaled outside Git, a manifest never reached the cluster, or an assumption
# in the estimate does not hold.
#
# A FinOps platform that cannot demonstrate this is producing two unrelated numbers that
# merely share a currency symbol.
set -euo pipefail

TOLERANCE=${TOLERANCE:-0.02}   # 2%, allows for rounding, not for disagreement
PROM_PORT=${PROM_PORT:-9090}
PROM_SVC=svc/observability-kube-prometh-prometheus

command -v ./bin/gate >/dev/null 2>&1 || { echo "build the gate first: make gate-build"; exit 1; }

echo "==> pricing manifests with M2 (the pre-merge estimate)"
./bin/gate price --config gate.yaml --format json > /tmp/gate-price.json
python3 - <<'PY'
import json
d = json.load(open('/tmp/gate-price.json'))
print("    pricing table v{}, {} workloads, ${:.2f}/month total".format(
    d['pricing_version'], len(d['workloads']), d['monthly_usd']))
PY

echo "==> reading M4's live attribution from Prometheus"
kubectl -n observability port-forward "$PROM_SVC" "${PROM_PORT}:9090" >/dev/null 2>&1 &
PF=$!
trap 'kill $PF 2>/dev/null || true' EXIT

# The port-forward needs a moment, and Prometheus needs to have scraped the exporter at
# least once. Polling beats a fixed sleep, which is either too short or wasteful.
for _ in $(seq 1 30); do
  if curl -sf "http://localhost:${PROM_PORT}/-/ready" >/dev/null 2>&1; then break; fi
  sleep 1
done

QUERY='gitops_platform_workload_cost_usd_per_month{basis="requested"}'
curl -sG "http://localhost:${PROM_PORT}/api/v1/query" --data-urlencode "query=${QUERY}" > /tmp/live-cost.json

python3 - "$TOLERANCE" <<'PY'
import json, sys

tolerance = float(sys.argv[1])

predicted = {}
for w in json.load(open('/tmp/gate-price.json'))['workloads']:
    # M4 attributes by (namespace, workload name), so the estimate is keyed the same way.
    # The workload kind is deliberately not part of the key: a Deployment and the
    # StatefulSet that replaced it are the same workload from a budgeting point of view.
    if w.get('namespace') and w.get('name'):
        # The whole workload is kept, not just monthly_usd: an autoscaled one is priced at
        # its ceiling, and reconciling it needs the floor as well. See the loop below.
        predicted[(w['namespace'], w['name'])] = w

live = json.load(open('/tmp/live-cost.json'))
if live.get('status') != 'success':
    print(f"FAIL: Prometheus query failed: {live}")
    raise SystemExit(1)

measured = {}
for r in live['data']['result']:
    m = r['metric']
    measured[(m['namespace'], m['workload'])] = float(r['value'][1])

if not measured:
    print("FAIL: M4 reported no workloads. Has Prometheus scraped the cost exporter yet?")
    raise SystemExit(1)

print()
print(f"{'WORKLOAD':<38} {'M2 PREDICTED':>13} {'M4 MEASURED':>13} {'DIFF':>9}")
print("-" * 76)

failures, missing, compared = [], [], 0
for key in sorted(set(predicted) | set(measured)):
    ns, name = key
    p, m = predicted.get(key), measured.get(key)
    label = f"{ns}/{name}"

    p_usd = p['monthly_usd'] if p is not None else None
    if p is None:
        # Running but not described by any manifest the gate can see. Either deployed
        # outside GitOps, or delivered from a Helm chart the gate cannot render. Reported
        # rather than failed: ArgoCD and the monitoring stack are legitimately in this
        # category.
        print(f"{label:<38} {'-':>13} {m:>13.2f} {'not in Git':>9}")
        continue
    if m is None:
        # This one IS a failure. Git says the workload exists and the gate priced it, but
        # M4 cannot see it running. Either it never deployed, or, the subtler case -
        # its cost is being attributed under the wrong labels, which is indistinguishable
        # from absent when you look workload by workload.
        print(f"{label:<38} {p_usd:>13.2f} {'-':>13} {'MISSING':>9}")
        missing.append(label)
        continue

    compared += 1

    if p.get('autoscaled'):
        # An autoscaled workload has no single true cost. The gate deliberately prices it
        # at the ceiling, what the change *authorises*, while M4 measures wherever demand
        # has currently put it. Comparing the measurement against the ceiling therefore
        # fails every idle scaler by construction: demo-worker sits at min=1 of max=6 with
        # no load, so the ceiling is exactly 6x the largest figure M4 could report, and the
        # test called a correctly-working platform an 83% cost discrepancy.
        #
        # What Git actually promises for these is a band, so the band is what is checked.
        # A workload outside it is still a real finding: the cluster is running something
        # the merged manifest never authorised.
        floor = p.get('floor_monthly_usd', p_usd)
        low, high = floor * (1 - tolerance), p_usd * (1 + tolerance)
        shown = f"{floor:.2f}-{p_usd:.2f}"
        if low <= m <= high:
            print(f"{label:<38} {shown:>13} {m:>13.2f} {'ok':>9}")
        else:
            nearest = floor if m < low else p_usd
            diff = abs(nearest - m) / max(nearest, 0.0001)
            failures.append((label, f"a band of ${shown}", m, diff))
            print(f"{label:<38} {shown:>13} {m:>13.2f} {'OUTSIDE':>9}")
        continue

    diff = abs(p_usd - m) / max(p_usd, 0.0001)
    flag = "ok" if diff <= tolerance else "MISMATCH"
    if diff > tolerance:
        failures.append((label, f"${p_usd:.2f}", m, diff))
    print(f"{label:<38} {p_usd:>13.2f} {m:>13.2f} {flag:>9}")

print()
if not compared:
    print("FAIL: no workload appeared on both sides, so nothing was actually reconciled.")
    raise SystemExit(1)

if missing:
    print(f"FAIL: {len(missing)} workload(s) exist in Git but were not measured live:")
    for label in missing:
        print(f"  {label}")
    print()
    print("Either they never deployed, or their cost is being attributed under different")
    print("labels than expected, check honorLabels on the exporter's PodMonitor.")
    raise SystemExit(1)

if failures:
    print(f"FAIL: {len(failures)} workload(s) disagree by more than {tolerance:.0%}:")
    for label, desc, m, diff in failures:
        print(f"  {label}: M2 predicted {desc}, M4 measured ${m:.2f} ({diff:.1%} apart)")
    print()
    print("A gap here is a finding, not a bug in the test: the cluster is reserving")
    print("something different from what Git describes.")
    raise SystemExit(1)

print(f"PASS: {compared} workload(s) reconcile within {tolerance:.0%}.")
print("      The cost predicted before merge is the cost being reserved in the cluster.")
PY
