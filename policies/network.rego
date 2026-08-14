# Network policy rules — the "no overly permissive network settings" half of the gate.
#
# M6 enforces network isolation in the cluster, but the policies themselves are manifests
# in Git and pass through this same gate (LLD §9.4: the platform governs changes to itself
# the way it governs application changes). Without these rules, the one manifest that could
# dismantle the entire network baseline would be the one manifest nobody checks.
package gate

is_network_policy if {
	input.kind == "NetworkPolicy"
}

# An ingress rule with an empty `from` admits every pod in the cluster. It looks
# restrictive — it names ports, it selects pods — while allowing exactly what the
# default-deny baseline was put in place to prevent.
violation contains v if {
	is_network_policy
	some rule in input.spec.ingress
	not rule.from
	v := {
		"rule": "GATE-016",
		"severity": "block",
		"message": sprintf("%s has an ingress rule with no `from` selector, which allows traffic from every pod in the cluster", [workload_ref]),
		"remediation": "Add an explicit `from` with a podSelector or namespaceSelector. An ingress rule without `from` is an allow-all rule wearing the shape of a restriction.",
	}
}

# 0.0.0.0/0 in an ipBlock reaches past the cluster to the entire internet.
violation contains v if {
	is_network_policy
	some rule in input.spec.ingress
	some from in rule.from
	from.ipBlock.cidr == "0.0.0.0/0"
	v := {
		"rule": "GATE-017",
		"severity": "block",
		"message": sprintf("%s allows ingress from 0.0.0.0/0", [workload_ref]),
		"remediation": "Restrict the CIDR, or use a podSelector/namespaceSelector instead. If public ingress is genuinely required it belongs at an ingress controller, not in a pod-level allow-list.",
	}
}

# An empty podSelector applies the rule to every pod in the namespace. That is correct and
# intended for a default-deny policy, and dangerous for a policy that allows something.
violation contains v if {
	is_network_policy
	count(object.get(input.spec, "podSelector", {})) == 0
	count(object.get(input.spec, "ingress", [])) > 0
	v := {
		"rule": "GATE-018",
		"severity": "warn",
		"message": sprintf("%s applies an allow rule to every pod in its namespace (empty podSelector)", [workload_ref]),
		"remediation": "Scope the policy with a `podSelector` unless it is deliberately namespace-wide. An empty selector is right for default-deny and usually wrong for an allow-list.",
	}
}
