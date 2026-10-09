// Package gameserver is a game backend small enough to read in one sitting, and instrumented the way a
// real one has to be: every request is a span that says what it did, and every number an operator would
// page on is a metric with a name that survives a dashboard being deleted and rebuilt.
//
// The instrument is deliberately not optional. Telemetry added after the fact is telemetry that describes
// the code somebody guessed at; a span per step means the code has to say what a step is.
package gameserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Instruments are the metrics a game server is judged on. They are built once and passed in, so that a
// test can read them and a deployment can chart them.
type Instruments struct {
	// Plays counts match steps, by outcome: a rate of plays that stops falling is a server that has
	// stopped serving, and a rate of failures that rises is the other thing worth waking up for.
	Plays metric.Int64Counter
	// PlayDuration is the time one step took. A mean is not the number: the p99 is what a player feels.
	PlayDuration metric.Float64Histogram
	// PlayersInMatch is how full the matches are, which is the number capacity planning actually needs -
	// CPU is a proxy for it, and a poor one.
	PlayersInMatch metric.Int64Histogram
}

// NewInstruments registers the metrics on a meter.
func NewInstruments(meter metric.Meter) (Instruments, error) {
	plays, err := meter.Int64Counter("gameserver.plays",
		metric.WithDescription("match steps served"),
		metric.WithUnit("{step}"))
	if err != nil {
		return Instruments{}, fmt.Errorf("counter: %w", err)
	}
	duration, err := meter.Float64Histogram("gameserver.play.duration",
		metric.WithDescription("time to serve one match step"),
		metric.WithUnit("ms"),
		// The buckets are the point of a histogram: a p99 read out of buckets that stop at 100ms is a
		// p99 of "at least 100ms". These cover a tick budget of 33ms with room either side.
		metric.WithExplicitBucketBoundaries(1, 5, 10, 20, 33, 50, 100, 250, 500, 1000))
	if err != nil {
		return Instruments{}, fmt.Errorf("histogram: %w", err)
	}
	players, err := meter.Int64Histogram("gameserver.match.players",
		metric.WithDescription("players in the match a step was served for"),
		metric.WithUnit("{player}"))
	if err != nil {
		return Instruments{}, fmt.Errorf("players histogram: %w", err)
	}
	return Instruments{Plays: plays, PlayDuration: duration, PlayersInMatch: players}, nil
}

// Server is the HTTP surface.
type Server struct {
	Tracer      trace.Tracer
	Instruments Instruments
	// Step is the game logic for one step of a match.
	Step StepFunc
}

// StepFunc advances a match by one step. It is a function so that the transport and the game do not have
// to know about each other, and so a test can make a step fail on purpose.
type StepFunc func(ctx context.Context, req PlayRequest) (PlayResponse, error)

// PlayRequest is one step of one match by one player.
type PlayRequest struct {
	MatchID  string `json:"match_id"`
	PlayerID string `json:"player_id"`
	Action   string `json:"action"`
}

// PlayResponse is what the player's client is told.
type PlayResponse struct {
	MatchID  string `json:"match_id"`
	Step     int    `json:"step"`
	Players  int    `json:"players"`
	Accepted bool   `json:"accepted"`
}

// ErrBadRequest is what a handler returns for a payload the client got wrong, so that the status code is
// a 400 and the span records it as the client's fault rather than the server's.
var ErrBadRequest = errors.New("gameserver: bad request")

// Routes is the server as an http.Handler, which is what a test drives and what the binary serves.
func (s Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/play", s.handlePlay)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return mux
}

func (s Server) handlePlay(w http.ResponseWriter, r *http.Request) {
	ctx, span := s.Tracer.Start(r.Context(), "play",
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attribute.String("http.request.method", r.Method)))
	defer span.End()

	started := time.Now()
	var req PlayRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "the payload is not a play request")
		s.record(ctx, started, req, "invalid", 0, 0)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the payload is not a play request"})
		return
	}
	if req.MatchID == "" || req.PlayerID == "" {
		// The attributes are what make a trace findable: "which match was slow" is a question a trace
		// without ids cannot answer.
		span.SetStatus(codes.Error, "match_id and player_id are required")
		s.record(ctx, started, req, "invalid", 0, 0)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "match_id and player_id are required"})
		return
	}
	span.SetAttributes(attribute.String("game.match.id", req.MatchID),
		attribute.String("game.player.id", req.PlayerID),
		attribute.String("game.action", req.Action))

	response, err := s.Step(ctx, req)
	if err != nil {
		span.RecordError(err)
		outcome := "invalid"
		if !errors.Is(err, ErrBadRequest) {
			// A rejected step is the client's fault and is not an error the operator is paged about; a
			// failed step is.
			span.SetStatus(codes.Error, err.Error())
			outcome = "failed"
		}
		s.record(ctx, started, req, outcome, 0, 0)
		writeJSON(w, statusFor(err), map[string]string{"error": err.Error()})
		return
	}

	span.SetAttributes(attribute.Int("game.match.step", response.Step),
		attribute.Int("game.match.players", response.Players))
	s.record(ctx, started, req, "ok", response.Players, response.Step)
	writeJSON(w, http.StatusOK, response)
}

// record is the one place a step becomes a metric, so that the outcome a span recorded and the outcome a
// dashboard counts cannot disagree.
func (s Server) record(ctx context.Context, started time.Time, req PlayRequest, outcome string, players, step int) {
	if s.Instruments.Plays == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String("outcome", outcome),
		attribute.String("action", emptyAs(req.Action, "none")),
	)
	s.Instruments.Plays.Add(ctx, 1, attrs)
	s.Instruments.PlayDuration.Record(ctx, float64(time.Since(started).Milliseconds()), attrs)
	if players > 0 {
		s.Instruments.PlayersInMatch.Record(ctx, int64(players), attrs)
	}
}

func statusFor(err error) int {
	if errors.Is(err, ErrBadRequest) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func emptyAs(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
