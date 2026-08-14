# Health probe rules.
#
# Probes are what make the cluster self-healing: without a liveness probe Kubernetes can
# only detect a process that has exited, not one that is running and wedged. A deadlocked
# container with no liveness probe stays in the Service, keeps its cost, and serves
# nothing — the exact failure the platform claims to recover from automatically.
package gate

violation contains v if {
	some c in containers
	not c.livenessProbe
	v := {
		"rule": "GATE-005",
		"severity": "block",
		"message": sprintf("container %q has no liveness probe", [c.name]),
		"remediation": "Add a `livenessProbe`. Without one, Kubernetes can only restart a container that has already crashed — a hung process is indistinguishable from a healthy one.",
	}
}

# Readiness is required only where something actually routes traffic to the workload.
# A queue consumer pulls its own work and is never in a Service's endpoints, so demanding
# a readiness probe from it would be a rule with no consumer — and rules that must be
# routinely ignored teach people to ignore rules.
#
# `receives_traffic` (see common.rego) answers this by looking for a Service in the
# rendered set whose selector actually matches this workload, rather than by inferring it
# from the presence of a container port. The port heuristic fires on any workload that
# exposes metrics, which is most of them.
violation contains v if {
	some c in containers
	not c.readinessProbe
	receives_traffic
	v := {
		"rule": "GATE-006",
		"severity": "block",
		"message": sprintf("container %q is routed to by a Service but has no readiness probe", [c.name]),
		"remediation": "Add a `readinessProbe`. Without one the pod joins its Service the moment the process starts, so traffic arrives before the container can serve it — which surfaces as errors during every rollout.",
	}
}

# A liveness probe that depends on a backing service turns one dependency outage into a
# cluster-wide restart storm: every pod fails its probe, every pod restarts, and the
# restarts add load to the dependency that was already struggling.
violation contains v if {
	some c in containers
	path := c.livenessProbe.httpGet.path
	readiness := c.readinessProbe.httpGet.path
	path == readiness
	v := {
		"rule": "GATE-007",
		"severity": "warn",
		"message": sprintf("container %q uses the same endpoint (%s) for liveness and readiness", [c.name, path]),
		"remediation": "Point them at different endpoints. Readiness may check dependencies; liveness must only report whether this process is wedged, or a dependency outage will restart every replica instead of just removing them from the Service.",
	}
}
