# Resource declaration rules.
#
# These are the rules that make the cost sub-gate meaningful. A container with no
# requests cannot be priced from its manifest, so without this rule the cheapest possible
# change — declaring nothing — would also be the one the cost gate waves through.
# Requests and cost governance are the same concern viewed twice.
package gate

violation contains v if {
	some c in containers
	not c.resources.requests.cpu
	v := {
		"rule": "GATE-001",
		"severity": "block",
		"message": sprintf("container %q declares no CPU request", [c.name]),
		"remediation": "Add `resources.requests.cpu`. Without it the scheduler cannot reserve capacity, the workload competes for CPU with everything else on the node, and the cost gate has to price it from an assumed default.",
	}
}

violation contains v if {
	some c in containers
	not c.resources.requests.memory
	v := {
		"rule": "GATE-002",
		"severity": "block",
		"message": sprintf("container %q declares no memory request", [c.name]),
		"remediation": "Add `resources.requests.memory`.",
	}
}

# Memory is limited but CPU deliberately is not.
#
# Exceeding a memory limit gets the container OOMKilled — there is no graceful
# degradation, so an unbounded container can take down its node. Exceeding a CPU limit
# only throttles, and a CPU limit set too low degrades latency in a way that is very hard
# to diagnose. So memory limits are required and CPU limits are left to the author.
violation contains v if {
	some c in containers
	not c.resources.limits.memory
	v := {
		"rule": "GATE-003",
		"severity": "block",
		"message": sprintf("container %q declares no memory limit", [c.name]),
		"remediation": "Add `resources.limits.memory`. Unlike CPU, memory is not compressible: a container without a memory limit can consume the whole node before the kubelet evicts anything.",
	}
}

# A request far below the limit means the scheduler reserves far less than the workload
# may actually use. The pod is billed and planned as small, then behaves as large — which
# is precisely the gap between predicted and live cost that this platform exists to expose.
violation contains v if {
	some c in containers
	request := units.parse_bytes(c.resources.requests.memory)
	limit := units.parse_bytes(c.resources.limits.memory)
	request * 4 < limit
	v := {
		"rule": "GATE-004",
		"severity": "warn",
		"message": sprintf("container %q requests %s of memory but may use up to %s — more than a 4× gap", [c.name, c.resources.requests.memory, c.resources.limits.memory]),
		"remediation": "Raise the request or lower the limit. A wide gap means the pre-merge cost estimate understates what this workload can actually consume.",
	}
}
