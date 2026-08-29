# Shared helpers for every rule in the policy sub-gate.
#
# Each rule file contributes objects to `violation`. The gate evaluates this package once
# per rendered manifest, passing that manifest as `input.manifest` and the whole rendered
# set as `input.peers`.
#
# The peer set matters: some properties are only decidable across objects, whether a
# Service actually routes to a Deployment, whether a workload is covered by a
# NetworkPolicy, and a rule that can see only one manifest has to guess. A guessing rule
# fires on correct manifests, and a gate that cries wolf is a gate that gets switched off.
#
# (The object under evaluation is called `manifest` rather than `object` because `object`
# is Rego's builtin namespace, and a rule of that name would shadow `object.get`.)
package gate

manifest := input.manifest

peers := object.get(input, "peers", [])

# pod_spec normalises where the pod template lives across workload kinds, so no rule has
# to care whether it is looking at a Deployment or a CronJob. Kinds not listed here leave
# pod_spec undefined, which makes every container rule below simply not apply.
pod_spec := manifest.spec.template.spec if {
	manifest.kind in {"Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job"}
}

pod_spec := manifest.spec if {
	manifest.kind == "Pod"
}

pod_spec := manifest.spec.jobTemplate.spec.template.spec if {
	manifest.kind == "CronJob"
}

# The labels a Service would select on.
pod_labels := object.get(object.get(manifest.spec.template, "metadata", {}), "labels", {}) if {
	manifest.kind in {"Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job"}
}

pod_labels := object.get(manifest.metadata, "labels", {}) if {
	manifest.kind == "Pod"
}

# Long-running containers: the ones whose configuration governs the workload in steady
# state. Native sidecars (init containers with restartPolicy: Always) belong here too,
# because they run for the pod's entire lifetime.
containers contains c if {
	some c in pod_spec.containers
}

containers contains c if {
	some c in pod_spec.initContainers
	c.restartPolicy == "Always"
}

# Ordinary init containers run to completion before the pod starts. Some rules apply to
# them (privilege, image provenance) and some do not (probes are meaningless on a
# container that is expected to exit).
init_containers contains c if {
	some c in pod_spec.initContainers
	not c.restartPolicy == "Always"
}

all_containers contains c if {
	some c in containers
}

all_containers contains c if {
	some c in init_containers
}

# workload_ref is used in messages so a violation points at something a reader can find.
workload_ref := sprintf("%s/%s", [manifest.kind, manifest.metadata.name])

# selects(service) is true when a Service's selector matches this workload's pod labels.
#
# A Kubernetes Service selector is a subset match: every key/value in the selector must be
# present on the pod, and the pod may carry additional labels. An equality check would
# miss every real Service, since pods always carry more labels than a selector names.
selects(service) if {
	service.kind == "Service"
	object.get(service.metadata, "namespace", "") == object.get(manifest.metadata, "namespace", "")
	selector := object.get(service.spec, "selector", {})
	count(selector) > 0
	every key, value in selector {
		pod_labels[key] == value
	}
}

# True when some Service in the rendered set routes traffic to this workload.
receives_traffic if {
	some service in peers
	selects(service)
}
