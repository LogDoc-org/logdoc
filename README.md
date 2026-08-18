# LogDoc v2

Structured-log-first platform: a single Go binary on top of ClickHouse.
Pipeline: **gather → pipe → sink → view → understand**.

> S1 (the spine): ingest HTTP/native, search, live tail, mini UI.
> S2 (the map): OTLP ingest and the Architecture Graph — a live service map
> built from logs alone. Work in progress.

## Quick start

```bash
docker compose -f deploy/docker-compose.yml up -d --build
```

UI: http://localhost:9001 · Health: `GET /healthz`

First log:

```bash
curl -X POST localhost:9001/api/v1/ingest \
  -d '[{"msg":"hello logdoc","app":"demo","lvl":"INFO","fields":{"env":"dev"}}]'
```

Search:

```bash
curl 'localhost:9001/api/v1/query?app=demo&lvl=INFO&q=hello'
```

Live tail: WebSocket `GET /api/v1/tail` (same filter parameters).

v1 compatibility: TCP/UDP `:9999` accepts the `ld_format` protocol —
existing `logdoc-go-appender` and `logback-appenders` work unchanged.

OTLP: gRPC logs on `:4317` — point any OpenTelemetry SDK or Collector at it
(`service.name`→app, body→msg, severity→lvl, attributes→fields).

## Architecture map

LogDoc builds a live service map from the logs themselves: shared
`trace_id`/`correlation_id` values and peer fields (`peer.service`, `target`,
`upstream`) turn into directed edges between services. No agents, no tracing
required — traces only refine the map.

- UI: the **Topology** tab — force-directed map, click a service or an edge
  to see its details and jump to its logs.
- API: `GET /api/v1/topology?window=5m` — nodes and edges with windowed
  rates (rps, error rate).
- Export: `GET /api/v1/topology/export?format=mermaid|markdown` — paste the
  current architecture straight into your docs.

## Development

```bash
make up      # ClickHouse for development (ports 8124/9010)
make ui      # build the frontend (ui/dist, goes into go:embed)
make build   # bin/logdoc
make test
./bin/logdoc # config: flags / env LOGDOC_* / -config logdoc.yml (see logdoc.example.yml)
```

Auth: `LOGDOC_API_KEY` (`X-API-Key` header, `Authorization: Bearer`, or `?api_key=`).
An empty key means dev mode with no auth.

## License

Apache 2.0
