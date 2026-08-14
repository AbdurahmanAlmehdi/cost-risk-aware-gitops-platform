# Shared helpers for every rule in the policy sub-gate.
#
# Each rule file contributes objects to `violation`, and the gate evaluates this package
# once per rendered manifest with that manifest as `input`. Writing rules against a single
# object — rather than a whole manifest set — keeps each rule readable and lets a
# violation name exactly one resource.
package gate

# pod_spec normalises where the pod template lives across workload kinds, so no rule has
# to care whether it is looking at a Deployment or a CronJob. Kinds not listed here leave
# pod_spec undefined, which makes every container rule below simply not apply.
pod_spec := input.spec.template.spec if {
	input.kind in {"Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job"}
}

pod_spec := input.spec if {
	input.kind == "Pod"
}

pod_spec := input.spec.jobTemplate.spec.template.spec if {
	input.kind == "CronJob"
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
workload_ref := sprintf("%s/%s", [input.kind, input.metadata.name])
