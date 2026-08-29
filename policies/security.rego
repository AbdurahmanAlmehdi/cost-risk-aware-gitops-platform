# Workload privilege rules.
#
# These are the "no overly permissive privilege settings" half of the LLD's policy sub-gate.
# They pair with M6: M6 restricts what a workload may talk to, and these restrict what a
# workload may do on the node it lands on. Neither is sufficient alone, network isolation
# does not help if a container can escape to the host, and a locked-down container is still
# dangerous if it can reach every database in the cluster.
package gate

# runAsNonRoot may be set on the pod or on the container, with the container winning.
# Checking only one location is the most common way this rule gets silently bypassed.
runs_as_non_root(c) if {
	c.securityContext.runAsNonRoot == true
}

runs_as_non_root(c) if {
	pod_spec.securityContext.runAsNonRoot == true
	not c.securityContext.runAsNonRoot == false
}

violation contains v if {
	some c in all_containers
	not runs_as_non_root(c)
	v := {
		"rule": "GATE-008",
		"severity": "block",
		"message": sprintf("container %q may run as root", [c.name]),
		"remediation": "Set `securityContext.runAsNonRoot: true` on the pod or the container. A root container that is compromised can write to any host path it has mounted.",
	}
}

violation contains v if {
	some c in all_containers
	c.securityContext.privileged == true
	v := {
		"rule": "GATE-009",
		"severity": "block",
		"message": sprintf("container %q runs privileged", [c.name]),
		"remediation": "Remove `privileged: true`. A privileged container has effectively full access to the host and its isolation from other workloads is nominal.",
	}
}

violation contains v if {
	some c in all_containers
	not c.securityContext.allowPrivilegeEscalation == false
	v := {
		"rule": "GATE-010",
		"severity": "block",
		"message": sprintf("container %q does not disable privilege escalation", [c.name]),
		"remediation": "Set `securityContext.allowPrivilegeEscalation: false`. Without it a setuid binary inside the container can gain privileges the container was not granted.",
	}
}

violation contains v if {
	some c in all_containers
	not drops_all_capabilities(c)
	v := {
		"rule": "GATE-011",
		"severity": "block",
		"message": sprintf("container %q does not drop all Linux capabilities", [c.name]),
		"remediation": "Set `securityContext.capabilities.drop: [\"ALL\"]`, then add back only what the workload genuinely needs.",
	}
}

drops_all_capabilities(c) if {
	some dropped in c.securityContext.capabilities.drop
	upper(dropped) == "ALL"
}

violation contains v if {
	some c in all_containers
	not c.securityContext.readOnlyRootFilesystem == true
	v := {
		"rule": "GATE-012",
		"severity": "warn",
		"message": sprintf("container %q has a writable root filesystem", [c.name]),
		"remediation": "Set `securityContext.readOnlyRootFilesystem: true` and mount an `emptyDir` for any path that must be writable. This is a warning rather than a block because some third-party images cannot run without a writable root.",
	}
}

# Host namespaces dissolve the boundary between the pod and the node itself. A pod sharing
# the host network also bypasses NetworkPolicy entirely, which would silently void M6's
# guarantees for that workload.
violation contains v if {
	some namespace_field in ["hostNetwork", "hostPID", "hostIPC"]
	object.get(pod_spec, namespace_field, false) == true
	v := {
		"rule": "GATE-013",
		"severity": "block",
		"message": sprintf("%s shares the host's %s namespace", [workload_ref, namespace_field]),
		"remediation": "Remove the host namespace setting. A pod on the host network is not subject to NetworkPolicy, so it silently escapes the platform's network baseline.",
	}
}
