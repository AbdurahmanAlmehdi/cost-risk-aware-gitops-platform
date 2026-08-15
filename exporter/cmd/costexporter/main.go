// Command costexporter is M4: live cost attribution.
//
// It reads usage from Prometheus, prices it with the platform's shared rate table, and
// re-exposes the result as Prometheus metrics. Prometheus then scrapes it, which closes
// the loop and lets M7 chart cost on the same time axis as the scaling events that caused
// it — the correlation the whole platform is built to make visible.
//
// It is strictly read-only (LLD §4.2). It observes and accounts; it never mutates a
// workload, and it holds no permission to.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/AbdurahmanAlmehdi/gitops-platform/pricing"

	"github.com/AbdurahmanAlmehdi/gitops-platform/exporter/internal/attribution"
	"github.com/AbdurahmanAlmehdi/gitops-platform/exporter/internal/promapi"
)

var (
	// The basis label is what makes reserved-versus-consumed a single query away in
	// Grafana. Splitting them into two metric names would force every panel that wants
	// the comparison to join two series by hand.
	costHourly = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gitops_platform_workload_cost_usd_per_hour",
		Help: "Modelled hourly cost of a workload, by basis (requested capacity vs actual usage).",
	}, []string{"namespace", "workload", "basis"})

	costMonthly = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gitops_platform_workload_cost_usd_per_month",
		Help: "Modelled monthly cost of a workload, by basis. Monthly is the unit budgets are set in.",
	}, []string{"namespace", "workload", "basis"})

	cpuCores = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gitops_platform_workload_cpu_cores",
		Help: "CPU attributed to a workload, by basis.",
	}, []string{"namespace", "workload", "basis"})

	memoryGiB = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gitops_platform_workload_memory_gib",
		Help: "Memory attributed to a workload, by basis.",
	}, []string{"namespace", "workload", "basis"})

	// Exposed so a dashboard can state which rates produced the figures it is drawing.
	// A cost chart whose rate table changed midway through the window is misleading in a
	// way nothing on the chart itself would reveal.
	pricingVersion = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gitops_platform_pricing_table_version",
		Help: "Version of the pricing table these figures were computed with.",
	}, []string{"currency", "source"})

	collectionErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gitops_platform_cost_collection_errors_total",
		Help: "Failed collection cycles. A gap in cost data is a gap, not a zero.",
	})

	lastSuccess = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "gitops_platform_cost_last_success_timestamp_seconds",
		Help: "When cost was last collected successfully. Lets a dashboard distinguish 'cost is zero' from 'cost is stale'.",
	})
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	promURL := envOr("PROMETHEUS_URL", "http://observability-kube-prometh-prometheus.observability.svc:9090")
	pricingPath := envOr("PRICING_PATH", "/etc/gitops-platform/pricing.yaml")
	rateWindow := envOr("RATE_WINDOW", "5m")
	interval := envDuration("COLLECT_INTERVAL", 30*time.Second, log)
	listenAddr := envOr("LISTEN_ADDR", ":9091")

	table, err := pricing.Load(pricingPath)
	if err != nil {
		// Without a rate table there is nothing to report. Failing loudly at startup is
		// far better than serving zeros that look like a workload costing nothing.
		log.Error("cannot load pricing table", "path", pricingPath, "error", err)
		os.Exit(1)
	}
	log.Info("pricing table loaded",
		"version", table.Metadata.Version, "currency", table.Spec.Currency, "source", table.Metadata.Source)

	registry := prometheus.NewRegistry()
	registry.MustRegister(costHourly, costMonthly, cpuCores, memoryGiB,
		pricingVersion, collectionErrors, lastSuccess)
	pricingVersion.WithLabelValues(table.Spec.Currency, table.Metadata.Source).
		Set(float64(table.Metadata.Version))

	collector := attribution.New(promapi.New(promURL, 10*time.Second), table, rateWindow)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	server := &http.Server{Addr: listenAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		log.Info("cost exporter listening", "addr", listenAddr, "prometheus", promURL, "interval", interval)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listener failed", "error", err)
			stop()
		}
	}()

	go collect(ctx, collector, interval, log)

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	log.Info("cost exporter stopped")
}

func collect(ctx context.Context, collector *attribution.Collector, interval time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		cycleCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		costs, err := collector.Collect(cycleCtx)
		cancel()

		if err != nil {
			collectionErrors.Inc()
			// A failed cycle leaves the previous gauge values in place rather than
			// zeroing them. Zeroing would draw a cost cliff on the dashboard that looks
			// exactly like a workload being deleted (LLD §4.5: a missed sample creates a
			// gap, not a crash — and never a false reading).
			log.Error("collection cycle failed", "error", err)
		} else {
			// Reset before repopulating so a workload that has been deleted stops being
			// reported. Without this its last known cost would persist forever and the
			// namespace total would keep counting something that no longer exists.
			costHourly.Reset()
			costMonthly.Reset()
			cpuCores.Reset()
			memoryGiB.Reset()

			for _, c := range costs {
				labels := prometheus.Labels{
					"namespace": c.Namespace,
					"workload":  c.Workload,
					"basis":     string(c.Basis),
				}
				costHourly.With(labels).Set(c.HourlyUSD)
				costMonthly.With(labels).Set(c.MonthlyUSD)
				cpuCores.With(labels).Set(c.CPUCores)
				memoryGiB.With(labels).Set(c.MemoryGiB)
			}
			lastSuccess.SetToCurrentTime()
			log.Info("collection cycle complete", "series", len(costs))
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration, log *slog.Logger) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		log.Warn("invalid duration, using default", "key", key, "value", v, "default", fallback)
		return fallback
	}
	return d
}
