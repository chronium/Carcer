package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

const (
	metricsServiceName           = "codexos-harness"
	metricsScopeName             = "codexos-harness"
	metricsShutdownTimeout       = 2 * time.Second
	metricsExportInterval        = 5 * time.Second
	metricsExportTimeout         = 2 * time.Second
	metricsMaxLabelBytes         = 128
	metricsUnitSeconds           = "s"
	metricsUnitBytes             = "By"
	metricsUnitTokens            = "{token}"
	maxMetricInteger       int64 = math.MaxInt64
)

var runtimeStates = [...]string{
	"stopped",
	"running",
	"paused",
	"awaiting_next_generation",
}

// MetricsOptions configures the metrics pipeline. Readers are useful for
// pull-based collection in tests and by an embedding harness. When
// OTLPEndpoint is non-empty, a periodic OTLP/HTTP reader is added in addition
// to the supplied readers.
type MetricsOptions struct {
	OTLPEndpoint  string
	MetricReaders []sdkmetric.Reader
}

// Metrics owns the OpenTelemetry instruments used by one harness run.
//
// Metrics are deliberately independent from EventLog: a malformed metric
// payload or an exporter failure must never change experiment control flow or
// the durable local event record.
type Metrics struct {
	provider *sdkmetric.MeterProvider

	mu     sync.Mutex
	closed bool

	healthMu       sync.Mutex
	degradedReason string

	generationCurrent        metric.Int64Gauge
	generationState          metric.Int64Gauge
	toolCalls                metric.Int64Counter
	toolDuration             metric.Float64Histogram
	toolInputBytes           metric.Int64Counter
	toolOutputBytes          metric.Int64Counter
	builds                   metric.Int64Counter
	buildDuration            metric.Float64Histogram
	codexTurns               metric.Int64Counter
	codexTurnDuration        metric.Float64Histogram
	codexSessionStarts       metric.Int64Counter
	reviews                  metric.Int64Counter
	reviewDuration           metric.Float64Histogram
	generations              metric.Int64Counter
	generationDuration       metric.Float64Histogram
	operatorActions          metric.Int64Counter
	featureRequests          metric.Int64Counter
	featureRequestsPending   metric.Int64Gauge
	modelInputTokens         metric.Int64Counter
	modelCachedInputTokens   metric.Int64Counter
	modelUncachedInputTokens metric.Int64Counter
	modelOutputTokens        metric.Int64Counter
	modelReasoningTokens     metric.Int64Counter
}

// MetricsError reports metrics setup failures. Runtime metric failures are
// recorded through Healthy and DegradedReason instead of being returned to the
// experiment caller.
type MetricsError struct {
	Reason string
	Err    error
}

func (e *MetricsError) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *MetricsError) Unwrap() error { return e.Err }

// NewMetrics creates the metrics pipeline for runDirectory. It performs no
// network activity unless an OTLP endpoint is explicitly configured.
func NewMetrics(runDirectory string, options MetricsOptions) (*Metrics, error) {
	run, err := filepath.Abs(runDirectory)
	if err != nil {
		return nil, &MetricsError{Reason: "could not resolve metrics run directory", Err: err}
	}
	if err := ensureMetricsRunDirectory(run); err != nil {
		return nil, &MetricsError{Reason: "could not create metrics run directory", Err: err}
	}

	readers := append([]sdkmetric.Reader(nil), options.MetricReaders...)
	for _, reader := range readers {
		if reader == nil {
			return nil, &MetricsError{Reason: "metrics reader must not be nil"}
		}
	}
	if endpoint := strings.TrimSpace(options.OTLPEndpoint); endpoint != "" {
		exporter, exportErr := otlpmetrichttp.New(
			context.Background(),
			otlpmetrichttp.WithEndpointURL(strings.TrimRight(endpoint, "/")+"/v1/metrics"),
			otlpmetrichttp.WithTimeout(metricsExportTimeout),
		)
		if exportErr != nil {
			return nil, &MetricsError{Reason: "could not create OTLP metric exporter", Err: exportErr}
		}
		readers = append(readers, sdkmetric.NewPeriodicReader(
			exporter,
			sdkmetric.WithInterval(metricsExportInterval),
			sdkmetric.WithTimeout(metricsExportTimeout),
		))
	}

	providerOptions := []sdkmetric.Option{
		sdkmetric.WithResource(resource.NewWithAttributes(
			"",
			attribute.String("service.name", metricsServiceName),
			attribute.String("codexos.run", filepath.Base(run)),
		)),
	}
	for _, reader := range readers {
		providerOptions = append(providerOptions, sdkmetric.WithReader(reader))
	}
	provider := sdkmetric.NewMeterProvider(providerOptions...)
	meter := provider.Meter(metricsScopeName)
	metrics, instrumentErr := newMetricsInstruments(provider, meter)
	if instrumentErr != nil {
		shutdownMetricsProvider(provider)
		return nil, instrumentErr
	}
	return metrics, nil
}

func ensureMetricsRunDirectory(run string) error {
	return os.MkdirAll(run, 0o755)
}

// Healthy reports whether runtime metric recording has degraded.
func (m *Metrics) Healthy() bool {
	if m == nil {
		return false
	}
	m.healthMu.Lock()
	degraded := m.degradedReason != ""
	m.healthMu.Unlock()
	return !degraded
}

// DegradedReason returns the first runtime metrics failure, if any.
func (m *Metrics) DegradedReason() string {
	if m == nil {
		return "metrics is nil"
	}
	m.healthMu.Lock()
	reason := m.degradedReason
	m.healthMu.Unlock()
	return reason
}

// Degrade records the first metrics problem without changing experiment
// control flow.
func (m *Metrics) Degrade(reason string) {
	if m == nil {
		return
	}
	m.healthMu.Lock()
	if m.degradedReason == "" {
		m.degradedReason = reason
		log.Printf("CodexOS observability degraded: %s", reason)
	}
	m.healthMu.Unlock()
}

// Record translates one structured event into bounded, low-cardinality
// metrics. Event data is never used as a free-form attribute set.
func (m *Metrics) Record(event string, data map[string]any) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.recordMetrics(event, data)
}

// SetRuntimeState publishes one-hot state gauges for the current run. The
// generation value is intentionally an unlabelled gauge to avoid cardinality
// growth.
func (m *Metrics) SetRuntimeState(generation *uint64, state string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	if generation != nil {
		value, ok := metricUint64(*generation)
		if !ok {
			m.Degrade("runtime-state metric recording failed: generation exceeds int64")
			return
		}
		m.generationCurrent.Record(context.Background(), value)
	}
	for _, knownState := range runtimeStates {
		value := int64(0)
		if knownState == state {
			value = 1
		}
		m.generationState.Record(context.Background(), value, metric.WithAttributes(attribute.String("state", knownState)))
	}
}

// SetFeatureRequestsPending publishes the current number of pending feature
// requests without labels.
func (m *Metrics) SetFeatureRequestsPending(count int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	value, ok := intToMetricInt64(count)
	if !ok {
		m.Degrade("feature-request metric recording failed: pending count exceeds int64")
		return
	}
	m.featureRequestsPending.Record(context.Background(), value)
}

// RecordModelTokens records a non-duplicating token delta using only model and
// role as attributes. The caller is responsible for deriving the delta from a
// cumulative app-server usage snapshot.
func (m *Metrics) RecordModelTokens(model, role string, inputTokens, cachedInputTokens, uncachedInputTokens, outputTokens, reasoningOutputTokens uint64) {
	if m == nil {
		return
	}
	values := [...]uint64{inputTokens, cachedInputTokens, uncachedInputTokens, outputTokens, reasoningOutputTokens}
	converted := [len(values)]int64{}
	for index, value := range values {
		if value > uint64(maxMetricInteger) {
			m.Degrade("token metric recording failed: token count exceeds int64")
			return
		}
		converted[index] = int64(value)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	attrs := metric.WithAttributes(attribute.String("model", boundedMetricLabel(model)), attribute.String("role", metricRoleLabel(role)))
	ctx := context.Background()
	m.modelInputTokens.Add(ctx, converted[0], attrs)
	m.modelCachedInputTokens.Add(ctx, converted[1], attrs)
	m.modelUncachedInputTokens.Add(ctx, converted[2], attrs)
	m.modelOutputTokens.Add(ctx, converted[3], attrs)
	m.modelReasoningTokens.Add(ctx, converted[4], attrs)
}

// Close is idempotent. Metric flush and shutdown each receive a bounded
// two-second context; errors are isolated from experiment control flow.
func (m *Metrics) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), metricsShutdownTimeout)
	defer cancel()
	if err := m.provider.ForceFlush(ctx); err != nil {
		log.Printf("OpenTelemetry metric flush failed: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), metricsShutdownTimeout)
	defer cancel()
	if err := m.provider.Shutdown(ctx); err != nil {
		log.Printf("OpenTelemetry metric shutdown failed: %v", err)
	}
}

func newMetricsInstruments(provider *sdkmetric.MeterProvider, meter metric.Meter) (*Metrics, error) {
	metrics := &Metrics{provider: provider}
	var err error
	if metrics.generationCurrent, err = meter.Int64Gauge("codexos_generation_current"); err != nil {
		return nil, newInstrumentError(err)
	}
	if metrics.generationState, err = meter.Int64Gauge("codexos_generation_state"); err != nil {
		return nil, newInstrumentError(err)
	}
	if metrics.toolCalls, err = meter.Int64Counter("codexos_tool_calls_total"); err != nil {
		return nil, newInstrumentError(err)
	}
	if metrics.toolDuration, err = meter.Float64Histogram("codexos_tool_duration_seconds", metric.WithUnit(metricsUnitSeconds)); err != nil {
		return nil, newInstrumentError(err)
	}
	if metrics.toolInputBytes, err = meter.Int64Counter("codexos_tool_input_bytes_total", metric.WithUnit(metricsUnitBytes)); err != nil {
		return nil, newInstrumentError(err)
	}
	if metrics.toolOutputBytes, err = meter.Int64Counter("codexos_tool_output_bytes_total", metric.WithUnit(metricsUnitBytes)); err != nil {
		return nil, newInstrumentError(err)
	}
	if metrics.builds, err = meter.Int64Counter("codexos_builds_total"); err != nil {
		return nil, newInstrumentError(err)
	}
	if metrics.buildDuration, err = meter.Float64Histogram("codexos_build_duration_seconds", metric.WithUnit(metricsUnitSeconds)); err != nil {
		return nil, newInstrumentError(err)
	}
	if metrics.codexTurns, err = meter.Int64Counter("codexos_codex_turns_total"); err != nil {
		return nil, newInstrumentError(err)
	}
	if metrics.codexTurnDuration, err = meter.Float64Histogram("codexos_codex_turn_duration_seconds", metric.WithUnit(metricsUnitSeconds)); err != nil {
		return nil, newInstrumentError(err)
	}
	if metrics.codexSessionStarts, err = meter.Int64Counter("codexos_codex_session_starts_total"); err != nil {
		return nil, newInstrumentError(err)
	}
	if metrics.reviews, err = meter.Int64Counter("codexos_reviews_total"); err != nil {
		return nil, newInstrumentError(err)
	}
	if metrics.reviewDuration, err = meter.Float64Histogram("codexos_review_duration_seconds", metric.WithUnit(metricsUnitSeconds)); err != nil {
		return nil, newInstrumentError(err)
	}
	if metrics.generations, err = meter.Int64Counter("codexos_generations_total"); err != nil {
		return nil, newInstrumentError(err)
	}
	if metrics.generationDuration, err = meter.Float64Histogram("codexos_generation_duration_seconds", metric.WithUnit(metricsUnitSeconds)); err != nil {
		return nil, newInstrumentError(err)
	}
	if metrics.operatorActions, err = meter.Int64Counter("codexos_operator_actions_total"); err != nil {
		return nil, newInstrumentError(err)
	}
	if metrics.featureRequests, err = meter.Int64Counter("codexos_feature_requests_total"); err != nil {
		return nil, newInstrumentError(err)
	}
	if metrics.featureRequestsPending, err = meter.Int64Gauge("codexos_feature_requests_pending"); err != nil {
		return nil, newInstrumentError(err)
	}
	if metrics.modelInputTokens, err = meter.Int64Counter("codexos_model_input_tokens_total", metric.WithUnit(metricsUnitTokens)); err != nil {
		return nil, newInstrumentError(err)
	}
	if metrics.modelCachedInputTokens, err = meter.Int64Counter("codexos_model_cached_input_tokens_total", metric.WithUnit(metricsUnitTokens)); err != nil {
		return nil, newInstrumentError(err)
	}
	if metrics.modelUncachedInputTokens, err = meter.Int64Counter("codexos_model_uncached_input_tokens_total", metric.WithUnit(metricsUnitTokens)); err != nil {
		return nil, newInstrumentError(err)
	}
	if metrics.modelOutputTokens, err = meter.Int64Counter("codexos_model_output_tokens_total", metric.WithUnit(metricsUnitTokens)); err != nil {
		return nil, newInstrumentError(err)
	}
	if metrics.modelReasoningTokens, err = meter.Int64Counter("codexos_model_reasoning_output_tokens_total", metric.WithUnit(metricsUnitTokens)); err != nil {
		return nil, newInstrumentError(err)
	}
	metrics.SetRuntimeState(nil, "stopped")
	return metrics, nil
}

func newInstrumentError(err error) error {
	return &MetricsError{Reason: "could not create metrics instrument", Err: err}
}

func shutdownMetricsProvider(provider *sdkmetric.MeterProvider) {
	ctx, cancel := context.WithTimeout(context.Background(), metricsShutdownTimeout)
	defer cancel()
	_ = provider.Shutdown(ctx)
}

func (m *Metrics) recordMetrics(event string, data map[string]any) {
	ctx := context.Background()
	switch event {
	case "tool_completed":
		tool, ok := metricString(data, "tool")
		if !ok {
			m.Degrade("metric recording failed for tool_completed: missing tool")
			return
		}
		status := toolStatus(metricValue(data, "status"))
		duration, ok := metricFloat(data, "duration_seconds")
		if !ok {
			m.Degrade("metric recording failed for tool_completed: invalid duration_seconds")
			return
		}
		inputBytes, ok := metricNonNegativeInt(data, "input_bytes", 0)
		if !ok {
			m.Degrade("metric recording failed for tool_completed: invalid input_bytes")
			return
		}
		outputBytes, ok := metricNonNegativeInt(data, "output_bytes", 0)
		if !ok {
			m.Degrade("metric recording failed for tool_completed: invalid output_bytes")
			return
		}
		tool = boundedMetricLabel(tool)
		toolAttrs := metric.WithAttributes(attribute.String("tool", tool))
		m.toolCalls.Add(ctx, 1, metric.WithAttributes(attribute.String("tool", tool), attribute.String("status", status)))
		m.toolDuration.Record(ctx, duration, toolAttrs)
		m.toolInputBytes.Add(ctx, inputBytes, toolAttrs)
		m.toolOutputBytes.Add(ctx, outputBytes, toolAttrs)
	case "build_completed":
		status := buildResult(metricValue(data, "status"))
		duration, ok := metricFloat(data, "duration_seconds")
		if !ok {
			m.Degrade("metric recording failed for build_completed: invalid duration_seconds")
			return
		}
		m.builds.Add(ctx, 1, metric.WithAttributes(attribute.String("result", status)))
		m.buildDuration.Record(ctx, duration)
	case "codex_session_started":
		model, modelOK := metricString(data, "model")
		effort, effortOK := metricString(data, "reasoning_effort")
		if !modelOK || !effortOK {
			m.Degrade("metric recording failed for codex_session_started: missing model or reasoning_effort")
			return
		}
		m.codexSessionStarts.Add(ctx, 1, metric.WithAttributes(attribute.String("model", boundedMetricLabel(model)), attribute.String("effort", boundedMetricLabel(effort))))
	case "codex_turn_completed", "codex_turn_interrupted", "codex_turn_failed", "planning_completed", "planning_interrupted", "planning_failed":
		model, modelOK := metricString(data, "model")
		effort, effortOK := metricString(data, "reasoning_effort")
		duration, durationOK := metricFloat(data, "duration_seconds")
		if !modelOK || !effortOK || !durationOK {
			m.Degrade("metric recording failed for " + event + ": missing model, reasoning_effort, or duration_seconds")
			return
		}
		result := strings.TrimPrefix(event, "codex_turn_")
		if strings.HasPrefix(event, "planning_") {
			result = strings.TrimPrefix(event, "planning_")
		}
		model = boundedMetricLabel(model)
		effort = boundedMetricLabel(effort)
		attrs := metric.WithAttributes(attribute.String("model", model), attribute.String("effort", effort))
		m.codexTurns.Add(ctx, 1, metric.WithAttributes(attribute.String("model", model), attribute.String("effort", effort), attribute.String("result", result)))
		m.codexTurnDuration.Record(ctx, duration, attrs)
	case "review_completed", "review_failed", "review_cancelled":
		model, modelOK := metricString(data, "model")
		effort, effortOK := metricString(data, "reasoning_effort")
		focus, focusOK := metricString(data, "focus")
		duration, durationOK := metricFloat(data, "duration_seconds")
		if !modelOK || !effortOK || !focusOK || !durationOK {
			m.Degrade("metric recording failed for " + event + ": missing review metric fields")
			return
		}
		result := strings.TrimPrefix(event, "review_")
		model = boundedMetricLabel(model)
		effort = boundedMetricLabel(effort)
		focus = metricReviewFocus(focus)
		attrs := metric.WithAttributes(attribute.String("model", model), attribute.String("effort", effort), attribute.String("focus", focus))
		m.reviews.Add(ctx, 1, metric.WithAttributes(attribute.String("model", model), attribute.String("effort", effort), attribute.String("focus", focus), attribute.String("result", result)))
		m.reviewDuration.Record(ctx, duration, attrs)
	case "generation_completed", "generation_aborted":
		transition, ok := metricString(data, "transition")
		duration, durationOK := metricFloat(data, "duration_seconds")
		if !ok || !durationOK {
			m.Degrade("metric recording failed for " + event + ": missing transition or duration_seconds")
			return
		}
		outcome := strings.TrimPrefix(event, "generation_")
		transition = boundedMetricLabel(transition)
		attrs := metric.WithAttributes(attribute.String("outcome", outcome), attribute.String("transition", transition))
		m.generations.Add(ctx, 1, attrs)
		m.generationDuration.Record(ctx, duration, attrs)
	case "operator_abort_feedback_attached":
		// This provenance event is not an operator command result.
		return
	default:
		if strings.HasPrefix(event, "operator_") {
			action := metricOperatorAction(strings.TrimPrefix(event, "operator_"))
			result, ok := metricString(data, "result")
			if !ok {
				m.Degrade("metric recording failed for " + event + ": missing result")
				return
			}
			m.operatorActions.Add(ctx, 1, metric.WithAttributes(attribute.String("action", action), attribute.String("result", boundedMetricLabel(result))))
			return
		}
		if event == "feature_requested" || event == "feature_approved" || event == "feature_denied" {
			m.featureRequests.Add(ctx, 1, metric.WithAttributes(attribute.String("event", strings.TrimPrefix(event, "feature_"))))
		}
	}
}

func metricValue(data map[string]any, key string) any {
	if data == nil {
		return nil
	}
	return data[key]
}

func metricString(data map[string]any, key string) (string, bool) {
	value, exists := data[key]
	if !exists || value == nil {
		return "", false
	}
	switch value := value.(type) {
	case string:
		return boundedMetricLabel(value), true
	case json.Number:
		return boundedMetricLabel(value.String()), true
	default:
		return boundedMetricLabel(fmt.Sprint(value)), true
	}
}

func metricFloat(data map[string]any, key string) (float64, bool) {
	value, exists := data[key]
	if !exists {
		return 0, false
	}
	var result float64
	switch value := value.(type) {
	case float64:
		result = value
	case float32:
		result = float64(value)
	case json.Number:
		parsed, err := strconv.ParseFloat(value.String(), 64)
		if err != nil {
			return 0, false
		}
		result = parsed
	case string:
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return 0, false
		}
		result = parsed
	case int:
		result = float64(value)
	case int8:
		result = float64(value)
	case int16:
		result = float64(value)
	case int32:
		result = float64(value)
	case int64:
		result = float64(value)
	case uint:
		result = float64(value)
	case uint8:
		result = float64(value)
	case uint16:
		result = float64(value)
	case uint32:
		result = float64(value)
	case uint64:
		result = float64(value)
	default:
		return 0, false
	}
	return result, !math.IsNaN(result) && !math.IsInf(result, 0)
}

func metricNonNegativeInt(data map[string]any, key string, fallback int64) (int64, bool) {
	if _, exists := data[key]; !exists {
		return fallback, true
	}
	value, ok := metricInteger(data[key])
	return value, ok && value >= 0
}

func metricInteger(value any) (int64, bool) {
	switch value := value.(type) {
	case int:
		return intToMetricInt64(value)
	case int8:
		return int64(value), true
	case int16:
		return int64(value), true
	case int32:
		return int64(value), true
	case int64:
		return value, true
	case uint:
		return metricUint64(uint64(value))
	case uint8:
		return int64(value), true
	case uint16:
		return int64(value), true
	case uint32:
		return int64(value), true
	case uint64:
		return metricUint64(value)
	case json.Number:
		parsed, err := strconv.ParseInt(value.String(), 10, 64)
		if err == nil {
			return parsed, true
		}
		parsedFloat, floatErr := strconv.ParseFloat(value.String(), 64)
		if floatErr == nil && parsedFloat == math.Trunc(parsedFloat) && parsedFloat >= math.MinInt64 && parsedFloat <= math.MaxInt64 {
			return int64(parsedFloat), true
		}
		return 0, false
	case string:
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err == nil {
			return parsed, true
		}
		parsedFloat, floatErr := strconv.ParseFloat(value, 64)
		if floatErr == nil && parsedFloat == math.Trunc(parsedFloat) && parsedFloat >= math.MinInt64 && parsedFloat <= math.MaxInt64 {
			return int64(parsedFloat), true
		}
		return 0, false
	case float32:
		parsed := float64(value)
		if parsed == math.Trunc(parsed) && parsed >= math.MinInt64 && parsed <= math.MaxInt64 {
			return int64(parsed), true
		}
		return 0, false
	case float64:
		if value == math.Trunc(value) && value >= math.MinInt64 && value <= math.MaxInt64 {
			return int64(value), true
		}
		return 0, false
	default:
		return 0, false
	}
}

func metricUint64(value uint64) (int64, bool) {
	if value > uint64(maxMetricInteger) {
		return 0, false
	}
	return int64(value), true
}

func intToMetricInt64(value int) (int64, bool) {
	return int64(value), true
}

func metricStatusValue(value any) (int64, bool) {
	return metricInteger(value)
}

func toolStatus(value any) string {
	status, ok := metricStatusValue(value)
	if !ok {
		return "other_failure"
	}
	switch status {
	case 0:
		return "success"
	case 1:
		return "guest_failure"
	case 2:
		return "harness_failure"
	default:
		return "other_failure"
	}
}

func buildResult(value any) string {
	status, ok := metricStatusValue(value)
	if !ok {
		return "harness_failure"
	}
	switch status {
	case 0:
		return "success"
	case 1:
		return "guest_failure"
	default:
		return "harness_failure"
	}
}

// boundedMetricLabel keeps trusted catalog values from turning a malformed or
// unexpectedly long event into an unbounded label value. Normal model, tool,
// effort, focus, transition, and role values are preserved exactly.
func boundedMetricLabel(value string) string {
	if !utf8.ValidString(value) {
		return "invalid"
	}
	if value == "" {
		return "unknown"
	}
	if len(value) <= metricsMaxLabelBytes {
		return value
	}
	value = value[:metricsMaxLabelBytes]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	if value == "" {
		return "unknown"
	}
	return value
}

func metricRoleLabel(value string) string {
	switch value {
	case string(ActivityImplementor), string(ActivityReviewer), string(ActivityHarness):
		return value
	default:
		return "unknown"
	}
}

func metricOperatorAction(value string) string {
	switch value {
	case "agent", "abort", "continue", "rollback", "quit", "pause", "resume":
		return value
	default:
		return "other"
	}
}

func metricReviewFocus(value string) string {
	switch value {
	case "general", "correctness", "design", "security", "performance":
		return value
	default:
		return "other"
	}
}
