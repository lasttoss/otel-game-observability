package gameserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lasttoss/otel-game-observability"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// The instrumented server, with exporters a test can read instead of a collector a test would have to be
// given. This is the whole reason the telemetry is wired through a provider rather than created inline.
type harness struct {
	server   gameserver.Server
	spans    *tracetest.InMemoryExporter
	reader   *sdkmetric.ManualReader
	provider *sdktrace.TracerProvider
}

func newHarness(t *testing.T, step gameserver.StepFunc) *harness {
	t.Helper()

	spans := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spans))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = meterProvider.Shutdown(context.Background()) })

	instruments, err := gameserver.NewInstruments(meterProvider.Meter("test"))
	if err != nil {
		t.Fatalf("instruments: %v", err)
	}
	t.Cleanup(spans.Reset)

	return &harness{
		server:   gameserver.Server{Tracer: provider.Tracer("test"), Instruments: instruments, Step: step},
		spans:    spans,
		reader:   reader,
		provider: provider,
	}
}

func (h *harness) play(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/play", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	h.server.Routes().ServeHTTP(recorder, request)
	return recorder
}

func (h *harness) metrics(t *testing.T) metricdata.ResourceMetrics {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("collect: %v", err)
	}
	return collected
}

// The claim the whole repository rests on: one step of one match is one span, and the span carries what a
// person asking "which match was slow" needs to find it.
func TestAStepIsASpanWithTheMatchAndThePlayerOnIt(t *testing.T) {
	h := newHarness(t, func(context.Context, gameserver.PlayRequest) (gameserver.PlayResponse, error) {
		return gameserver.PlayResponse{MatchID: "m-1", Step: 3, Players: 4, Accepted: true}, nil
	})

	recorder := h.play(t, `{"match_id":"m-1","player_id":"p-9","action":"move"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body)
	}

	var response gameserver.PlayResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("the response is not JSON: %v (%s)", err, recorder.Body)
	}
	if response.Step != 3 || response.Players != 4 {
		t.Errorf("response = %+v", response)
	}

	spans := h.spans.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("%d spans were recorded, want 1: %v", len(spans), spans)
	}
	span := spans[0]
	if span.Name != "play" {
		t.Errorf("span name = %q, want play", span.Name)
	}
	want := map[string]string{
		"game.match.id":  "m-1",
		"game.player.id": "p-9",
		"game.action":    "move",
	}
	for key, value := range want {
		if got := attr(span.Attributes, key); got != value {
			t.Errorf("span attribute %s = %q, want %q", key, got, value)
		}
	}
	if got := attrInt(span.Attributes, "game.match.step"); got != 3 {
		t.Errorf("game.match.step = %d, want 3", got)
	}
	if got := attrInt(span.Attributes, "game.match.players"); got != 4 {
		t.Errorf("game.match.players = %d, want 4", got)
	}
	if span.Status.Code == codes.Error {
		t.Error("a served step was recorded as an error")
	}
}

func TestAPayloadWithNoPlayerIsA400AndASpanThatSaysWhy(t *testing.T) {
	called := false
	h := newHarness(t, func(context.Context, gameserver.PlayRequest) (gameserver.PlayResponse, error) {
		called = true
		return gameserver.PlayResponse{}, nil
	})

	recorder := h.play(t, `{"match_id":"m-1"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", recorder.Code)
	}
	if called {
		t.Error("the game logic ran for a request that was missing a player")
	}
	if spans := h.spans.GetSpans(); len(spans) != 1 || spans[0].Status.Code != codes.Error {
		t.Errorf("the rejected request was not recorded as an error span: %v", spans)
	}
	if count(t, h.metrics(t), "gameserver.plays", "invalid") != 1 {
		t.Error("the rejected request is not in the play counter as invalid")
	}
	// The request that gets a 400 is the one somebody will want to trace back to a client, so the span
	// has to keep what the client actually sent.
	if got := attr(h.spans.GetSpans()[0].Attributes, "game.match.id"); got != "m-1" {
		t.Errorf("the rejected span does not carry the match id: %q", got)
	}
}

// A step the game refused is the client's problem and must not look like a server failure on a dashboard:
// the same shape as a 400, not the same as a 500.
func TestARejectedStepIsNotAServerError(t *testing.T) {
	h := newHarness(t, func(context.Context, gameserver.PlayRequest) (gameserver.PlayResponse, error) {
		return gameserver.PlayResponse{}, gameserver.ErrBadRequest
	})

	recorder := h.play(t, `{"match_id":"m-1","player_id":"p-9","action":"move"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", recorder.Code)
	}
	if spans := h.spans.GetSpans(); len(spans) != 1 || spans[0].Status.Code == codes.Error {
		t.Errorf("a rejected step was recorded as a server error: %v", spans)
	}
	if count(t, h.metrics(t), "gameserver.plays", "invalid") != 1 {
		t.Error("a rejected step is not counted as invalid")
	}
}

func TestAStepThatFailedIsAnErrorSpanAndAFailedMetric(t *testing.T) {
	failure := errors.New("the match is gone")
	h := newHarness(t, func(context.Context, gameserver.PlayRequest) (gameserver.PlayResponse, error) {
		return gameserver.PlayResponse{}, failure
	})

	recorder := h.play(t, `{"match_id":"m-1","player_id":"p-9"}`)
	if recorder.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", recorder.Code)
	}
	spans := h.spans.GetSpans()
	if len(spans) != 1 || spans[0].Status.Code != codes.Error {
		t.Fatalf("spans = %v, want one error span", spans)
	}
	if !strings.Contains(spans[0].Status.Description, "the match is gone") {
		t.Errorf("the span does not say why: %q", spans[0].Status.Description)
	}
	if got := count(t, h.metrics(t), "gameserver.plays", "failed"); got != 1 {
		t.Errorf("the failed play is counted %d times, want 1", got)
	}
}

// A dashboard is only as good as the agreement between what it counts and what a trace shows, so the two
// are asserted against the same requests here.
func TestTheCountersCountWhatTheSpansShow(t *testing.T) {
	h := newHarness(t, func(context.Context, gameserver.PlayRequest) (gameserver.PlayResponse, error) {
		return gameserver.PlayResponse{MatchID: "m-1", Step: 1, Players: 10, Accepted: true}, nil
	})

	for i := 0; i < 5; i++ {
		if recorder := h.play(t, `{"match_id":"m-1","player_id":"p-1","action":"move"}`); recorder.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d", i, recorder.Code)
		}
	}
	h.play(t, `{"match_id":"m-1"}`) // one bad request

	if got := len(h.spans.GetSpans()); got != 6 {
		t.Errorf("%d spans for 6 requests", got)
	}
	collected := h.metrics(t)
	if got := count(t, collected, "gameserver.plays", "ok"); got != 5 {
		t.Errorf("ok plays = %d, want 5", got)
	}
	if got := count(t, collected, "gameserver.plays", "invalid"); got != 1 {
		t.Errorf("invalid plays = %d, want 1", got)
	}
	if got := histogramCount(t, collected, "gameserver.play.duration"); got != 6 {
		t.Errorf("the duration histogram has %d samples for 6 requests", got)
	}
	if got := histogramCount(t, collected, "gameserver.match.players"); got != 5 {
		t.Errorf("the players histogram has %d samples, want one per served step", got)
	}
}

func TestHealthzAnswersWithoutASpan(t *testing.T) {
	h := newHarness(t, func(context.Context, gameserver.PlayRequest) (gameserver.PlayResponse, error) {
		return gameserver.PlayResponse{}, nil
	})

	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	recorder := httptest.NewRecorder()
	h.server.Routes().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", recorder.Code)
	}
	if got := len(h.spans.GetSpans()); got != 0 {
		t.Errorf("a health check made %d spans: a probe every five seconds would drown the traces", got)
	}
}

func attr(attrs []attribute.KeyValue, key string) string {
	for _, a := range attrs {
		if string(a.Key) == key {
			return a.Value.AsString()
		}
	}
	return ""
}

func attrInt(attrs []attribute.KeyValue, key string) int64 {
	for _, a := range attrs {
		if string(a.Key) == key {
			return a.Value.AsInt64()
		}
	}
	return 0
}

// count is the sum of gameserver.plays for one outcome.
func count(t *testing.T, collected metricdata.ResourceMetrics, name, outcome string) int64 {
	t.Helper()
	for _, scope := range collected.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != name {
				continue
			}
			sum, ok := metric.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want a sum", name, metric.Data)
			}
			var total int64
			for _, point := range sum.DataPoints {
				if string(attr(point.Attributes.ToSlice(), "outcome")) == outcome {
					total += point.Value
				}
			}
			return total
		}
	}
	return 0
}

func histogramCount(t *testing.T, collected metricdata.ResourceMetrics, name string) uint64 {
	t.Helper()
	for _, scope := range collected.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != name {
				continue
			}
			switch data := metric.Data.(type) {
			case metricdata.Histogram[float64]:
				var total uint64
				for _, point := range data.DataPoints {
					total += point.Count
				}
				return total
			case metricdata.Histogram[int64]:
				var total uint64
				for _, point := range data.DataPoints {
					total += point.Count
				}
				return total
			default:
				t.Fatalf("%s is %T, want a histogram", name, metric.Data)
			}
		}
	}
	return 0
}
