# Handoff — Cost- and Risk-Aware GitOps Platform

Written 2026-08-27. Everything below is verified state, not intention.

Repository: https://github.com/AbdurahmanAlmehdi/cost-risk-aware-gitops-platform (private)

## Why this handoff exists

The remaining work needs the **Cloudflare MCP servers**, which cannot attach to the session
that was doing the work. The plugins are installed (`claude plugin install cloudflare@cloudflare`)
and the user ran `/reload-plugins`, but MCP servers bind at session start — `ToolSearch` for
`cloudflare` returns nothing in the old session. **A fresh session should have them.**

Verify first: search for a Cloudflare tool. If they are still absent, fall back to the
`cloudflared` CLI, which is authenticated via `~/.cloudflared/cert.pem` and works today.

## Goal

A graduation project: a Kubernetes platform that **prices and checks a change before it can
merge, deploys it only after it passes, then measures it against that prediction while it
runs.** Merges three earlier proposals (FinOps CI/CD, self-healing autoscaling, zero-trust
network) into one system. Spec of record: `docs/LLD.md`. Modules M1–M8.

Immediate objective: get it reachable by teammates for review, behind authentication,
without exposing the cluster.

## Current progress

| Phase | Module | State |
|---|---|---|
| 0 | Cluster substrate | Done — kind, Calico, enforcement verified |
| 1 | M1 Source & CI | Done — Go workload, multi-arch GHCR publish, digest-pinned |
| 2 | M2 Pre-Merge Gate | Done — cost + policy, 18 rules, ~22 tests, live on PRs |
| 3 | M3 GitOps Delivery | Done — ArgoCD app-of-apps; drift reverts ~5s, merge→cluster 41s |
| 4 | M4 / M7 | Done — cost exporter + Grafana; prediction reconciles with measurement |
| 5 | M5 Elasticity | Done — KEDA 1→6 in 15s, backlog drained, cost tracked the curve |
| 6 | M6 Security Baseline | **Partly** — policies merged and applied, matrix test NOT re-run |
| 7 | M8 Cost Intelligence | Not started |

The headline result, measured: M2 priced a change's ceiling at **$70.36/month** before merge;
under load M4 measured **$70.36**. `make reconcile` reproduces it.

## Uncommitted / in-flight

Branch **`feat/demo-dashboard`** — one commit (`6febf32`), gate passing, **not pushed**.

⚠️ **This branch predates `main`.** It forked at `d4695cc`; main is now `496897a` (PR #18,
merged). **Rebase onto main before pushing** — the branch does not contain the single-node
scheduling fix.

It contains:
- `manifests/apps/edge/` — Caddy + cloudflared (the "edge stack")
- `manifests/argocd/apps/edge.yaml` — its ArgoCD Application, sync-wave 2
- `manifests/apps/demo-api/networkpolicy.yaml` — allow-rule for `edge → demo-api`
- `tools/edge-secrets.sh`, `make edge-secrets`, `make edge-routes`
- Gate fix: percentage now measured against the **whole estate**, not just changed roots
- `dashboard/go.mod`, `dashboard/kube.go` — **incomplete**, see Next Steps

## Infrastructure that exists right now

**AWS** (account `240571106679`, region `eu-central-1`, CLI authenticated as `abdurahman-cli`)

- EC2 `i-054dedc804b0ca4e0` — `m7i.xlarge`, **currently stopped**. Only its 40GB disk bills (~$3.50/mo).
- Running it costs **$0.2415/hr ($5.80/day)**. Start/stop: `make demo-host-start` / `demo-host-stop` / `demo-host-status`.
- Security group `sg-08dab2da789bdaefc` — every rule is a `/32` to the user's IP. **Nothing open to 0.0.0.0/0.**
- Budget `gitops-platform-demo-cap`, **$40/month**, with a `RUN_SSM_DOCUMENTS` action that
  genuinely **stops the instance** at 100% (reuses the existing `BudgetStopEC2Role`).
- k3s installed via user-data, plus an idle auto-stop (90 min without a session → shutdown).
- **The public IP changes on every start.** No Elastic IP (an idle one bills).

**Cloudflare**

- Domain `abdurahman.ly` is on Cloudflare (`jaxson`/`olivia` nameservers).
- Tunnel **`gitops-platform`** created, id `52d405a8-ed59-4109-aadf-74bc5bcafd6c`.
- Credentials at `~/.cloudflared/52d405a8-....json`, account cert at `~/.cloudflared/cert.pem`.
- **No DNS records created yet — deliberately.** `gitops` / `argocd` / `grafana`.abdurahman.ly
  are all free.

**Pinned image digests** (GATE-014 blocks unpinned images):
- `caddy@sha256:4c6e91c6ed0e2fa03efd5b44747b625fec79bc9cd06ac5235a779726618e530d`
- `cloudflare/cloudflared@sha256:0aa26e284f05e6c77ae375b8c9c11d9eb6a448fb7bcd8d40f31cb6176189eb38`

## What worked

- **Prove enforcement, never assume it.** `tools/verify-cni.sh` checks that NetworkPolicy is
  *enforced*, not merely accepted. This caught that kind's default CNI enforces nothing.
- **Every claim is a command.** `make verify` runs CNI enforcement → drift → reconciliation →
  connectivity matrix → load test. This is the demo (see `docs/DEMO.md`).
- **One pricing module** shared by M2 and M4, so prediction and measurement are comparable
  by construction. This is the project's central argument.
- **A startup canary in the policy engine.** If the rules stop matching anything, the gate
  refuses to run rather than approving every PR.
- **Everything through PRs**, including changes to the gate's own config — the platform
  governs itself. 18 PRs, all gated.

## What did NOT work — do not repeat

- **Cloudflare MCP in the old session.** Installed, `/reload-plugins` run, still invisible.
  Needs a fresh session. This is the reason for this handoff.
- **The laptop kind cluster is exhausted.** At nine ArgoCD Applications it hit ~1390% CPU with
  the API server timing out. The dominant driver was ArgoCD reconciliation (now `600s`) —
  scaling the application-controller to 0 dropped CPU to ~239%. **Do not add more to it.**
  Use the AWS host or DOKS (`make doks-up`, `docs/DEPLOY-DOKS.md`).
- **`t3.xlarge` is a trap.** Burstable, throttles to 1.6 sustained cores — the exact saturation
  that broke the laptop, and it bills surplus CPU unpredictably. `m7i.xlarge` was chosen for this.
- **Distroless containers cannot be probed with `nc`.** The connectivity matrix reported working
  paths as denied because it exec'd netcat into images that have no shell. Now probes from
  throwaway pods wearing each workload's labels.
- **kustomize silently ignores files not in `resources:`.** The Grafana dashboard was committed,
  reviewed, gated, merged and deployed while doing nothing at all.
- **Prometheus target labels override an exporter's own labels.** Every workload's cost was
  attributed to the `observability` namespace until `honorLabels: true` was set. Values were
  right; attribution was silently wrong.
- **A hard `nodeSelector` breaks single-node clusters** — one node cannot carry two values of
  one label key. Fixed in PR #18 (preferred nodeAffinity; the taint still enforces isolation).
- **The percentage in the cost gate was scope-dependent.** Measured against only changed roots,
  a $2.88 change read as 57.7%. Now measured against the whole estate (3.6%). Note: the first
  attempt fixed the *displayed* number while the verdict still gated on the wrong one.
- **EC2 Instance Connect keys expire in ~60 seconds.** Re-push before each SSH.
  The private key for the `abdurahman-t3code` key pair is **not on this machine** — use
  `aws ec2-instance-connect send-ssh-public-key` with a generated key.
- **A web terminal was requested and deliberately refused.** A browser shell on a host holding a
  cluster-admin kubeconfig is a remote-code-execution endpoint. The dashboard uses fixed
  actions via API calls instead — no shell path exists even in principle. **Keep it that way.**

## How delivery works (there is no deploy step)

**CI never pushes to the cluster, and holds no cluster credentials.** This is deliberate —
LLD §9.4 — not an omission:

```
PR opened   → CI: test → build+publish image (digest-pinned) → gate posts verdict
merge       → CI: publish image. That is all CI does.
ArgoCD      → running INSIDE the cluster, notices Git changed, pulls and applies
```

Delivery is pull-based, so a compromised pipeline cannot reach the cluster. It is also why
the cluster needs no inbound access at all, which is what makes the Cloudflare tunnel a
clean fit rather than a workaround.

Consequence: reconciliation is on a `600s` interval with no webhook, so a merge takes up to
ten minutes to appear. Measured merge→cluster was 41s when the interval was shorter. Adding a
webhook is possible once the tunnel exists, but pull-based delivery should stay the default.

### KNOWN GAP: application code changes do not reach the cluster

Two paths, and only one is closed:

| Change | Result |
|---|---|
| A manifest (YAML) | merge → ArgoCD pulls → **deployed** |
| Application code (Go) | merge → CI publishes a new image → **nothing deploys it** |

The manifests pin a digest (`sha256:1b009011…`); CI publishes new digests but **never writes
to the repository**. So a new image exists that nothing references. This is a direct
consequence of digest pinning — Git is the authority, and nothing updates the digest in Git.

**Do not fix this with ArgoCD Image Updater committing to `main`.** It would bypass M2, making
the one change type that alters what actually runs the one change nobody reviews — which
contradicts the platform's central claim.

**Fix: CI opens a pull request that bumps the digest**, which then passes through the gate
like any other change. Not yet implemented. It needs `contents: write` + `pull-requests: write`
on a dedicated job, editing the `digest:` line in the relevant
`manifests/apps/*/kustomization.yaml` (deliberately kept as a single line for exactly this).

## Security position

Audited on the AWS host. Findings and their status:

| Finding | Status |
|---|---|
| Security group is `/32`-only, nothing world-open | OK |
| SSH key-only, passwords disabled | OK |
| k8s API (6443) + kubelet (10250) bound to `*` | **Protected only by the security group.** The tunnel is the fix: once cloudflared runs, delete every inbound rule. |
| ArgoCD / Grafana on plaintext HTTP | Mitigated once Cloudflare terminates TLS |
| Demo API has **no authentication** | Closed by design: it gets **no public hostname**. The dashboard calls it in-cluster. |

**Basic auth (Caddy) is the interim inner layer** — no identity, no MFA, no per-person
revocation. **Cloudflare Access is the intended outer layer** and needs an API token with
`Access:Edit`, which `cert.pem` does not provide.

## Next steps

1. **Verify Cloudflare MCP is available.** If yes, prefer it over the CLI — it can create Access
   applications, which `cert.pem` cannot.
2. **Rebase `feat/demo-dashboard` onto `main`** (it predates PR #18), then push and open a PR.
3. **Finish the demo dashboard.** `dashboard/kube.go` is done (hand-rolled Kubernetes +
   Prometheus HTTP clients — client-go was deliberately avoided). Still needed:
   - HTTP handlers: `GET /api/status`, `POST /api/demo/load`, `POST /api/demo/drift`, SSE progress
   - `dashboard/web/index.html` — walks `docs/DEMO.md` as steps with **buttons only**, live
     status tiles, written so a non-technical viewer can follow it
   - `Dockerfile`, CI matrix entry, manifests under `manifests/apps/demo-dashboard/`
   - Route it at `gitops.abdurahman.ly` (Caddy already points there)
4. **Bring the edge stack up**, in this order — the order matters:
   - `make demo-host-start`, then deploy the platform to the k3s host
   - `make edge-secrets` (prints the reviewer password once)
   - **Verify Caddy actually challenges for credentials**
   - Only then create DNS: `make edge-routes`, or add Public Hostnames in the Zero Trust
     dashboard (same CNAMEs, and it is where Access attaches)
   - Then **delete every inbound security-group rule** — that is the payoff
5. **Add Cloudflare Access** over the three hostnames, allow-listing teammate emails.
6. **Re-run `make verify` on a healthy cluster.** Two M6 checks were never confirmed: the
   rewritten connectivity matrix, and KEDA's Redis path (its `connection refused` looked like
   Redis restarting, not a policy block — Redis had 8 restarts from cluster instability).
7. **Close the image-update loop** (see the known gap above): a CI job that opens a digest-bump
   PR after publishing. Without it, application code changes never reach the cluster — which
   will be confusing during a demonstration if someone edits Go and nothing happens.
8. **M8** (cost anomaly detection + PR explainer) is the last unstarted module.

## Useful commands

```bash
make help                 # every target
make verify               # every proof the platform makes about itself
make demo-host-status     # instance state, address, month-to-date vs the $40 cap
make cost                 # price the manifests as they stand
make gate BASE=main       # run the gate locally
```

Keep [PR #1](https://github.com/AbdurahmanAlmehdi/cost-risk-aware-gitops-platform/pull/1) open —
it is a real change, still blocked, with a costed and explained reason. It is the single best
demonstration asset and it works with no cluster running.
