module github.com/AbdurahmanAlmehdi/gitops-platform/exporter

go 1.24

require (
	github.com/AbdurahmanAlmehdi/gitops-platform/pricing v0.0.0
	github.com/prometheus/client_golang v1.23.2
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.66.1 // indirect
	github.com/prometheus/procfs v0.16.1 // indirect
	go.yaml.in/yaml/v2 v2.4.2 // indirect
	golang.org/x/sys v0.35.0 // indirect
	google.golang.org/protobuf v1.36.8 // indirect
	sigs.k8s.io/yaml v1.6.0 // indirect
)

// M4 prices live usage with the exact code M2 prices manifests with. A second
// implementation reading the same YAML would drift, and a drifted rate makes the
// pre-merge estimate and the live measurement quietly incomparable — which would
// dissolve the one claim this platform is built to demonstrate.
replace github.com/AbdurahmanAlmehdi/gitops-platform/pricing => ../pricing
