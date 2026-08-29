// Command worker is the consumer half of the demo workload (M1) and the workload M5
// scales (M5).
//
// It pulls jobs from the shared queue and burns CPU for the requested duration. This is
// the expensive tier: its replica count is what KEDA changes, its CPU consumption is
// what M4 prices, and the relationship between the two is the correlation M7 plots.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/AbdurahmanAlmehdi/gitops-platform/app/internal/metrics"
	"github.com/AbdurahmanAlmehdi/gitops-platform/app/internal/queue"
	"github.com/AbdurahmanAlmehdi/gitops-platform/app/internal/work"
)

// dequeueTimeout bounds each blocking pop. It must be comfortably shorter than the
// pod's terminationGracePeriodSeconds, otherwise a scale-down would sit in a blocking
// read until the kubelet SIGKILLs it, losing the job it was about to receive.
const dequeueTimeout = 5 * time.Second

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	redisAddr := envOr("REDIS_ADDR", "redis:6379")
	redisPassword := os.Getenv("REDIS_PASSWORD")
	metricsAddr := envOr("METRICS_ADDR", ":8080")

	// Concurrency is per-pod parallelism; replica count is cluster-wide parallelism.
	// Keeping it at 1 by default means throughput is a straight function of replicas,
	// so a scale event on the dashboard has an unambiguous effect. Raising it is the
	// knob for showing vertical vs. horizontal scaling as alternatives.
	concurrency := envIntOr("CONCURRENCY", 1, log)

	q := queue.New(redisAddr, redisPassword)
	defer q.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	metricsServer := &http.Server{
		Addr:              metricsAddr,
		Handler:           newMetricsMux(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics listener failed", "error", err)
		}
	}()

	log.Info("worker starting", "redis", redisAddr, "concurrency", concurrency)

	var wg sync.WaitGroup
	for i := range concurrency {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			consume(ctx, q, log.With("consumer", id))
		}(i)
	}

	<-ctx.Done()
	log.Info("shutdown signal received, finishing in-flight work")

	// Consumers observe ctx and return after their current job; waiting for them is
	// what makes a scale-down lossless. KEDA removing a replica must not drop a job.
	wg.Wait()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = metricsServer.Shutdown(shutdownCtx)
	log.Info("worker stopped")
}

func consume(ctx context.Context, q *queue.Queue, log *slog.Logger) {
	// Backoff applies only to genuine errors, never to an empty queue. Idling is the
	// normal steady state at minimum replicas and must not be penalised.
	const (
		backoffMin = 500 * time.Millisecond
		backoffMax = 15 * time.Second
	)
	backoff := backoffMin

	for {
		if ctx.Err() != nil {
			return
		}

		job, err := q.Dequeue(ctx, dequeueTimeout)
		switch {
		case errors.Is(err, queue.ErrEmpty):
			backoff = backoffMin
			continue
		case errors.Is(err, context.Canceled):
			return
		case err != nil:
			metrics.JobsProcessed.WithLabelValues("error").Inc()
			log.Error("dequeue failed", "error", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > backoffMax {
				backoff = backoffMax
			}
			continue
		}
		backoff = backoffMin

		// Queueing delay is measured here because the consumer is the only party that
		// knows when the wait actually ended. This is the series that degrades under
		// load and recovers after a scale-up, the proof that scaling helped.
		if !job.EnqueuedAt.IsZero() {
			metrics.JobWaitTime.Observe(time.Since(job.EnqueuedAt).Seconds())
		}

		start := time.Now()
		work.Do(ctx, time.Duration(job.DurationMS)*time.Millisecond)
		metrics.JobDuration.Observe(time.Since(start).Seconds())
		metrics.JobsProcessed.WithLabelValues("ok").Inc()
	}
}

func newMetricsMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.Handler())
	// The worker has no readiness concept. It pulls work rather than receiving it, so
	// it is never in a Service's endpoint list. Liveness alone is meaningful here, and
	// it deliberately does not consult Redis (see the API's handleLive for why).
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOr(key string, fallback int, log *slog.Logger) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		log.Warn("invalid integer env var, using default", "key", key, "value", v, "default", fallback)
		return fallback
	}
	return n
}
