// Command cx-metrics-gen pushes synthetic OTLP metrics to Coralogix so that
// recording rules can be developed and tested against data that actually moves.
//
// It runs as a long lived service, normally a single replica Deployment on
// Kubernetes. Metrics are pushed over OTLP/gRPC; there is no scrape endpoint.
// The HTTP listener exists only for liveness and readiness probes.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"cx-metrics-gen/internal/generator"
	"cx-metrics-gen/internal/telemetry"
)

// state is the small amount of runtime information the probes and the status
// endpoint report on.
type state struct {
	ready        atomic.Bool
	ticks        atomic.Int64
	apiEvents    atomic.Int64
	sloEvents    atomic.Int64
	lastTickUnix atomic.Int64
	startedUnix  int64
	tickSeconds  int
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	if err := run(); err != nil {
		log.Fatalf("fatal: %v", err)
	}
	log.Println("stopped cleanly")
}

func run() error {
	// SIGTERM is what Kubernetes sends when it wants the pod gone.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	telCfg, err := telemetry.ConfigFromEnv()
	if err != nil {
		return err
	}
	genCfg := generator.ConfigFromEnv()

	provider, err := telemetry.NewMeterProvider(ctx, telCfg)
	if err != nil {
		return err
	}

	gen, err := generator.New(provider, genCfg)
	if err != nil {
		return err
	}

	st := &state{startedUnix: time.Now().Unix(), tickSeconds: genCfg.TickSeconds}

	log.Printf("starting: endpoint=%s application=%s subsystem=%s instance=%q customers=%d active_per_tick=%d tick=%ds export_every=%s peak_series~%d",
		telCfg.Endpoint, telCfg.Application, telCfg.Subsystem, telCfg.InstanceID,
		genCfg.CustomerCount, genCfg.ActivePerTick, genCfg.TickSeconds,
		telCfg.ExportInterval, gen.ExpectedPeakSeries())

	srv := &http.Server{
		Addr:              getenv("HTTP_ADDR", ":8080"),
		Handler:           newMux(st),
		ReadHeaderTimeout: 5 * time.Second,
	}
	srvErr := make(chan error, 1)
	go func() {
		log.Printf("probe listener on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
			return
		}
		srvErr <- nil
	}()

	// First tick immediately, so a fresh pod produces data without waiting out
	// a full interval.
	tick(ctx, gen, st)
	st.ready.Store(true)

	ticker := time.NewTicker(time.Duration(genCfg.TickSeconds) * time.Second)
	defer ticker.Stop()

loop:
	for {
		select {
		case <-ctx.Done():
			log.Println("shutdown signal received")
			break loop
		case err := <-srvErr:
			if err != nil {
				return err
			}
			break loop
		case <-ticker.C:
			tick(ctx, gen, st)
		}
	}

	// Give the final export and the HTTP server a bounded window to finish.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
	// Shutdown flushes whatever the periodic reader has not exported yet. A
	// failure here means the last batch was lost, which is worth a loud log
	// line but is not a crash: the shutdown was asked for and it completed.
	// Returning an error would exit non-zero and make Kubernetes report a
	// perfectly normal pod termination as a failure.
	if err := provider.Shutdown(shutdownCtx); err != nil {
		log.Printf("warning: final metric flush failed, last batch lost: %v", err)
	}
	return nil
}

func tick(ctx context.Context, gen *generator.Generator, st *state) {
	now := time.Now()
	res := gen.Tick(ctx, now)

	st.ticks.Add(1)
	st.apiEvents.Add(res.APIEvents)
	st.sloEvents.Add(res.SLOEvents)
	st.lastTickUnix.Store(now.Unix())

	log.Printf("tick %d: %s", st.ticks.Load(), res)
}

func newMux(st *state) *http.ServeMux {
	mux := http.NewServeMux()

	// Liveness: the process is wedged if the tick loop has stalled well past
	// its interval. Restarting is the right response.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		last := st.lastTickUnix.Load()
		if last == 0 {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("starting\n"))
			return
		}
		stale := time.Duration(st.tickSeconds)*time.Second*3 + 30*time.Second
		if time.Since(time.Unix(last, 0)) > stale {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("tick loop stalled\n"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	// Readiness: true once the first tick has been recorded.
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !st.ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready\n"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ready":          st.ready.Load(),
			"ticks":          st.ticks.Load(),
			"api_events":     st.apiEvents.Load(),
			"slo_events":     st.sloEvents.Load(),
			"uptime_seconds": time.Now().Unix() - st.startedUnix,
			"last_tick":      time.Unix(st.lastTickUnix.Load(), 0).UTC().Format(time.RFC3339),
			"note":           "metrics are pushed over OTLP, this endpoint is not a scrape target",
		})
	})

	return mux
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
