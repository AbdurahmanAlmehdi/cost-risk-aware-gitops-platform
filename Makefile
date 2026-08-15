# Cost- and Risk-Aware GitOps Platform
#
# Every target is idempotent: re-running it converges rather than duplicating state.
# `make help` lists what is available.

SHELL := /bin/bash
.DEFAULT_GOAL := help

CLUSTER_NAME  := gitops-platform
KIND_CONFIG   := platform/kind/cluster.yaml

CALICO_VERSION := v3.32.1
KEDA_VERSION   := v2.20.2

# Pinned so a rebuild months from now reproduces the same platform. Floating versions
# would make the project unreproducible for anyone reviewing it later.
export CALICO_VERSION KEDA_VERSION

.PHONY: help
help: ## Show available targets
	@grep -hE '^[a-zA-Z0-9_.-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

# -----------------------------------------------------------------------------
# M0 — cluster substrate
# -----------------------------------------------------------------------------
.PHONY: cluster-up
cluster-up: ## Create the kind cluster (no CNI yet — nodes stay NotReady until `make cni`)
	@kind get clusters 2>/dev/null | grep -qx '$(CLUSTER_NAME)' \
		&& echo "cluster '$(CLUSTER_NAME)' already exists" \
		|| kind create cluster --config $(KIND_CONFIG)

.PHONY: cni
cni: ## Install Calico and wait for nodes to become Ready
	# As of Calico v3.32 the operator's CRDs ship in their own manifest and are no
	# longer bundled with the operator Deployment. They must be applied first —
	# applying the Installation CR before its CRD exists fails with a bare NotFound.
	kubectl apply --server-side --force-conflicts \
		-f https://raw.githubusercontent.com/projectcalico/calico/$(CALICO_VERSION)/manifests/operator-crds.yaml
	kubectl apply --server-side --force-conflicts \
		-f https://raw.githubusercontent.com/projectcalico/calico/$(CALICO_VERSION)/manifests/tigera-operator.yaml
	kubectl wait --for=condition=Established --timeout=120s \
		crd/installations.operator.tigera.io
	kubectl apply -f platform/network/calico-installation.yaml
	@echo "waiting for Calico to program the dataplane (this takes a couple of minutes)..."
	kubectl wait --for=condition=Ready node --all --timeout=300s
	@$(MAKE) --no-print-directory verify-cni

.PHONY: verify-cni
verify-cni: ## Assert the CNI actually enforces NetworkPolicy (not just accepts it)
	@bash tools/verify-cni.sh

.PHONY: cluster-down
cluster-down: ## Delete the kind cluster
	kind delete cluster --name $(CLUSTER_NAME)

.PHONY: cluster-status
cluster-status: ## Show node and system-pod state
	@kubectl get nodes -o wide
	@echo
	@kubectl get pods -A --field-selector=status.phase!=Running 2>/dev/null \
		| grep -v Completed || echo "all pods Running or Completed"

# -----------------------------------------------------------------------------
# M3 — GitOps delivery
# -----------------------------------------------------------------------------
ARGOCD_CHART_VERSION := 10.3.3
REPO_URL := https://github.com/AbdurahmanAlmehdi/cost-risk-aware-gitops-platform.git

.PHONY: argocd
argocd: ## Install ArgoCD onto the platform node
	helm repo add argo https://argoproj.github.io/argo-helm >/dev/null
	helm repo update argo >/dev/null
	helm upgrade --install argocd argo/argo-cd \
		--version $(ARGOCD_CHART_VERSION) \
		--namespace argocd --create-namespace \
		-f platform/argocd/values.yaml \
		--wait --timeout 10m
	@echo "ArgoCD UI: http://localhost:8080  (user: admin)"
	@echo "password:  make argocd-password"

.PHONY: argocd-repo-secret
argocd-repo-secret: ## Grant ArgoCD read access to this repository
	# The credential is read from the local gh CLI and written straight into the cluster.
	# It is never committed, never echoed, and never leaves this machine — the cluster is
	# local and disposable. Real deployments would use SOPS or a sealed-secrets
	# controller so the encrypted secret could itself live in Git.
	@kubectl create secret generic repo-gitops-platform \
		--namespace argocd \
		--from-literal=type=git \
		--from-literal=url=$(REPO_URL) \
		--from-literal=username=$$(gh api user --jq .login) \
		--from-literal=password=$$(gh auth token) \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	@kubectl label secret repo-gitops-platform --namespace argocd \
		argocd.argoproj.io/secret-type=repository --overwrite >/dev/null
	@echo "repository credential installed"

.PHONY: argocd-password
argocd-password: ## Print the initial ArgoCD admin password
	@kubectl -n argocd get secret argocd-initial-admin-secret \
		-o jsonpath='{.data.password}' | base64 -d; echo

.PHONY: gitops-bootstrap
gitops-bootstrap: argocd argocd-repo-secret ## Install ArgoCD and hand the cluster over to Git
	# The only manual apply in the platform. Everything after this comes from Git.
	kubectl apply -f manifests/argocd/root.yaml
	@echo
	@echo "The cluster now reconciles toward Git. Watch it with:"
	@echo "  kubectl -n argocd get applications -w"

.PHONY: drift-test
drift-test: ## Prove self-heal reverts a manual change to a live resource
	@bash tools/drift-test.sh

# -----------------------------------------------------------------------------
# M2 — pre-merge gate
# -----------------------------------------------------------------------------
.PHONY: gate-build
gate-build: ## Build the gate binary
	cd gate && go build -o ../bin/gate ./cmd/gate

.PHONY: gate-test
gate-test: ## Run the gate's golden-fixture tests
	cd gate && go test ./...

.PHONY: gate
gate: gate-build ## Run the gate locally against the working tree (BASE=<ref>)
	./bin/gate evaluate --config gate.yaml --base $${BASE:-origin/main}

.PHONY: cost
cost: gate-build ## Price the manifests as they stand, with no comparison
	./bin/gate price --config gate.yaml

# -----------------------------------------------------------------------------
# M4 / M7 — cost attribution and observability
# -----------------------------------------------------------------------------
.PHONY: reconcile
reconcile: gate-build ## Check M2's pre-merge estimate against M4's live measurement
	@bash tools/reconcile.sh

.PHONY: grafana-password
grafana-password: ## Print the Grafana admin password
	@kubectl -n observability get secret observability-grafana \
		-o jsonpath='{.data.admin-password}' | base64 -d; echo

# -----------------------------------------------------------------------------
# Aggregate
# -----------------------------------------------------------------------------
.PHONY: bootstrap
bootstrap: cluster-up cni ## Bring up a fully working empty platform
	@echo
	@echo "cluster is ready. next: make argocd"

.PHONY: test
test: gate-test ## Run every test in the repository
