package observability

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestMetricsRecordUsesExactNamesUnitsValuesAndBoundedAttributes(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	metrics, err := NewMetrics(t.TempDir(), MetricsOptions{MetricReaders: []sdkmetric.Reader{reader}})
	if err != nil {
		t.Fatal(err)
	}
	metrics.Record("tool_completed", map[string]any{
		"tool": "read", "status": 0, "duration_seconds": 0.25,
		"input_bytes": 18, "output_bytes": 9, "path": "seed/kernel.c", "request_id": 9,
	})
	metrics.Record("build_completed", map[string]any{"status": 1, "duration_seconds": 1.5})
	metrics.Record("codex_session_started", map[string]any{"model": "gpt-5.6-sol", "reasoning_effort": "high"})
	metrics.Record("codex_turn_completed", map[string]any{"model": "gpt-5.6-sol", "reasoning_effort": "high", "duration_seconds": 2.0})
	metrics.Record("review_completed", map[string]any{"model": "gpt-5.6-luna", "reasoning_effort": "high", "focus": "security", "duration_seconds": 0.5})
	metrics.Record("generation_completed", map[string]any{"transition": "rollback", "duration_seconds": 3.0})
	metrics.Record("operator_pause", map[string]any{"result": "success", "request_id": 9})
	metrics.Record("feature_requested", map[string]any{"request_id": 9})
	metrics.RecordModelTokens("gpt-5.6-sol", "implementor", 100, 40, 60, 25, 10)
	metrics.SetRuntimeState(testMetricsUint64Pointer(47), "running")
	metrics.SetFeatureRequestsPending(2)

	data := testMetricsCollect(t, reader)
	metricsByName := make(map[string]metricdata.Metrics, len(data.ScopeMetrics[0].Metrics))
	for _, metric := range data.ScopeMetrics[0].Metrics {
		metricsByName[metric.Name] = metric
		if metric.Name == "codexos_generation_state" {
			testMetricsAssertGaugeValues(t, metric, map[string]int64{
				"stopped": 0, "running": 1, "paused": 0, "awaiting_next_generation": 0,
			})
		}
	}

	expectedNames := map[string]struct{}{
		"codexos_generation_current": {}, "codexos_generation_state": {},
		"codexos_tool_calls_total": {}, "codexos_tool_duration_seconds": {},
		"codexos_tool_input_bytes_total": {}, "codexos_tool_output_bytes_total": {},
		"codexos_builds_total": {}, "codexos_build_duration_seconds": {},
		"codexos_codex_turns_total": {}, "codexos_codex_turn_duration_seconds": {},
		"codexos_codex_session_starts_total": {}, "codexos_reviews_total": {},
		"codexos_review_duration_seconds": {}, "codexos_generations_total": {},
		"codexos_generation_duration_seconds": {}, "codexos_operator_actions_total": {},
		"codexos_feature_requests_total": {}, "codexos_feature_requests_pending": {},
		"codexos_model_input_tokens_total": {}, "codexos_model_cached_input_tokens_total": {},
		"codexos_model_uncached_input_tokens_total": {}, "codexos_model_output_tokens_total": {},
		"codexos_model_reasoning_output_tokens_total": {},
	}
	if len(metricsByName) != len(expectedNames) {
		t.Fatalf("metric names = %v, want exactly %v", testMetricsNames(metricsByName), testMetricsNamesFromSet(expectedNames))
	}
	for name := range expectedNames {
		if _, ok := metricsByName[name]; !ok {
			t.Errorf("metric %q was not collected", name)
		}
	}

	testMetricsAssertSum(t, metricsByName["codexos_tool_calls_total"], "", map[string]string{"tool": "read", "status": "success"}, 1)
	testMetricsAssertHistogram(t, metricsByName["codexos_tool_duration_seconds"], "s", map[string]string{"tool": "read"}, 0.25)
	testMetricsAssertSum(t, metricsByName["codexos_tool_input_bytes_total"], "By", map[string]string{"tool": "read"}, 18)
	testMetricsAssertSum(t, metricsByName["codexos_tool_output_bytes_total"], "By", map[string]string{"tool": "read"}, 9)
	testMetricsAssertSum(t, metricsByName["codexos_builds_total"], "", map[string]string{"result": "guest_failure"}, 1)
	testMetricsAssertHistogram(t, metricsByName["codexos_build_duration_seconds"], "s", nil, 1.5)
	testMetricsAssertSum(t, metricsByName["codexos_codex_session_starts_total"], "", map[string]string{"model": "gpt-5.6-sol", "effort": "high"}, 1)
	testMetricsAssertSum(t, metricsByName["codexos_codex_turns_total"], "", map[string]string{"model": "gpt-5.6-sol", "effort": "high", "result": "completed"}, 1)
	testMetricsAssertHistogram(t, metricsByName["codexos_codex_turn_duration_seconds"], "s", map[string]string{"model": "gpt-5.6-sol", "effort": "high"}, 2)
	testMetricsAssertSum(t, metricsByName["codexos_reviews_total"], "", map[string]string{"model": "gpt-5.6-luna", "effort": "high", "focus": "security", "result": "completed"}, 1)
	testMetricsAssertHistogram(t, metricsByName["codexos_review_duration_seconds"], "s", map[string]string{"model": "gpt-5.6-luna", "effort": "high", "focus": "security"}, 0.5)
	testMetricsAssertSum(t, metricsByName["codexos_generations_total"], "", map[string]string{"outcome": "completed", "transition": "rollback"}, 1)
	testMetricsAssertHistogram(t, metricsByName["codexos_generation_duration_seconds"], "s", map[string]string{"outcome": "completed", "transition": "rollback"}, 3)
	testMetricsAssertSum(t, metricsByName["codexos_operator_actions_total"], "", map[string]string{"action": "pause", "result": "success"}, 1)
	testMetricsAssertSum(t, metricsByName["codexos_feature_requests_total"], "", map[string]string{"event": "requested"}, 1)
	testMetricsAssertGauge(t, metricsByName["codexos_generation_current"], "", nil, 47)
	testMetricsAssertGauge(t, metricsByName["codexos_feature_requests_pending"], "", nil, 2)

	for _, name := range []string{
		"codexos_model_input_tokens_total", "codexos_model_cached_input_tokens_total",
		"codexos_model_uncached_input_tokens_total", "codexos_model_output_tokens_total",
		"codexos_model_reasoning_output_tokens_total",
	} {
		if metricsByName[name].Unit != "{token}" {
			t.Errorf("%s unit = %q, want {token}", name, metricsByName[name].Unit)
		}
	}
	testMetricsAssertSum(t, metricsByName["codexos_model_input_tokens_total"], "{token}", map[string]string{"model": "gpt-5.6-sol", "role": "implementor"}, 100)
	testMetricsAssertSum(t, metricsByName["codexos_model_cached_input_tokens_total"], "{token}", map[string]string{"model": "gpt-5.6-sol", "role": "implementor"}, 40)
	testMetricsAssertSum(t, metricsByName["codexos_model_uncached_input_tokens_total"], "{token}", map[string]string{"model": "gpt-5.6-sol", "role": "implementor"}, 60)
	testMetricsAssertSum(t, metricsByName["codexos_model_output_tokens_total"], "{token}", map[string]string{"model": "gpt-5.6-sol", "role": "implementor"}, 25)
	testMetricsAssertSum(t, metricsByName["codexos_model_reasoning_output_tokens_total"], "{token}", map[string]string{"model": "gpt-5.6-sol", "role": "implementor"}, 10)
	metrics.Close()
	metrics.Close()
}

func TestMetricsMalformedPayloadAndCountsDegradeWithoutAffectingOtherEvents(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	metrics, err := NewMetrics(t.TempDir(), MetricsOptions{MetricReaders: []sdkmetric.Reader{reader}})
	if err != nil {
		t.Fatal(err)
	}
	metrics.Record("tool_completed", map[string]any{"tool": "read", "duration_seconds": "not-a-duration"})
	metrics.Record("build_completed", map[string]any{"duration_seconds": "not-a-duration", "status": 0})
	metrics.RecordModelTokens("model", "role", testMetricsMaxInt64Uint64()+1, 0, 0, 0, 0)
	metrics.Record("operator_pause", map[string]any{"result": "success"})
	if metrics.Healthy() || metrics.DegradedReason() == "" {
		t.Fatalf("metrics did not degrade: healthy=%v reason=%q", metrics.Healthy(), metrics.DegradedReason())
	}
	data := testMetricsCollect(t, reader)
	for _, scope := range data.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name == "codexos_tool_calls_total" || metric.Name == "codexos_builds_total" {
				t.Fatalf("malformed metric unexpectedly recorded: %s", metric.Name)
			}
		}
	}
	metrics.Close()
}

func TestMetricsOTLPExportUsesConfiguredEndpointAndExcludesPrivateFields(t *testing.T) {
	type request struct {
		path string
		body []byte
	}
	received := make(chan request, 2)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, input *http.Request) {
		body, err := io.ReadAll(input.Body)
		if err != nil {
			t.Errorf("read OTLP request: %v", err)
		}
		received <- request{path: input.URL.Path, body: body}
		response.Header().Set("Content-Type", "application/x-protobuf")
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	run := filepath.Join(t.TempDir(), "run-observable-identity")
	metrics, err := NewMetrics(run, MetricsOptions{OTLPEndpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	metrics.Record("tool_completed", map[string]any{
		"tool": "read", "status": 0, "duration_seconds": 0.25,
		"path": "SOURCE-CONTENT-SECRET", "request_id": 47,
	})
	metrics.Close()
	metrics.Close()

	select {
	case exported := <-received:
		if exported.path != "/v1/metrics" {
			t.Fatalf("OTLP path = %q, want /v1/metrics", exported.path)
		}
		for _, expected := range [][]byte{[]byte(metricsServiceName), []byte("run-observable-identity")} {
			if !bytes.Contains(exported.body, expected) {
				t.Errorf("OTLP body does not contain resource value %q", expected)
			}
		}
		if bytes.Contains(exported.body, []byte("SOURCE-CONTENT-SECRET")) {
			t.Fatal("private event field was exported as metric data")
		}
	case <-time.After(time.Second):
		t.Fatal("metrics close did not flush to the configured OTLP endpoint")
	}
}

func TestMetricsNormalizeUnboundedOrUnknownLabels(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	metrics, err := NewMetrics(t.TempDir(), MetricsOptions{MetricReaders: []sdkmetric.Reader{reader}})
	if err != nil {
		t.Fatal(err)
	}
	longLabel := strings.Repeat("λ", 100)
	boundedLabel := strings.Repeat("λ", 64)
	metrics.Record("tool_completed", map[string]any{
		"tool": longLabel, "status": "invalid", "duration_seconds": 0.1,
	})
	metrics.Record("review_completed", map[string]any{
		"model": longLabel, "reasoning_effort": "high", "focus": "private-path",
		"duration_seconds": 0.2,
	})
	metrics.Record("operator_private-command", map[string]any{"result": longLabel})
	metrics.RecordModelTokens(longLabel, "untrusted-role", 1, 0, 0, 0, 0)

	data := testMetricsCollect(t, reader)
	metricsByName := make(map[string]metricdata.Metrics)
	for _, scope := range data.ScopeMetrics {
		for _, collected := range scope.Metrics {
			metricsByName[collected.Name] = collected
		}
	}
	testMetricsAssertSum(t, metricsByName["codexos_tool_calls_total"], "", map[string]string{
		"tool": boundedLabel, "status": "other_failure",
	}, 1)
	testMetricsAssertSum(t, metricsByName["codexos_reviews_total"], "", map[string]string{
		"model": boundedLabel, "effort": "high", "focus": "other", "result": "completed",
	}, 1)
	testMetricsAssertSum(t, metricsByName["codexos_operator_actions_total"], "", map[string]string{
		"action": "other", "result": boundedLabel,
	}, 1)
	testMetricsAssertSum(t, metricsByName["codexos_model_input_tokens_total"], "{token}", map[string]string{
		"model": boundedLabel, "role": "unknown",
	}, 1)
	metrics.Close()
}

func testMetricsCollect(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.ScopeMetrics) != 1 {
		t.Fatalf("scope metrics = %d, want one", len(data.ScopeMetrics))
	}
	return data
}

func testMetricsAssertSum(t *testing.T, metric metricdata.Metrics, unit string, attrs map[string]string, want int64) {
	t.Helper()
	if metric.Unit != unit {
		t.Errorf("%s unit = %q, want %q", metric.Name, metric.Unit, unit)
	}
	points, ok := metric.Data.(metricdata.Sum[int64])
	if !ok || len(points.DataPoints) != 1 {
		t.Fatalf("%s data = %#v, want one int64 sum", metric.Name, metric.Data)
	}
	testMetricsAssertAttrs(t, metric.Name, points.DataPoints[0].Attributes, attrs)
	if points.DataPoints[0].Value != want {
		t.Errorf("%s value = %d, want %d", metric.Name, points.DataPoints[0].Value, want)
	}
}

func testMetricsAssertHistogram(t *testing.T, metric metricdata.Metrics, unit string, attrs map[string]string, want float64) {
	t.Helper()
	if metric.Unit != unit {
		t.Errorf("%s unit = %q, want %q", metric.Name, metric.Unit, unit)
	}
	points, ok := metric.Data.(metricdata.Histogram[float64])
	if !ok || len(points.DataPoints) != 1 {
		t.Fatalf("%s data = %#v, want one float64 histogram", metric.Name, metric.Data)
	}
	testMetricsAssertAttrs(t, metric.Name, points.DataPoints[0].Attributes, attrs)
	if points.DataPoints[0].Sum != want || points.DataPoints[0].Count != 1 {
		t.Errorf("%s sum/count = %v/%d, want %v/1", metric.Name, points.DataPoints[0].Sum, points.DataPoints[0].Count, want)
	}
}

func testMetricsAssertGauge(t *testing.T, metric metricdata.Metrics, unit string, attrs map[string]string, want int64) {
	t.Helper()
	if metric.Unit != unit {
		t.Errorf("%s unit = %q, want %q", metric.Name, metric.Unit, unit)
	}
	points, ok := metric.Data.(metricdata.Gauge[int64])
	if !ok || len(points.DataPoints) != 1 {
		t.Fatalf("%s data = %#v, want one int64 gauge", metric.Name, metric.Data)
	}
	testMetricsAssertAttrs(t, metric.Name, points.DataPoints[0].Attributes, attrs)
	if points.DataPoints[0].Value != want {
		t.Errorf("%s value = %d, want %d", metric.Name, points.DataPoints[0].Value, want)
	}
}

func testMetricsAssertGaugeValues(t *testing.T, metric metricdata.Metrics, want map[string]int64) {
	t.Helper()
	points, ok := metric.Data.(metricdata.Gauge[int64])
	if !ok || len(points.DataPoints) != len(want) {
		t.Fatalf("%s data = %#v, want %d gauge points", metric.Name, metric.Data, len(want))
	}
	for _, point := range points.DataPoints {
		attrs := point.Attributes.ToSlice()
		if len(attrs) != 1 {
			t.Fatalf("%s state attrs = %v, want one", metric.Name, attrs)
		}
		state := attrs[0].Value.AsString()
		if point.Value != want[state] {
			t.Errorf("state %q value = %d, want %d", state, point.Value, want[state])
		}
	}
}

func testMetricsAssertAttrs(t *testing.T, name string, got attribute.Set, want map[string]string) {
	t.Helper()
	actual := make(map[string]string)
	for _, keyValue := range got.ToSlice() {
		actual[string(keyValue.Key)] = keyValue.Value.AsString()
		if keyValue.Key == "path" || keyValue.Key == "request_id" || keyValue.Key == "call_id" || keyValue.Key == "generation" {
			t.Errorf("%s has private attribute %q", name, keyValue.Key)
		}
	}
	if len(actual) != len(want) {
		t.Errorf("%s attrs = %v, want %v", name, actual, want)
		return
	}
	for key, value := range want {
		if actual[key] != value {
			t.Errorf("%s attr %q = %q, want %q", name, key, actual[key], value)
		}
	}
}

func testMetricsNames(metrics map[string]metricdata.Metrics) []string {
	result := make([]string, 0, len(metrics))
	for name := range metrics {
		result = append(result, name)
	}
	return result
}

func testMetricsNamesFromSet(metrics map[string]struct{}) []string {
	result := make([]string, 0, len(metrics))
	for name := range metrics {
		result = append(result, name)
	}
	return result
}

func testMetricsMaxInt64Uint64() uint64 { return uint64(^uint64(0) >> 1) }

func testMetricsUint64Pointer(value uint64) *uint64 { return &value }
