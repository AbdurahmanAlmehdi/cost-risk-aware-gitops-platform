# Power switch for the review host

A page with two buttons: start the instance, stop the instance. Cloudflare Access decides
who may press them.

## Why this is not part of the platform

Everything else in this repository is delivered to the cluster by ArgoCD. This is not, and
cannot be. It exists to start the machine the cluster runs on, so it has to be running
when that machine is not. The demonstration dashboard at `gitops.abdurahman.ly` can stop
the host, but it can never start it, because stopping the host stops the dashboard too.

A Worker also removes a bottleneck that had nothing to do with the platform. The instance
was previously controlled through the AWS console, which needs a passkey held on one
laptop, so one person had to be present for anyone else to look at anything.

## Setting it up

Three steps, in order. The middle one produces a credential, which is why it is yours to
run rather than something the repository can do for you.

### 1. An AWS identity that can do almost nothing

Create a user whose entire permitted vocabulary is starting, stopping and describing one
instance. If these credentials leak, the worst available outcome is that somebody toggles
a demonstration server.

```bash
aws iam create-user --user-name gitops-platform-power \
  --tags Key=CostCenter,Value=gitops-platform Key=Project,Value=graduation-project

aws iam put-user-policy --user-name gitops-platform-power \
  --policy-name power-cycle-review-host \
  --policy-document '{
    "Version": "2012-10-17",
    "Statement": [
      {
        "Effect": "Allow",
        "Action": ["ec2:StartInstances", "ec2:StopInstances"],
        "Resource": "arn:aws:ec2:eu-central-1:240571106679:instance/i-054dedc804b0ca4e0"
      },
      {
        "Effect": "Allow",
        "Action": ["ec2:DescribeInstances"],
        "Resource": "*"
      }
    ]
  }'
```

`DescribeInstances` cannot be restricted to a single instance -- AWS does not support
resource-level permissions on it -- so it is granted broadly and reads nothing but state.

### 2. Deploy, then hand it the key

```bash
cd edge-control
npx wrangler deploy
npx wrangler secret put AWS_ACCESS_KEY_ID
npx wrangler secret put AWS_SECRET_ACCESS_KEY
```

Create the access key with `aws iam create-access-key --user-name gitops-platform-power`
and paste each value when prompted. `wrangler secret put` reads from the terminal and
sends the value straight to Cloudflare, so the key never enters a file, a commit, or a
shell history entry.

Until both secrets exist the page still loads and says which ones are missing, so a
half-finished deployment explains itself rather than failing blankly.

### 3. Put Access in front of it

**This step is not optional.** Without it the URL is a public button that starts and stops
a billable machine. Add a self-hosted Access application for the Worker's hostname and
point its policy at the existing `gitops-platform reviewers` group, so the same people who
can see the dashboards can also wake them, and removing someone removes both at once.

## What it looks like from a phone

State, address, and what it is costing right now. When the instance is stopped, only
Start is enabled; when it is running, only Stop. After either button the page polls until
the instance settles, so the state shown is the state that exists.

## Notes

The Worker signs its own AWS requests rather than importing an SDK, which keeps the whole
thing one file with no build step.

`wrangler tail` shows every state change with the address of whoever caused it. That is
the only audit trail for an action that costs money, which is why observability is on in
`wrangler.toml`.
