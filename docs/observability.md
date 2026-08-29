# CodexOS observability

The harness writes the authoritative structured activity record to
`<run-directory>/events.jsonl`. Each line is one schema-versioned JSON event.
The optional `--otlp-endpoint` console argument exports metrics only, using
OTLP/HTTP at `<endpoint>/v1/metrics`.

OpenTelemetry log export is intentionally not used by Python. A separately
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
