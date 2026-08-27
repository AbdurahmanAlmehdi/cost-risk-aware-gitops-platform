package main

// The demonstration dashboard.
//
// docs/DEMO.md is a twelve-minute runbook that assumes a terminal, a large font, and
// someone who knows which command comes next. This serves the same argument to someone who
// has none of those things: the acts become buttons, and the proofs stream back in plain
// English as they happen.
//
// It is deliberately NOT a web terminal. A browser shell on a host holding a cluster-admin
// kubeconfig is a remote-execution endpoint wearing a friendly hat, and it was refused for
// that reason. What exists instead is three fixed actions — read status, run the drift
// proof, run the load proof — each a specific sequence of API calls with no caller-supplied
// command, path, or parameter. There is no shell path here even in principle, and the RBAC
// this runs under permits exactly one write to the cluster.

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

type server struct {
	kube *kube
	api  *demoAPI
	prom string
	log  *slog.Logger

	// One demonstration at a time. Two reviewers pressing buttons at once would interleave
	// two narratives into one stream and, worse, let a drift test race a load test over the
	// same deployment.
	mu      sync.Mutex
	running string
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	k, err := newKube()
	if err != nil {
		// The dashboard's whole purpose is to report on a cluster. Starting without a way
		// to reach one would serve a page of blanks that looks like a healthy platform
		// with nothing deployed.
		log.Error("cannot reach the Kubernetes API", "error", err)
		os.Exit(1)
	}

	srv := &server{
		kube: k,
		api: &demoAPI{
			base: envOr("DEMO_API_URL", "http://demo-api.demo.svc.cluster.local:8080"),
			http: &http.Client{Timeout: 10 * time.Second},
		},
		prom: envOr("PROMETHEUS_URL", "http://observability-kube-prometh-prometheus.observability.svc:9090"),
		log:  log,
	}

	content, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Error("cannot open embedded assets", "error", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServer(http.FS(content)))
	mux.HandleFunc("GET /api/status", srv.handleStatus)
	mux.HandleFunc("POST /api/demo/drift", srv.handleDemo("drift"))
	mux.HandleFunc("POST /api/demo/load", srv.handleDemo("load"))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	// Readiness is not tied to the cluster being healthy. This page is most useful
	// precisely when something is wrong, and taking it out of the Service the moment
	// Prometheus hiccups would remove the instrument that explains the hiccup. Unreachable
	// dependencies surface as "unknown" tiles instead, never as zeros.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	addr := envOr("LISTEN_ADDR", ":8080")
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		// No write timeout: a load demonstration streams for up to ten minutes, and a
		// write deadline would cut the narration off mid-proof.
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
	}()

	log.Info("dashboard listening", "addr", addr)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

// -----------------------------------------------------------------------------
// Status
// -----------------------------------------------------------------------------

// metric carries a value that may legitimately be unavailable. A tile reading "0" when the
// truth is "unknown" is the same failure M4's freshness metric exists to prevent, so the
// zero value here is absence, not zero.
type metric struct {
	Value *float64 `json:"value"`
	Error string   `json:"error,omitempty"`
}

func measured(v float64) metric { return metric{Value: &v} }
func unknown(err error) metric  { return metric{Error: err.Error()} }

type statusResponse struct {
	Apps      []argoApp `json:"apps"`
	AppsError string    `json:"apps_error,omitempty"`

	Replicas metric `json:"replicas"`
	MinR     metric `json:"min_replicas"`
	MaxR     metric `json:"max_replicas"`
	Queue    metric `json:"queue_depth"`

	Reserved    metric `json:"reserved_usd_per_month"`
	Actual      metric `json:"actual_usd_per_month"`
	Waste       metric `json:"waste_usd_per_month"`
	Utilisation metric `json:"utilisation_percent"`
	CostAgeSecs metric `json:"cost_age_seconds"`

	Running string `json:"running,omitempty"`
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	var out statusResponse

	if apps, err := s.kube.argoApps(ctx); err != nil {
		out.AppsError = err.Error()
	} else {
		out.Apps = apps
	}

	if dep, err := s.kube.deployment(ctx, demoNS, workerDeploy); err != nil {
		out.Replicas = unknown(err)
	} else {
		out.Replicas = measured(float64(dep.Status.Replicas))
	}

	if minR, maxR, err := s.kube.bounds(ctx, demoNS, workerDeploy); err != nil {
		out.MinR, out.MaxR = unknown(err), unknown(err)
	} else {
		out.MinR, out.MaxR = measured(float64(minR)), measured(float64(maxR))
	}

	if depth, err := s.api.queueDepth(ctx); err != nil {
		out.Queue = unknown(err)
	} else {
		out.Queue = measured(float64(depth))
	}

	// Reserved versus actual, from the same exporter and the same rate table the gate
	// prices with. The basis label is what makes this one query instead of a join.
	reserved, errR := prom(ctx, s.prom, `sum(gitops_platform_workload_cost_usd_per_month{basis="requested"})`)
	actual, errA := prom(ctx, s.prom, `sum(gitops_platform_workload_cost_usd_per_month{basis="actual"})`)
	if errR != nil {
		out.Reserved = unknown(errR)
	} else {
		out.Reserved = measured(reserved)
	}
	if errA != nil {
		out.Actual = unknown(errA)
	} else {
		out.Actual = measured(actual)
	}
	if errR == nil && errA == nil {
		out.Waste = measured(reserved - actual)
		if reserved > 0 {
			out.Utilisation = measured(actual / reserved * 100)
		} else {
			out.Utilisation = unknown(fmt.Errorf("nothing reserved"))
		}
	} else {
		out.Waste = unknown(fmt.Errorf("cost data incomplete"))
		out.Utilisation = unknown(fmt.Errorf("cost data incomplete"))
	}

	// How old the cost figures are. Without this a flat line reads as "this costs nothing"
	// rather than "the exporter stopped".
	if age, err := prom(ctx, s.prom, `time() - gitops_platform_cost_last_success_timestamp_seconds`); err != nil {
		out.CostAgeSecs = unknown(err)
	} else {
		out.CostAgeSecs = measured(age)
	}

	s.mu.Lock()
	out.Running = s.running
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

// -----------------------------------------------------------------------------
// Running a demonstration, streamed
// -----------------------------------------------------------------------------

func (s *server) handleDemo(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		s.mu.Lock()
		if s.running != "" {
			busy := s.running
			s.mu.Unlock()
			http.Error(w, fmt.Sprintf("the %s demonstration is already running", busy), http.StatusConflict)
			return
		}
		s.running = name
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			s.running = ""
			s.mu.Unlock()
		}()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		var mu sync.Mutex
		emit := func(e event) {
			mu.Lock()
			defer mu.Unlock()
			payload, err := json.Marshal(e)
			if err != nil {
				return
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		}

		// The demonstrations are long; the client going away should stop the work rather
		// than leave a load test polling into a closed socket.
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
		defer cancel()

		s.log.Info("demonstration started", "demo", name)

		var err error
		switch name {
		case "drift":
			err = s.runDrift(ctx, emit)
		case "load":
			err = s.runLoad(ctx, emit)
		default:
			err = fmt.Errorf("unknown demonstration %q", name)
		}

		if err != nil {
			// A failure is streamed, not swallowed. Every claim this platform makes is a
			// command that fails loudly when the claim stops being true, and that has to
			// remain true when the command is a button.
			s.log.Error("demonstration failed", "demo", name, "error", err)
			emit(event{Phase: name, Level: "fail", Message: err.Error()})
			return
		}
		s.log.Info("demonstration passed", "demo", name)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
