# Demonstration runbook

A 12-minute walkthrough for someone who will never read the code. Everything below is a
web page, a dashboard, or a terminal printing plain English — no source files are opened
at any point.

The order is the platform's own argument, in sequence: **govern a change before it runs,
then measure it while it runs.** Resist showing modules in the order they were built; show
them in the order the story needs.

---

## Before they arrive

```bash
make doks-up          # or ensure the local cluster is healthy
make gitops-bootstrap
make doks-forward     # leave running in its own terminal
```

Open these four tabs, in this order, and leave them open:

1. GitHub → **pull request #1** (open, blocked)
2. GitHub → **pull request #3** (merged, passed)
3. **ArgoCD** → http://localhost:8080
4. **Grafana** → the *Cost- and Risk-Aware GitOps Platform* dashboard

Have one terminal ready, large font, in the repository root.

Do a full dry run the day before. Not for the demo's sake — to find out which parts are
slow, so you know when to keep talking.

---

## Act 1 — the problem (45 seconds, no screen)

Say this before showing anything:

> A developer changes one line in a deployment file — replicas from 1 to 8 — because a
> load test was slow. Nobody notices. It passes review, because reviewers read code, not
> capacity. The cluster grows eightfold. The finance team finds out five weeks later, on
> an invoice, with no way to trace which change caused it.
>
> Every part of that is normal, and every part of it is preventable.

Then: *"This platform makes that change impossible to merge, and here is what it looks
like when someone tries."*

---

## Act 2 — govern before it runs (3 minutes)

**Show: GitHub pull request #1.**

This is a real pull request, still open, still blocked. Scroll to the gate's comment.

Point at three things, in this order:

| What | Say |
|---|---|
| ❌ Gate failed — merge blocked | "The merge button is disabled. Not a warning — the change cannot land." |
| The cost table: $11.73 → $370.62, **+3060%** | "It priced the change *before* it ran. Nothing was deployed to learn this." |
| GATE-005, missing liveness probe | "And it checks safety, not only money. This container has no health check, so Kubernetes could never restart it if it hung." |

The line worth landing:

> Cost review normally happens monthly, in a spreadsheet, after the money is spent. Here
> it happens at code review, before anything is deployed, in the place the decision is
> actually made.

If asked "who decides the thresholds?" — they are in a version-controlled file, changed by
pull request, and that change goes through the same gate.

**Then show pull request #3** (merged, all green): the same machinery approving a change.
Thirty seconds. It matters — otherwise the gate looks like something that only ever says
no.

---

## Act 3 — Git governs the cluster (2 minutes)

**Show: the ArgoCD UI.**

Let the application tree render. It is genuinely nice to look at, and it does the
explaining for you: nine applications, all green, each tracing back to a folder in Git.

> Nothing here was deployed by hand. Every one of these came from a merged pull request.
> Nobody on this project has run a deploy command.

**Then the drift test.** In the terminal:

```bash
make drift-test
```

Narrate while it runs:

> I'm editing the live cluster directly — the way an engineer would at 3am during an
> incident. Watch what happens.

It reverts in about five seconds, unattended.

> That is the difference between Git being where the configuration is *kept* and Git being
> what the cluster *is*. Manual changes don't survive here.

---

## Act 4 — measure while it runs (2 minutes)

**Show: the Grafana dashboard.**

Three panels, in this order:

1. **Reserved vs actual spend** — two lines. "The top line is what we're paying for. The
   bottom is what we're using. The gap is waste."
2. **Monthly waste** — a single number. "That is money spent on capacity nobody used."
3. **Utilisation** — a percentage. "Low isn't automatically bad. It's headroom we chose to
   buy — but now it's a decision instead of an accident."

The point to land:

> The cost you saw quoted on the pull request, and the cost on this dashboard, are computed
> from the same rate table by the same code. So the estimate can be *checked*, not just
> trusted.

---

## Act 5 — demand drives cost (3 minutes, the centrepiece)

Terminal:

```bash
make load-test
```

Immediately switch to Grafana, the **Demand → replicas → spend** panel. Talk over it:

> I've just put 600 jobs on the queue. Nothing has been reconfigured — the system is
> reacting on its own.

Then narrate the three lines as they move, roughly fifteen seconds apart:

1. Queue depth jumps to ~600 — "demand arrives"
2. Replicas climb 1 → 6 — "the platform responds"
3. The cost line follows upward — "and here is what that response costs, live"

Then the backlog drains and, a minute or two later, replicas fall back to 1 and cost
returns to its floor.

The closing line for this act:

> Autoscaling demos usually stop at the middle line — replicas went up, therefore it works.
> That only proves something happened. The queue draining proves it *helped*, and the cost
> line proves what it cost. Those three on one axis is the whole point of joining these
> three projects into one.

If they ask only one question, it will probably be *"what stops it scaling to a thousand?"*
— the answer is that the gate prices an autoscaler at its **ceiling**, so authorising a
maximum is itself a reviewed, costed decision.

---

## Act 6 — none of this is a slide (1 minute)

```bash
make verify
```

Five checks, each printing PASS:

| Check | Proves |
|---|---|
| CNI enforcement | network isolation is *enforced*, not merely configured |
| Drift reversion | Git governs the cluster |
| Cost reconciliation | the pre-merge estimate matches live measurement |
| Connectivity matrix | permitted paths work, everything else is denied |
| Load test | demand drives replicas, and the backlog actually clears |

> Every claim I've made in the last ten minutes is a command in this repository that fails
> loudly if the claim stops being true.

---

## Volunteer the limitations

Do this before they ask. It reads as confidence, and examiners reward it.

- **The cost figures are modelled, not billed.** Published cloud rates applied to a local
  cluster. What's demonstrated is that prediction and measurement agree — not that the
  money is real.
- **The gate can only price what it renders from Git.** Components installed from Helm
  charts show as $0.00. That gap is visible in the reconciliation output rather than hidden.
- **Network isolation is layer 3/4.** A service mesh with mTLS was considered and
  deliberately scoped out: it injects a proxy into every pod, which would distort the cost
  measurements this project exists to make.

---

## If something breaks

- **Cluster slow or unresponsive** — this is why the demo runs on a managed cluster. If it
  happens anyway: keep talking, and fall back to the GitHub pull requests, which are static
  and always work.
- **Load test doesn't scale** — check `kubectl -n demo get hpa`. If the external metric
  reads `<unknown>`, KEDA cannot reach Redis; the pull requests and dashboard still tell
  the story.
- **Grafana empty** — the dashboard needs a few minutes of history after a fresh cluster.
  Bring the cluster up well before, not on the hour.

The safest possible fallback: **pull request #1 alone makes the argument.** If everything
else fails, that one page still shows a change being blocked with a costed, explained
reason.
