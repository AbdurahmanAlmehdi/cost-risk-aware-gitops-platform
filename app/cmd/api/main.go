// Command api is the producer half of the demo workload (M1).
//
// It accepts jobs over HTTP, places them on the shared queue, and publishes the queue
// depth that M5 scales on. It deliberately does no heavy work itself: keeping the
// producer cheap and the consumer expensive is what makes the autoscaling demonstration
// legible, load arrives at a fixed-size front door and is absorbed by a tier that grows.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/AbdurahmanAlmehdi/gitops-platform/app/internal/metrics"
	"github.com/AbdurahmanAlmehdi/gitops-platform/app/internal/queue"
)

const (
	maxJobDurationMS     = 60_000
	defaultJobDurationMS = 250
)

type server struct {
	q   *queue.Queue
	log *slog.Logger
	// ready is flipped once the first successful queue probe completes and back on
	// sustained failure. Readiness is separate from liveness so that a Redis outage
	// removes the API from the Service (stop sending it traffic) without restarting
	// it (a restart would not fix a dependency that is down, and the crash-loop would
	// destroy the evidence of what happened).
	ready atomic.Bool
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	addr := envOr("LISTEN_ADDR", ":8080")
	redisAddr := envOr("REDIS_ADDR", "redis:6379")
	redisPassword := os.Getenv("REDIS_PASSWORD")

	q := queue.New(redisAddr, redisPassword)
	defer q.Close()

	srv := &server{q: q, log: log}

	mux := http.NewServeMux()
	mux.Handle("POST /api/jobs", srv.instrument("/api/jobs", srv.handleEnqueue))
	mux.Handle("GET /api/queue", srv.instrument("/api/queue", srv.handleQueueDepth))
	mux.Handle("GET /healthz", http.HandlerFunc(srv.handleLive))
	mux.Handle("GET /readyz", http.HandlerFunc(srv.handleReady))
	mux.Handle("GET /metrics", promhttp.Handler())

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go srv.publishQueueDepth(ctx, 2*time.Second)

	go func() {
		log.Info("api listening", "addr", addr, "redis", redisAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listen failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	// Drain in-flight requests before exiting. Without this a rolling update returns
	// 502s to the load generator, which would show up on the M7 dashboard as an
	// availability dip caused by the deployment rather than by the workload.
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", "error", err)
	}
}

// publishQueueDepth samples the shared backlog on an interval and exposes it as a gauge.
//
// The API owns this rather than the workers because every worker would report the same
// number, and Prometheus would then have to de-duplicate identical series from a set of
// pods that is itself changing size, precisely while M5 is scaling it.
func (s *server) publishQueueDepth(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var consecutiveFailures int
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		depth, err := s.q.Depth(probeCtx)
		cancel()

		if err != nil {
			metrics.QueueDepthScrapeErrors.Inc()
			consecutiveFailures++
			// One missed sample is a blip; a run of them means the dependency is gone.
			// Only then is readiness withdrawn, so a transient hiccup does not flap
			// the API in and out of the Service.
			if consecutiveFailures >= 3 && s.ready.Swap(false) {
				s.log.Error("queue unreachable, marking not ready", "error", err)
			}
		} else {
			consecutiveFailures = 0
			metrics.QueueDepth.Set(float64(depth))
			if !s.ready.Swap(true) {
				s.log.Info("queue reachable, marking ready")
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type enqueueRequest struct {
	DurationMS int `json:"duration_ms"`
	Count      int `json:"count"`
}

type enqueueResponse struct {
	Accepted   int    `json:"accepted"`
	DurationMS int    `json:"duration_ms"`
	QueueDepth int64  `json:"queue_depth"`
	Message    string `json:"message,omitempty"`
}

func (s *server) handleEnqueue(w http.ResponseWriter, r *http.Request) int {
	req := enqueueRequest{DurationMS: defaultJobDurationMS, Count: 1}

	// An empty body is a valid request for one default job. It keeps the load
	// generator's command line short, which matters during a live demo.
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			return writeJSON(w, http.StatusBadRequest, enqueueResponse{
				Message: fmt.Sprintf("invalid request body: %v", err),
			})
		}
	}
	// Query parameters override the body so load can be driven with a plain GET-style
	// URL from any HTTP tool.
	if v := r.URL.Query().Get("duration_ms"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return writeJSON(w, http.StatusBadRequest, enqueueResponse{Message: "duration_ms must be an integer"})
		}
		req.DurationMS = n
	}
	if v := r.URL.Query().Get("count"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return writeJSON(w, http.StatusBadRequest, enqueueResponse{Message: "count must be an integer"})
		}
		req.Count = n
	}

	if req.DurationMS < 0 || req.DurationMS > maxJobDurationMS {
		return writeJSON(w, http.StatusBadRequest, enqueueResponse{
			Message: fmt.Sprintf("duration_ms must be between 0 and %d", maxJobDurationMS),
		})
	}
	if req.Count < 1 || req.Count > 10_000 {
		return writeJSON(w, http.StatusBadRequest, enqueueResponse{
			Message: "count must be between 1 and 10000",
		})
	}

	now := time.Now()
	for i := range req.Count {
		job := queue.Job{
			ID:         fmt.Sprintf("%d-%d", now.UnixNano(), i),
			DurationMS: req.DurationMS,
			EnqueuedAt: now,
		}
		if err := s.q.Enqueue(r.Context(), job); err != nil {
			metrics.JobsEnqueued.WithLabelValues("error").Inc()
			s.log.Error("enqueue failed", "error", err, "accepted", i)
			// Report what was actually accepted rather than failing the whole batch:
			// the caller needs to know the real backlog it created.
			return writeJSON(w, http.StatusServiceUnavailable, enqueueResponse{
				Accepted: i, DurationMS: req.DurationMS, Message: "queue unavailable",
			})
		}
		metrics.JobsEnqueued.WithLabelValues("ok").Inc()
	}

	depth, err := s.q.Depth(r.Context())
	if err != nil {
		s.log.Warn("depth read failed after enqueue", "error", err)
	}
	return writeJSON(w, http.StatusAccepted, enqueueResponse{
		Accepted: req.Count, DurationMS: req.DurationMS, QueueDepth: depth,
	})
}

func (s *server) handleQueueDepth(w http.ResponseWriter, r *http.Request) int {
	depth, err := s.q.Depth(r.Context())
	if err != nil {
		return writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "queue unavailable"})
	}
	return writeJSON(w, http.StatusOK, map[string]any{"queue_depth": depth})
}

// handleLive answers "is this process wedged?". Nothing more. It must not consult
// Redis: a liveness probe that fails when a dependency is down turns one outage into a
// cluster-wide crash-loop (LLD §9.2, the control/data plane failure split applied at
// the pod level).
func (s *server) handleLive(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *server) handleReady(w http.ResponseWriter, _ *http.Request) {
	if !s.ready.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("queue unreachable"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

// instrument records latency and status for a handler that returns its status code.
func (s *server) instrument(path string, h func(http.ResponseWriter, *http.Request) int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		code := h(w, r)
		metrics.HTTPDuration.WithLabelValues(path).Observe(time.Since(start).Seconds())
		metrics.HTTPRequests.WithLabelValues(path, r.Method, strconv.Itoa(code)).Inc()
	})
}

func writeJSON(w http.ResponseWriter, code int, body any) int {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
	return code
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
