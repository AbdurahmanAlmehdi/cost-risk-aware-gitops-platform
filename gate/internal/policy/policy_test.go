package policy_test

import (
	"context"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/AbdurahmanAlmehdi/gitops-platform/gate/internal/cost"
	"github.com/AbdurahmanAlmehdi/gitops-platform/gate/internal/policy"
)

// policyDir is the real rule set, not a fixture copy. These tests assert against the
// policies the platform actually enforces, a copy would drift and start passing while
// production rules were broken.
func policyDir(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "..", "policies")
}

func mustDecode(t *testing.T, raw string) cost.Object {
	t.Helper()
	var obj cost.Object
	if err := yaml.Unmarshal([]byte(raw), &obj); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return obj
}

// compliantWorker is the reference queue consumer: no readiness probe (nothing routes to
// it) but everything else in place.
const compliantWorker = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: demo-worker
  namespace: demo
spec:
  replicas: 1
  template:
    metadata:
      labels:
        app.kubernetes.io/name: demo-worker
    spec:
      containers:
        - name: worker
          image: ghcr.io/example/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
          ports:
            - name: http
              containerPort: 8080
          resources:
            requests:
              cpu: 500m
              memory: 64Mi
            limits:
              memory: 128Mi
          livenessProbe:
            httpGet:
              path: /healthz
              port: http
          securityContext:
            runAsNonRoot: true
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
`

func rulesFired(t *testing.T, objects []cost.Object) map[string]policy.Violation {
	t.Helper()
	engine, err := policy.New(context.Background(), policyDir(t))
	if err != nil {
		t.Fatalf("compile policies: %v", err)
	}
	result := engine.Evaluate(context.Background(), objects)
	if len(result.Errors) > 0 {
		t.Fatalf("policy evaluation errors: %v", result.Errors)
	}
	fired := make(map[string]policy.Violation, len(result.Violations))
	for _, v := range result.Violations {
		fired[v.Rule] = v
	}
	return fired
}

// TestCompliantWorkloadPasses is the guard against the most dangerous failure mode a
// policy engine has: rules that silently match nothing. A gate that approves everything
// looks identical to a gate that is working, right up until it lets something through.
func TestCompliantWorkloadPasses(t *testing.T) {
	fired := rulesFired(t, []cost.Object{mustDecode(t, compliantWorker)})
	for rule, v := range fired {
		if v.Severity == "block" {
			t.Errorf("compliant workload blocked by %s: %s", rule, v.Message)
		}
	}
}

func TestMissingLivenessProbeIsBlocked(t *testing.T) {
	obj := mustDecode(t, compliantWorker)
	spec := obj["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	container := spec["containers"].([]any)[0].(map[string]any)
	delete(container, "livenessProbe")

	fired := rulesFired(t, []cost.Object{obj})
	if _, ok := fired["GATE-005"]; !ok {
		t.Fatalf("GATE-005 did not fire for a container with no liveness probe; fired: %v", keys(fired))
	}
}

// TestReadinessRequiredOnlyWhenRouted is the rule that motivated giving policies access
// to the whole manifest set. The same workload must pass alone and fail once a Service
// selects it, a per-object rule cannot tell the two cases apart.
func TestReadinessRequiredOnlyWhenRouted(t *testing.T) {
	worker := mustDecode(t, compliantWorker)

	t.Run("no service routes to it", func(t *testing.T) {
		fired := rulesFired(t, []cost.Object{worker})
		if _, ok := fired["GATE-006"]; ok {
			t.Error("GATE-006 fired on a workload no Service selects")
		}
	})

	t.Run("a service routes to it", func(t *testing.T) {
		service := mustDecode(t, `
apiVersion: v1
kind: Service
metadata:
  name: demo-worker
  namespace: demo
spec:
  selector:
    app.kubernetes.io/name: demo-worker
  ports:
    - port: 8080
`)
		fired := rulesFired(t, []cost.Object{worker, service})
		if _, ok := fired["GATE-006"]; !ok {
			t.Errorf("GATE-006 did not fire on a workload a Service selects; fired: %v", keys(fired))
		}
	})
}

func TestPrivilegeViolationsAreBlocked(t *testing.T) {
	cases := map[string]func(container, pod map[string]any){
		"GATE-008": func(c, _ map[string]any) {
			delete(c["securityContext"].(map[string]any), "runAsNonRoot")
		},
		"GATE-009": func(c, _ map[string]any) {
			c["securityContext"].(map[string]any)["privileged"] = true
		},
		"GATE-010": func(c, _ map[string]any) {
			c["securityContext"].(map[string]any)["allowPrivilegeEscalation"] = true
		},
		"GATE-011": func(c, _ map[string]any) {
			delete(c["securityContext"].(map[string]any), "capabilities")
		},
		"GATE-013": func(_, pod map[string]any) {
			pod["hostNetwork"] = true
		},
	}

	for rule, mutate := range cases {
		t.Run(rule, func(t *testing.T) {
			obj := mustDecode(t, compliantWorker)
			pod := obj["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			container := pod["containers"].([]any)[0].(map[string]any)
			mutate(container, pod)

			fired := rulesFired(t, []cost.Object{obj})
			v, ok := fired[rule]
			if !ok {
				t.Fatalf("%s did not fire; fired: %v", rule, keys(fired))
			}
			if v.Severity != "block" {
				t.Errorf("%s severity = %q, want block", rule, v.Severity)
			}
			if v.Remediation == "" {
				t.Errorf("%s has no remediation; a violation that does not say what to do "+
					"instead makes the gate an obstacle rather than a guardrail", rule)
			}
		})
	}
}

func TestUnpinnedImageIsFlagged(t *testing.T) {
	obj := mustDecode(t, compliantWorker)
	pod := obj["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	pod["containers"].([]any)[0].(map[string]any)["image"] = "ghcr.io/example/app:latest"

	fired := rulesFired(t, []cost.Object{obj})
	for _, rule := range []string{"GATE-014", "GATE-015"} {
		if _, ok := fired[rule]; !ok {
			t.Errorf("%s did not fire for a :latest image; fired: %v", rule, keys(fired))
		}
	}
}

func TestPermissiveNetworkPolicyIsBlocked(t *testing.T) {
	obj := mustDecode(t, `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-everything
  namespace: demo
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: redis
  ingress:
    - ports:
        - port: 6379
`)
	fired := rulesFired(t, []cost.Object{obj})
	if _, ok := fired["GATE-016"]; !ok {
		t.Errorf("GATE-016 did not fire for an ingress rule with no `from`; fired: %v", keys(fired))
	}
}

func keys(m map[string]policy.Violation) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
