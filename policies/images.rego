# Image provenance rules.
#
# This is the rule that makes the LLD's central contract enforceable: "what M2 evaluates is
# byte-identical to what M3 deploys". A tag is a mutable pointer — the image behind
# `:v1.2.3` can be replaced after the gate has approved it, so everything the gate checked
# would describe an artefact that is no longer the one being deployed. Only a digest makes
# the approval refer to a specific set of bytes.
package gate

violation contains v if {
	some c in all_containers
	not contains(c.image, "@sha256:")
	v := {
		"rule": "GATE-014",
		"severity": "block",
		"message": sprintf("container %q uses image %q, which is not pinned to a digest", [c.name, c.image]),
		"remediation": "Reference the image as `repository@sha256:<digest>`. A tag can be repointed at different bytes after this gate has approved it, so a tagged deployment is not the artefact that was reviewed.",
	}
}

# Called out separately from the digest rule because it is the specific case people
# recognise, and a message naming `:latest` explains itself faster than a general one.
violation contains v if {
	some c in all_containers
	endswith(c.image, ":latest")
	v := {
		"rule": "GATE-015",
		"severity": "block",
		"message": sprintf("container %q uses the floating tag `:latest`", [c.name]),
		"remediation": "Pin to a digest. With `:latest`, two pods of the same Deployment created minutes apart can be running different code.",
	}
}
