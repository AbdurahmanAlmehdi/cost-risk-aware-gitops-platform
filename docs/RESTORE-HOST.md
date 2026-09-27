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

### 4. Check it came back

```bash
make demo-host-status
```

k3s starts on boot, so ArgoCD, Grafana and the exporter should come up without
help. Give it a couple of minutes, then open the three hostnames. The public
address changes on every start, so reviewers need
`make demo-host-allow IP=<address>` again.

### 5. Stop it

```bash
make demo-host-stop
```

The host is meant to be off. Three guards exist — the 90-minute idle auto-stop on
the machine, the budget's stop action at the cap, and this command — and the
order matters: the first two are safety nets for when someone forgets the third.
