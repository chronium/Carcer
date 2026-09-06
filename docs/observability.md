# CodexOS observability

The harness writes the authoritative structured activity record to
`<run-directory>/events.jsonl`. Each line is one schema-versioned JSON event.
The optional `--otlp-endpoint` console argument exports metrics only, using
OTLP/HTTP at `<endpoint>/v1/metrics`.

OpenTelemetry log export is intentionally not used by the harness. A separately
managed local Grafana Alloy can discover and tail the durable JSONL files,
convert the Loki entries to OpenTelemetry logs, and forward them over OTLP/HTTP
to a configurable central Alloy ingress. For example:

```alloy
loki.source.file "codexos_events" {
  targets = [{
    __path__       = "/srv/codexos/**/events.jsonl",
    "loki.format" = "json",
    service_name   = "codexos-harness",
  }]
  forward_to = [otelcol.receiver.loki.codexos_events.receiver]

  file_match {
    enabled     = true
    sync_period = "5s"
  }
}

otelcol.receiver.loki "codexos_events" {
  output {
    logs = [otelcol.exporter.otlphttp.central.input]
  }
}

otelcol.exporter.otlphttp "central" {
  client {
    endpoint = sys.env("CODEXOS_CENTRAL_OTLP_ENDPOINT")
  }
}
```

Alloy is configured and run outside CodexOS. The harness does not start it and
does not know a Loki or Prometheus endpoint. The central Alloy is the sole
telemetry ingress and performs the internal Loki/Prometheus routing. The
harness does not depend on either Alloy instance, Loki, Prometheus, or Grafana
being available.

Model token counters use positive deltas from the app-server notification's
cumulative `tokenUsage.total` values. Repeated snapshots add nothing; malformed
or decreasing snapshots degrade observability without affecting the Codex turn
or replacing the last accepted cumulative baseline. All token counters use only
the trusted `model` and `role` attributes and the `{token}` unit.

The model-token metrics are:

* `codexos_model_input_tokens_total`: all input tokens, including cached input;
* `codexos_model_cached_input_tokens_total`: the cached subset of input;
* `codexos_model_uncached_input_tokens_total`: input not served from cache;
* `codexos_model_output_tokens_total`: all output tokens, including reasoning;
* `codexos_model_reasoning_output_tokens_total`: the reasoning subset of output.

Thus total input is cached input plus uncached input. The app-server's current
usage schema reports reasoning output as a breakdown within total output.

Representative PromQL queries:

```promql
rate(codexos_tool_calls_total[5m])
rate(codexos_builds_total[15m])
codexos_generation_state{state="running"}
```

Representative model-token panels for one selected run can use:

```promql
sum by (role, model) (
  last_over_time(codexos_model_input_tokens_total{codexos_run="$run"}[$__range])
)
sum by (role, model) (
  last_over_time(codexos_model_cached_input_tokens_total{codexos_run="$run"}[$__range])
)
sum by (role, model) (
  last_over_time(codexos_model_uncached_input_tokens_total{codexos_run="$run"}[$__range])
)
sum by (role, model) (
  last_over_time(codexos_model_output_tokens_total{codexos_run="$run"}[$__range])
)
sum by (role, model) (
  last_over_time(codexos_model_reasoning_output_tokens_total{codexos_run="$run"}[$__range])
)
```

Because each Loki line is JSON, LogQL can decode fields at query time. The
exact resource-label spelling depends on the central Alloy/Loki configuration:

```logql
{service_name="codexos-harness"} | json | event="generation_completed"
{service_name="codexos-harness"} | json | event="tool_completed" | data_tool="build"
```

Trusted build-attempt events additionally connect receipt, decoding, artifact
identity, candidate start, READY, protocol validation, final outcome, and
latest-success using one generation-scoped `build_attempt_id`. Reviewer
lifecycle events carry a `review_id`. Review-yield events bind the originating
request/call/thread/turn/phase to a stable source-snapshot identity and the one
continuation turn; `review_source_read` records a snapshot-backed source range
and exact returned-byte identity. Hashes and IDs are structured
event data for forensic correlation only; they are never metric attributes or
labels. Exact retained bytes and authoritative manifests remain in the private
run-local `build-review-provenance/` tree described in
[`validated-successors-and-provenance.md`](validated-successors-and-provenance.md).
Build provenance storage fails closed as a trusted harness failure. Review
capture remains non-interfering and records review outcome separately from
durable evidence completeness.

Serial tool dispatch records low-cardinality lifecycle evidence around the
app-server queue, guest invocation, nested host-service receipt, response
preparation, bounded write progress/completion, and the outer guest response.
Byte counts and protocol request IDs are structured event data only, never
metric labels. Host-service arguments and response contents are not recorded.

Each fresh generation also records one private run-local planning artifact under
`planning-evidence/generation-NNNN/`. Its manifest identifies the generation,
app-server thread, ordered turn attempts, outcomes, exact UTF-8 response byte
counts, and SHA-256 values. Interrupted attempts remain explicitly interrupted;
only a successful planning attempt atomically publishes the immutable final
`response.txt` without rewriting or summarization. The evidence remains available
if the generation later aborts.

Operational `planning_started`, `planning_completed`, `planning_interrupted`,
`planning_yielded`, and `planning_failed` events contain serving/turn provenance and, for a captured
response, only its byte count and digest. Plan text is excluded from JSONL event
payloads, OTLP metrics and labels, fresh-successor prompts, successor handoffs,
generation Git provenance, and public tags. Within the current generation it
remains available only as the same app-server thread's natural history and this
private trusted evidence, not as autonomous persisted state or a plan-approval
mechanism.

Turn-scoped dynamic-call routing is recorded separately from tool execution.
Queueing, result readiness, response-write attempts, and the single terminal
delivery, rejection, or orphaning outcome carry the originating JSON-RPC request,
call, thread, turn, and phase identities. Turn-terminal evidence also records any
still-pending call IDs. A response write is only an attempt; the matching
app-server `item/completed` notification is the delivery evidence. An admitted
review request instead records `tool_result_yielded`, closes the origin to later
tool admission, and never records result-ready, response-write, delivered, or
orphaned evidence. Reviewer completion and the later trusted continuation remain
separate states, and review text
is not included in operational events. A planning turn with an orphaned ordinary call is recorded
as failed and remains retryable instead of advancing to implementation. The
isolated reviewer applies the same delivery proof to its own read-only calls and
quiesces them before its process and workspace are retired.
