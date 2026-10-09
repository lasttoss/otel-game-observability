// Command gameserver is the instrumented game backend: a few endpoints, a trace for every request, and a
// /metrics endpoint for Prometheus. Run it against the collector in this repository:
//
//	docker compose up -d
//	OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317 go run ./cmd/gameserver
//
// Without the endpoint it still serves, and says so: a service that refuses to start because the
// telemetry is not there is a service that cannot be run on a laptop.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lasttoss/otel-game-observability"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

func main() {
	os.Exit(run())
}

func run() int {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serviceName := env("OTEL_SERVICE_NAME", "gameserver")
	// No schema URL: resource.Default() carries one, and Merge refuses to mix two. The attributes are
	// stable names, so the schema they belong to is not this service's business.
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		"",
		semconv.ServiceName(serviceName),
		semconv.ServiceVersion("1.0.0"),
		// The attribute rather than a semconv constant: the name is stable, the Go constant for it has
		// moved between versions, and a telemetry pipeline should not break on a library rename.
		attribute.String("deployment.environment.name", env("DEPLOYMENT_ENVIRONMENT", "local")),
	))
	if err != nil {
		logger.Error("resource", "error", err)
		return 2
	}

	// Traces go to the collector over OTLP: the application does not know where they are stored, which is
	// the property that makes the backend swappable.
	tracerProvider, err := newTracerProvider(ctx, res, logger)
	if err != nil {
		logger.Error("tracer provider", "error", err)
		return 2
	}
	defer func() {
		// The last spans are the interesting ones - a service that stops is a trace worth having - so the
		// shutdown gets its own context rather than the one that was just cancelled.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tracerProvider.Shutdown(shutdownCtx)
	}()

	// Metrics go to Prometheus, which pulls them: a game server's scrape can be the health check that
	// notices the process is up but not serving, and pull survives a pod that cannot reach the network.
	registry := prometheus.NewRegistry()
	metricExporter, err := otelprom.New(otelprom.WithRegisterer(registry))
	if err != nil {
		logger.Error("prometheus exporter", "error", err)
		return 2
	}
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(metricExporter),
	)
	defer func() { _ = meterProvider.Shutdown(context.Background()) }()

	instruments, err := gameserver.NewInstruments(meterProvider.Meter("github.com/lasttoss/otel-game-observability"))
	if err != nil {
		logger.Error("instruments", "error", err)
		return 2
	}

	server := gameserver.Server{
		Tracer:      tracerProvider.Tracer("github.com/lasttoss/otel-game-observability"),
		Instruments: instruments,
		Step:        newMatchStore().Step,
	}

	mux := server.Routes()
	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	address := env("ADDR", ":8080")
	httpServer := &http.Server{
		Addr:              address,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("serving", "address", address, "service", serviceName, "otel", env("OTEL_EXPORTER_OTLP_ENDPOINT", "none"))
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("listen", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("stopping")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown", "error", err)
		return 1
	}
	return 0
}

func newTracerProvider(ctx context.Context, res *resource.Resource, logger *slog.Logger) (*sdktrace.TracerProvider, error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		logger.Warn("OTEL_EXPORTER_OTLP_ENDPOINT is not set: traces are not being exported")
		return sdktrace.NewTracerProvider(sdktrace.WithResource(res)), nil
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(strings.TrimPrefix(endpoint, "http://")),
		otlptracegrpc.WithInsecure())
	if err != nil {
		return nil, err
	}
	return sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exporter,
			// A batch every second, because a trace of a game session is worth reading while the session
			// is still going on.
			sdktrace.WithBatchTimeout(time.Second)),
	), nil
}

// matchStore is the demo game: matches that a player joins and moves in. It is here to be replaced.
type matchStore struct {
	mu      sync.Mutex
	matches map[string]*match
}

type match struct {
	step    int
	players map[string]bool
}

func newMatchStore() *matchStore {
	return &matchStore{matches: map[string]*match{}}
}

const maxPlayersPerMatch = 10

func (s *matchStore) Step(_ context.Context, req gameserver.PlayRequest) (gameserver.PlayResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, exists := s.matches[req.MatchID]
	switch {
	case req.Action == "join":
		if !exists {
			current = &match{players: map[string]bool{}}
			s.matches[req.MatchID] = current
		}
		if len(current.players) >= maxPlayersPerMatch && !current.players[req.PlayerID] {
			// The client asked for something the match cannot do: a 400, not a 500.
			return gameserver.PlayResponse{}, gameserver.ErrBadRequest
		}
		current.players[req.PlayerID] = true
	case !exists:
		return gameserver.PlayResponse{}, gameserver.ErrBadRequest
	case !current.players[req.PlayerID]:
		return gameserver.PlayResponse{}, gameserver.ErrBadRequest
	default:
		current.step++
	}

	return gameserver.PlayResponse{
		MatchID:  req.MatchID,
		Step:     current.step,
		Players:  len(current.players),
		Accepted: true,
	}, nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
