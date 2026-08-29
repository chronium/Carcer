# CodexOS observability

The harness writes the authoritative structured activity record to
`<run-directory>/events.jsonl`. Each line is one schema-versioned JSON event.
The optional `--otlp-endpoint` console argument exports metrics only, using
OTLP/HTTP at `<endpoint>/v1/metrics`.

OpenTelemetry log export is intentionally not used by Python. A separately
managed local Grafana Alloy can tail the durable JSONL file and forward it over
OTLP/HTTP to a configurable central Alloy ingress. (`otelcol.receiver.filelog`
currently requires Alloy's `public-preview` stability level.) For example:

```alloy
otelcol.storage.file "codexos_events" {}

otelcol.receiver.filelog "codexos_events" {
  include  = ["/srv/codexos/run/events.jsonl"]
  storage  = otelcol.storage.file.codexos_events.handler
  resource = {
    "service.name" = "codexos-harness",
    "codexos.run"  = sys.env("CODEXOS_RUN_ID"),
  }
  operators = [{
    type = "json_parser",
  }]

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
or decreasing snapshots degrade observability without affecting the Codex turn.

Representative PromQL queries:

```promql
rate(codexos_tool_calls_total[5m])
rate(codexos_builds_total[15m])
codexos_generation_state{state="running"}
```

Because each Loki line is JSON, LogQL can decode fields at query time. The
exact resource-label spelling depends on the central Alloy/Loki configuration:

```logql
{service_name="codexos-harness"} | json | event="generation_completed"
{service_name="codexos-harness"} | json | event="tool_completed" | data_tool="build"
```
