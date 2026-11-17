# Instrumented Hugo

Build an extended binary from the Hugo repository:

```sh
mkdir -p /tmp/hugo-otel-bin
go build -tags extended -o /tmp/hugo-otel-bin/hugo .
```

Start a local Collector in a second terminal, from the Hugo repository:

```sh
docker run --rm -p 127.0.0.1:4318:4318 \
  -v "$PWD/scripts/otel/collector.yaml:/etc/otelcol/config.yaml:ro" \
  otel/opentelemetry-collector:0.165.0 \
  --config=/etc/otelcol/config.yaml
```

This development configuration prints all three signals. For a trace UI and
dashboards, point Hugo at your backend's OTLP/HTTP endpoint instead.

In a prepared [OpenTelemetry website checkout](https://github.com/open-telemetry/opentelemetry.io),
use the instrumented binary for its full build:

```sh
export PATH="$PWD/node_modules/.bin:$PATH"
npm run _prebuild
HUGO_OTEL_ENABLED=true \
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf \
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 \
OTEL_SERVICE_NAME=hugo \
OTEL_RESOURCE_ATTRIBUTES=project.name=opentelemetry.io \
OTEL_BSP_MAX_QUEUE_SIZE=65536 \
/tmp/hugo-otel-bin/hugo --cleanDestinationDir --logLevel info \
  -e dev --buildDrafts --buildFuture --baseURL http://localhost
npm run _hugo::post
```

This follows the website's [full build scripts](https://github.com/open-telemetry/opentelemetry.io/blob/main/package.json).
Invoke the binary directly: npm prepends `node_modules/.bin` to `PATH`, where the
website's own `hugo-extended` dependency can shadow your binary. Keep
`node_modules/.bin` on `PATH` so Hugo can find Dart Sass and other npm tools. Follow that
repository's setup instructions first; its theme, generated content, Node
dependencies, and external tools are still required.

For local Compy, activate a configuration with traces, metrics, and logs pipelines
(the shipped `debug` configuration prints all signals). Use
`OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:14318` in the command above, or get your
configured endpoint with `eval "$(compy env)"`. Keep the protocol `http/protobuf`.
Use `compy status` to confirm the listener and `compy log` to inspect received data.
Unset any `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`, `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`,
or `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` overrides so every signal uses Compy.

## Emissions

- Traces: `hugo.command.build` covers the CLI build, including configuration and
  static copying. It is a [CLI span](https://opentelemetry.io/docs/specs/semconv/cli/cli-spans/)
  with this low-cardinality name instead of `{process.executable.name}`, and carries
  `process.executable.name`, `process.executable.path`, `process.pid`, and
  `process.exit.code`. `hugo.build` contains processing, assembly, rendering, deferred
  rendering, and post-processing spans. Each page output gets a `hugo.page.render`
  span with language, kind, output format, relative content path, and template name.
  Pagination outputs are included. Template execution inherits the page's context.
  Cached-partial misses produce `hugo.partial.execute` spans with
  `hugo.partial.name`. Concurrent cache reads that overlap production produce
  `hugo.partial.cache_wait` spans under the waiting caller, linked to the producer.
  Their duration includes cache lookup and scheduling; ordinary cache hits emit
  no spans, and cache variant values are not exported. Sass compilation produces
  a `hugo.sass.transform` child span with the relative `hugo.resource.path`.
  Failed spans carry error status and `error.type`, which is the Go type of the
  error (after unwrapping `fmt.Errorf` wrappers), e.g. `*errors.errorString`.
- Metrics: `hugo.build.count`, `hugo.build.duration`, `hugo.page.render.count`, and
  `hugo.page.render.duration`. Durations use seconds and explicit histogram buckets.
  Build metrics distinguish rebuilds and failures. Page metrics use language, kind,
  output format, and failure type; paths and template names are span attributes only.
  Counts include failed attempts and count each output rendering separately.
  `hugo.template.execution.duration` (by `hugo.template.name`, inclusive of nested
  templates), `hugo.partial.cache.request.count` and `hugo.partial.cache.lookup.duration`
  (by `hugo.partial.cache.result`), and `hugo.page.content.render.duration` (by language
  and output format). Content renders lazily, so its cost also appears in whichever
  template first uses `.Content`, `.Summary`, `.WordCount` etc. With
  `--templateMetrics --templateMetricsHints`, `hugo.template.cache.potential` exports
  Hugo's output-similarity hint (0–1); it does not mean caching is safe.
- Logs: one trace-correlated completion or failure record per build. Existing Hugo
  log entries are also exported at the selected `--logLevel` and correlate with
  the CLI build span where available. Hugo's log API has no per-entry context, so
  those entries do not carry page span IDs.

Instrumentation uses the `github.com/gohugoio/hugo` scope with the Hugo version as
scope version and `service.version`, and semantic conventions schema 1.43.0.
Build-specific names use the `hugo.*` namespace; service, process, and error
attributes follow OpenTelemetry semantic conventions. This covers
the build pipeline, rather than instrumenting every template function, resource
transformation, outgoing request, or development-server HTTP request.

## Configuration

Telemetry is off unless `HUGO_OTEL_ENABLED=true`. `OTEL_SDK_DISABLED=true` overrides
it. All three signals default to OTLP when enabled; disable individual signals with
`OTEL_TRACES_EXPORTER=none`, `OTEL_METRICS_EXPORTER=none`, or `OTEL_LOGS_EXPORTER=none`.
The supported transport is OTLP/HTTP with protobuf. Unsupported exporter or protocol
settings produce a warning and leave the build uninstrumented.

The SDK/exporters honor standard resource, sampling, batch, metric interval, and
OTLP endpoint, header, timeout, compression, and TLS environment settings. Signal
endpoint overrides must include `/v1/traces`, `/v1/metrics`, or `/v1/logs`; the
shared endpoint above adds these paths automatically. Exporters default to localhost.

A trace passed in [environment variables](https://opentelemetry.io/docs/specs/otel/context/env-carriers/)
(`TRACEPARENT`, `TRACESTATE`, `BAGGAGE`, W3C format) is continued, so the build span
becomes a child of e.g. a CI job span. With the default parent-based sampler, an
unsampled parent disables tracing for the build.

Providers are scoped to a command invocation, with no global provider replacement.
Export is asynchronous and shutdown flushes all three signals, even for builds
shorter than the metric export interval. Shutdown has a five-second total deadline;
export errors warn without changing the build result. A full or partial rebuild
starts another build span with `hugo.build.rebuild=true`.

Large multilingual builds can emit many page spans. The example increases the
bounded span queue to absorb bursts; if the receiver cannot keep up, spans may be
dropped. Use `OTEL_TRACES_SAMPLER=parentbased_traceidratio` and
`OTEL_TRACES_SAMPLER_ARG=0.1` to sample whole builds at 10% while retaining metrics
and logs. Spans include relative content paths; log messages may contain filenames
and other values already present in Hugo's console output.

To compare overhead, build the same checkout with `HUGO_OTEL_ENABLED=false` and
identical caches, destinations, and build arguments.
