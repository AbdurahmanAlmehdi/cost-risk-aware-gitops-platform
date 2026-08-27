package main

// The two demonstrations the dashboard can drive, and the status it reports between them.
//
// Each one mirrors a script in tools/ — drift-test.sh and load-test.sh — and deliberately
// keeps that script's *assertions*, not only its narration. A dashboard that reported
// "scaled up" without checking the backlog drained would be a worse demonstration than the
// terminal it replaces, because it would look more authoritative while proving less.
//
// Nothing here takes a command, a path, or a shell from the caller. The load size and the
// drifted value are constants below: the reviewer chooses *whether* a demonstration runs,
// never what it does. That is what keeps an authenticated web button from being a remote
// execution endpoint (see docs/LLD.md §9.4 and the handoff's note on the refused terminal).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const (
	demoNS        = "demo"
	apiDeployment = "demo-api"
	apiContainer  = "api"
	workerDeploy  = "demo-worker"

	// Fixed load, matching docs/DEMO.md act 5. Not caller-supplied: the job count is the
	// one parameter that drives spending, so it is not something an HTTP request may set.
	loadJobs     = 600
	loadJobMS    = 400
	scaleTimeout = 180 * time.Second
	drainTimeout = 420 * time.Second

	// The drift test edits memory, never replicas: the platform excludes .spec.replicas
	// from diffing so KEDA can scale workers, so drifting replicas would prove the
	// opposite of the point. Same reasoning as tools/drift-test.sh.
	driftedMemory = "999Mi"
	driftTimeout  = 180 * time.Second

	pollInterval = 5 * time.Second
)

// event is one line of a running demonstration, streamed to the browser as it happens.
type event struct {
	Phase   string         `json:"phase"`
	Message string         `json:"message"`
	Level   string         `json:"level"` // info | pass | fail
	Metrics map[string]any `json:"metrics,omitempty"`
}

type emitFunc func(event)

func info(e emitFunc, phase, format string, args ...any) {
	e(event{Phase: phase, Level: "info", Message: fmt.Sprintf(format, args...)})
}

// -----------------------------------------------------------------------------
// Kubernetes reads and the single patch
// -----------------------------------------------------------------------------

type deployment struct {
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Name      string `json:"name"`
					Resources struct {
						Limits map[string]string `json:"limits"`
					} `json:"resources"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		Replicas      int `json:"replicas"`
		ReadyReplicas int `json:"readyReplicas"`
	} `json:"status"`
}

func (k *kube) deployment(ctx context.Context, ns, name string) (*deployment, error) {
	var d deployment
	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", ns, name)
	if err := k.get(ctx, path, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// memoryLimit returns the container's declared memory limit, which is the field the drift
// test moves and the platform must move back.
func (d *deployment) memoryLimit(container string) (string, int, bool) {
	for i, c := range d.Spec.Template.Spec.Containers {
		if c.Name == container {
			v, ok := c.Resources.Limits["memory"]
			return v, i, ok
		}
	}
	return "", 0, false
}

// patchMemoryLimit is the only write this service makes to the cluster, and the RBAC it
// runs under permits no other. See manifests/apps/demo-dashboard/rbac.yaml.
func (k *kube) patchMemoryLimit(ctx context.Context, ns, name string, index int, value string) error {
	patch := []map[string]any{{
		"op":    "replace",
		"path":  fmt.Sprintf("/spec/template/spec/containers/%d/resources/limits/memory", index),
		"value": value,
	}}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", ns, name)
	_, err = k.do(ctx, http.MethodPatch, path, "application/json-patch+json", body)
	return err
}

type scaledObject struct {
	Spec struct {
		MinReplicaCount *int `json:"minReplicaCount"`
		MaxReplicaCount *int `json:"maxReplicaCount"`
	} `json:"spec"`
}

// bounds reports the replica range KEDA is authorised to move within. The load test
// asserts against these rather than hard-coding them, so the assertion follows the
// manifest if the manifest changes (LLD §5.6).
func (k *kube) bounds(ctx context.Context, ns, name string) (minR, maxR int, err error) {
	var s scaledObject
	path := fmt.Sprintf("/apis/keda.sh/v1alpha1/namespaces/%s/scaledobjects/%s", ns, name)
	if err := k.get(ctx, path, &s); err != nil {
		return 0, 0, err
	}
	minR, maxR = 1, 0
	if s.Spec.MinReplicaCount != nil {
		minR = *s.Spec.MinReplicaCount
	}
	if s.Spec.MaxReplicaCount != nil {
		maxR = *s.Spec.MaxReplicaCount
	}
	if maxR == 0 {
		return 0, 0, fmt.Errorf("no ScaledObject bounds for %s/%s — is M5 deployed?", ns, name)
	}
	return minR, maxR, nil
}

type argoApp struct {
	Name   string `json:"name"`
	Sync   string `json:"sync"`
	Health string `json:"health"`
}

func (k *kube) argoApps(ctx context.Context) ([]argoApp, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Sync struct {
					Status string `json:"status"`
				} `json:"sync"`
				Health struct {
					Status string `json:"status"`
				} `json:"health"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := k.get(ctx, "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications", &list); err != nil {
		return nil, err
	}
	apps := make([]argoApp, 0, len(list.Items))
	for _, it := range list.Items {
		apps = append(apps, argoApp{
			Name:   it.Metadata.Name,
			Sync:   it.Status.Sync.Status,
			Health: it.Status.Health.Status,
		})
	}
	return apps, nil
}

// -----------------------------------------------------------------------------
// The demo API — the queue's front door
// -----------------------------------------------------------------------------

type demoAPI struct {
	base string
	http *http.Client
}

func (a *demoAPI) queueDepth(ctx context.Context) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+"/api/queue", nil)
	if err != nil {
		return 0, err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("demo api: %s", resp.Status)
	}
	var body struct {
		QueueDepth int64 `json:"queue_depth"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, err
	}
	return body.QueueDepth, nil
}

func (a *demoAPI) enqueue(ctx context.Context, count, durationMS int) error {
	url := fmt.Sprintf("%s/api/jobs?count=%d&duration_ms=%d", a.base, count, durationMS)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("enqueue: %s", resp.Status)
	}
	return nil
}
