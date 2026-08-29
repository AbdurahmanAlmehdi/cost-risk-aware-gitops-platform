# Deploying the platform on DigitalOcean Kubernetes

The demonstration environment. Everything here is also true of the local `kind` cluster -
the platform is plain Kubernetes and Helm throughout, so the only environment-specific
part is provisioning the cluster itself.

## Why managed Kubernetes rather than a VPS

A single VPS running k3s is cheaper and works. Managed Kubernetes is chosen for one
specific reason, learned the hard way on the local cluster: **the control plane stops
competing with the workloads**.

On the laptop cluster, once the platform reached nine ArgoCD Applications, three of them
Helm-sourced, the API server climbed past two cores and began timing out requests for
everything else. Pods restarted, Applications went `Unknown`, and the failure looked like
several unrelated problems rather than one capacity problem. On DOKS the control plane is
DigitalOcean's, on their hardware, and none of that lands on the nodes running Prometheus
and the demo workloads.

The second reason is smaller but real: the node and load-balancer prices are published
rates, so `pricing.yaml` can model the provider the platform is genuinely running on
rather than a generic one.

## Cost

With the GitHub Student Developer Pack's $200 DigitalOcean credit:

| Item | Monthly |
|---|---|
| 3 × `s-2vcpu-4gb` nodes | ~$72 |
| Control plane | free |
| **Total** | **~$72/mo, about 2.7 months of credit** |

The cluster bills **by the hour**, so the honest way to use the credit is to create it for
the week of the demonstration and destroy it afterwards. A week costs roughly $17.

Sizing is taken from measurements rather than guesses: the platform's steady state is
~2.5 cores, and the load test briefly needs headroom for six worker replicas on top. Two
nodes was tried at laptop scale and the API server began timing out under reconciliation
load.

## Prerequisites

```bash
brew install doctl
doctl auth init          # paste a personal access token from the DO control panel
```

## Bring the cluster up

```bash
make doks-up
```

This creates two node pools. One tainted `workload=platform` for ArgoCD, Prometheus,
Grafana, KEDA and the cost exporter, and one labelled `workload=apps` for the demo
workloads. The taint is what lets M5 scale the worker tier without contending with the
components measuring it.

It then runs `verify-cni.sh` before declaring success. **DOKS ships Cilium, which does
enforce NetworkPolicy, but that is verified, not assumed.** If the check fails the script
stops and tells you to install Calico, because M6 on a non-enforcing CNI is a security
baseline that exists only as documentation.

Note what this script does *not* do: it installs no CNI. On kind, Calico is mandatory
because kindnet enforces nothing. Here the bundled dataplane already works, and replacing
a working one would add a component for no reason.

## Hand the cluster over to Git

```bash
make gitops-bootstrap
```

Identical to the local cluster. ArgoCD, the repository credential, and the root
Application. That it is the same command is the point: an environment needing its own
bootstrap procedure is not really being delivered by GitOps.

Watch it converge:

```bash
kubectl -n argocd get applications -w
```

## Reach the surfaces

```bash
make doks-forward
```

Grafana on `:3000`, ArgoCD on `:8080`, the demo API on `:8081`, the same ports the kind
cluster maps through the host, so every other tool works unchanged.

Port-forwarding rather than LoadBalancers is deliberate: three DO LoadBalancers cost more
per month than the nodes, and nothing here needs to be reachable by anyone but the
presenter.

### If a supervisor needs a URL without you present

Add a Cloudflare Tunnel, free, and it opens no inbound ports:

1. Create a tunnel in the Cloudflare Zero Trust dashboard and copy its token.
2. Run `cloudflared` in-cluster with that token as a Secret.
3. Route `grafana.<your-domain>` → `http://observability-grafana.observability.svc:80`.

Deliberately not automated here, because it needs a credential this repository must never
contain. Everything else in the platform is reproducible from Git; this one step is not.

## Prove it works

```bash
make verify
```

Runs, in order: CNI enforcement, drift reversion, the pre-merge/live cost reconciliation,
the connectivity matrix, and the load test. Each fails loudly rather than reporting a
qualified success. This sequence *is* the demonstration. It makes every claim the project
makes, and checks each one.

## Tear it down

```bash
make doks-down
```

Bills by the hour. Destroy it when the demonstration is over; `make doks-up` reproduces it
in about ten minutes.

## What differs from the local cluster

| | kind | DOKS |
|---|---|---|
| CNI | Calico, installed explicitly (kindnet enforces no policy) | Cilium, bundled, verified before use |
| Node access | `extraPortMappings` in the kind config | `make doks-forward` |
| Node subnet | `172.16.0.0/12` | `10.0.0.0/8` (VPC) |
| Storage | local-path | `do-block-storage` |
| Control plane | a container competing with your workloads | managed, free, elsewhere |

Only one of these reaches the manifests: the node subnet, in demo-api's NetworkPolicy.
That rule allows the RFC1918 ranges a cluster's nodes occupy, so the same manifest is
correct in both environments. The alternative failure mode is silent, on a cluster whose
subnet is not listed, the front door is simply dead and the load generator times out with
nothing to say why.
