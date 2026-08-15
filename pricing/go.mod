// The platform's single pricing authority, deliberately its own module.
//
// Both the control plane (M2, pricing manifests before merge) and the data plane (M4,
// pricing live usage after deploy) depend on it, and neither depends on the other. A
// shared leaf module is what keeps that dependency graph honest: the alternative — the
// cost exporter importing the gate — would make a data-plane component depend on a
// control-plane one purely to borrow a rate table.
module github.com/AbdurahmanAlmehdi/gitops-platform/pricing

go 1.24

require sigs.k8s.io/yaml v1.6.0

require go.yaml.in/yaml/v2 v2.4.2 // indirect
