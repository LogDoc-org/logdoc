# Contributing to LogDoc

Thanks for your interest in the project! Issues, questions, and pull requests are welcome.

## Environment

You need: Go 1.25+, Node 22+ (for the UI), Docker.

```bash
make up      # ClickHouse for development (ports 8124/9010)
make ui      # build the frontend (ui/dist, goes into go:embed)
make build   # bin/logdoc
make test    # unit tests
make lint    # golangci-lint (or go vet if not installed)
```

Full stack: `docker compose -f deploy/docker-compose.yml up -d --build`.

## Pull requests

- Small, focused PRs are easier to review and get merged faster.
- New logic comes with tests. Especially protocol parsing (`internal/ingest`)
  and SQL generation (`internal/storage/clickhouse` — golden tests).
- Before submitting: `make test` and `make lint` must be green.
- `ui/dist` is committed together with UI changes (needed for `go:embed` and CI).

## Architectural invariants

Three things no PR may cut (details in the code and README):

1. `tenant_id` is present in the schema and in every query.
2. Storage is accessed only through the `storage.Store` interface; no direct
   ClickHouse calls from ingest/query/UI code.
3. The logical query plan (`query.Plan`) is kept separate from SQL generation.

## Bug reports

Include the version (`GET /healthz`), the ingest path (HTTP / native TCP/UDP),
and, if possible, a minimal reproducible example.

## License

By submitting a contribution, you agree to license it under Apache 2.0.
