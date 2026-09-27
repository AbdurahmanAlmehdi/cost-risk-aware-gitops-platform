# Restoring the review host

The review host is disposable. It lives stopped, and if it is ever terminated the
40 GB root disk is what carries the platform — k3s, ArgoCD, Grafana, the exporter
and every bit of demonstration state live on it. This is how to bring it back.

## What the host is

Recovered on 2026-09-27 from the CloudTrail `RunInstances` record for
`i-054dedc804b0ca4e0` (launched 2026-08-27, terminated 2026-09-08).

| | |
|---|---|
| Instance type | **`m7i.xlarge`** — 4 vCPU, 16 GB |
| Rate | **$0.2415/hour** in `eu-central-1` |
| Architecture | `x86_64` |
| Base image | `ami-04bc554a9635a77c8` — Ubuntu 24.04 (noble) amd64, `hvm-ssd-gp3` |
| Root device | `/dev/sda1`, 40 GB |
| Boot mode | `uefi-preferred`, IMDSv2 required, ENA + `sriov simple` |
| Availability zone | **`eu-central-1b`** — not a preference, see below |
| Security group | `sg-08dab2da789bdaefc` (`gitops-platform-demo-sg`) |
| Key pair | `abdurahman-t3code` — though access is normally EC2 Instance Connect |
| Snapshot | **`snap-0807cef72a853334b`** (40 GB, unencrypted) |

**WRITE THIS DOWN RATHER THAN RE-DERIVING IT.** CloudTrail's event history keeps
90 days. The launch record above expires around **25 November 2026**, and after
that the only way to recover the instance type is to recognise $0.2415/hour in
`edge-control/worker.js` as an `m7i.xlarge`.

**The AZ is load-bearing.** An EBS volume can only attach to an instance in its
own zone, and the snapshot is a `eu-central-1b` volume. Launching in `1a`
produces a confusing failure well after the point you thought you were finished.

**Which snapshot.** Two snapshots were retained when the projects were paused,
both originally tagged only "t3code / gitops-platform-demo — identify on
restore". The 40 GB one is this host: `tools/demo-host.sh` says "only its 40GB
disk persists". The 30 GB encrypted one is the t3code dev box. Both are now
tagged with the answer, so this paragraph should never be needed again.

## Restoring

### 1. Register an image from the snapshot

A snapshot cannot be booted; an AMI can. The attributes below are copied from the
original Ubuntu AMI — `uefi-preferred` and `ena-support` in particular, because
an `m7i` will not launch from an image that claims neither.

```bash
aws ec2 register-image --region eu-central-1 \
  --name "gitops-platform-demo-restore-$(date -u +%Y%m%d-%H%M%S)" \
  --architecture x86_64 --virtualization-type hvm \
  --root-device-name /dev/sda1 \
  --ena-support --sriov-net-support simple \
  --boot-mode uefi-preferred --imds-support v2.0 \
  --block-device-mappings \
    'DeviceName=/dev/sda1,Ebs={SnapshotId=snap-0807cef72a853334b,VolumeSize=40,VolumeType=gp3,DeleteOnTermination=false}'
```

`DeleteOnTermination=false` is deliberate: terminating the instance must not take
the platform's state with it. The cost is that a terminate leaves a 40 GB volume
behind at about $3.50/month, so **snapshot it and delete it** rather than letting
it sit unnamed — a pile of unidentifiable orphan volumes is exactly what the
2026-09-08 cleanup had to untangle.

An AMI registered this way was created on 2026-09-27: **`ami-0d8c00e6ced01969b`**.
Reuse it rather than registering another.

### 2. Launch

```bash
aws ec2 run-instances --region eu-central-1 \
  --image-id ami-0d8c00e6ced01969b \
  --instance-type m7i.xlarge \
  --subnet-id "$(aws ec2 describe-subnets --region eu-central-1 \
      --filters Name=availability-zone,Values=eu-central-1b Name=default-for-az,Values=true \
      --query 'Subnets[0].SubnetId' --output text)" \
  --security-group-ids sg-08dab2da789bdaefc \
  --key-name abdurahman-t3code \
  --associate-public-ip-address \
  --metadata-options 'HttpEndpoint=enabled,HttpTokens=required' \
  --tag-specifications \
    'ResourceType=instance,Tags=[{Key=Name,Value=gitops-platform-demo},{Key=CostCenter,Value=gitops-platform},{Key=Project,Value=gitops-platform-demo}]' \
    'ResourceType=volume,Tags=[{Key=Name,Value=gitops-platform-demo-root},{Key=CostCenter,Value=gitops-platform},{Key=Project,Value=gitops-platform-demo}]' \
  --query 'Instances[0].InstanceId' --output text
```

**`CostCenter=gitops-platform` is not decoration.** The $40 budget filters on that
tag, and `tools/demo-host.sh status` reports month-to-date spend through the same
filter. An untagged instance is invisible to both, which means the cap silently
stops capping.

**Billing starts the moment it launches**, at $5.80 a day. Stop it as soon as the
restore is verified.

### 3. Reconnect the switch

```bash
tools/reconnect-power.sh <new-instance-id>
```

One command, because the id lives in four places that must agree: the IAM policy
(the only thing that actually gates the API call), the Worker's `INSTANCE_ID`,
the budget's stop action, and `tools/demo-host.sh`. The script updates all four,
proves the IAM change with `simulate-principal-policy`, and redeploys the Worker.
Deploying does not disturb the two Worker secrets.

Then commit the three files it changed.

### 4. Fix what the disk remembers about the old machine

**This is the step that is easy to miss, and it presents as a 502.** k3s stores
its state on the restored disk, and that state describes the *previous* machine.
The instance comes up, k3s runs, `cloudflared` connects to the tunnel — and every
hostname still 502s, because Caddy's upstreams were never scheduled.

Three things need correcting, in this order. All of them were hit on 2026-09-27.

**a. Re-apply the node label.** Nine manifests carry
`nodeSelector: workload=platform`. That label was on the old node object, and a
rebuilt host registers as a *new* node without it — its private IP changes, so
k3s picks a new node name. ArgoCD, Grafana, KEDA, Prometheus and the cost
exporter all sit `Pending` with
`didn't match Pod's node affinity/selector`, while `caddy` and `cloudflared`
(which have no selector) run happily and make the cluster look healthy.

```bash
sudo k3s kubectl label node "$(hostname)" workload=platform --overwrite
```

**b. Delete the old node object.** `kubectl get nodes` shows two control-planes,
the old one `NotReady` and 31 days old. Its pods sit in `Terminating` forever —
nothing is left to confirm the delete — and a `Terminating` StatefulSet pod
blocks its replacement, because StatefulSet identities are unique. That is what
keeps `argocd-application-controller-0` and Prometheus down.

```bash
sudo k3s kubectl delete node <old-node-name>
sudo k3s kubectl get pods -A --no-headers | awk '$4=="Terminating"{print $1, $2}' \
  | while read ns p; do sudo k3s kubectl -n "$ns" delete pod "$p" --force --grace-period=0; done
```

`--force` is normally dangerous on a StatefulSet pod because two copies could run
at once. It is safe *here*, and only here, because the node those pods belonged
to is a terminated EC2 instance: nothing is still running them.

**c. Re-adopt the Prometheus volume.** local-path pins each PV to a node with
`nodeAffinity`, and that field is **immutable** — you cannot simply repoint it:

```
field is immutable, except for updating from beta label to GA
```

The data is still on the disk at `/var/lib/rancher/k3s/storage/<pvc>_.../`. The
reclaim policy is `Delete`, so **set it to `Retain` before deleting anything**,
or the claim takes the directory with it. Then delete the PVC and PV, create a
replacement PV with the same `local.path`, the live node's affinity, and a
`claimRef` naming the PVC the StatefulSet will recreate — the `claimRef`
pre-binds it so the dynamic provisioner cannot race in with an empty volume.

Expect the metrics themselves to be gone anyway: Prometheus enforces its
retention window at startup, and a host that has been off for weeks is past it.
111 MB became 78 MB of WAL on the 2026-09-27 restore. The point of preserving the
volume is a clean start, not the history.

### 5. Check it came back

```bash
make demo-host-status
```

k3s starts on boot, but see step 4 — "up" is not the same as "scheduled". The
check that actually means something is the origin answering, and every ArgoCD
application being healthy:

```bash
sudo k3s kubectl get pods -A | grep -v Running        # expect nothing interesting
sudo k3s kubectl -n argocd get applications           # expect Synced + Healthy
for h in gitops argocd grafana; do
  curl -s -o /dev/null -w "$h %{http_code}\n" \
    -H "Host: $h.abdurahman.ly" "http://$(sudo k3s kubectl -n edge get svc caddy \
      -o jsonpath='{.spec.clusterIP}'):8080/"
done                                                  # expect 200 200 302
```

Grafana answering 302 is correct — it redirects to its login page. The public
address changes on every start, so reviewers need
`make demo-host-allow IP=<address>` again.

### 6. Stop it

```bash
make demo-host-stop
```

The host is meant to be off. Three guards exist — the 90-minute idle auto-stop on
the machine, the budget's stop action at the cap, and this command — and the
order matters: the first two are safety nets for when someone forgets the third.
