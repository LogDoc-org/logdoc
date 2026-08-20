# Migrating from LogDoc v1 (community)

v2 is a ground-up rewrite: a single Go binary instead of the Java server,
ClickHouse as the only database (no PostgreSQL), Apache 2.0 all the way.

## What keeps working unchanged

- **The wire protocol.** `ld_format` over TCP/UDP is byte-compatible;
  v2 listens on `:9999` by default. Existing `logdoc-go-appender`,
  `logback-appenders` and anything else speaking the protocol reconnect
  to v2 without a change.
- **The data model.** The same seven levels (0–6), `app`/`src`/`pid`,
  arbitrary structured fields.
- **The single port.** API and UI on `:9001`, as before.

## What changed

| v1 | v2 |
|---|---|
| Java server (`logdoc/community` image) + PostgreSQL + ClickHouse | one Go binary + ClickHouse |
| Sink/pipe plugins as jars/`.so` loaded by the server | protocols and notifications built in |
| Watchdog rules with composite conditions | alert rules with nested and/or `match` conditions — `notify:` in yaml or the Rules tab in the UI |
| Users, roles, per-user tokens | built-in users with admin/member roles, JWT sessions, revocable personal tokens (Access tab); the API key stays as the bootstrap credential |
| Syslog sink plugin (jar) | built-in syslog listener (RFC 3164/5424, TCP+UDP), `ingest.syslog` in config |
| Parsing in the file-watchers agent only | server-side `pipelines:` — grok/regex/json parsing, severity mapping, rewrites and drops on ingest, whatever the source |
| — | OTLP/gRPC ingest, architecture map, Mermaid export, MCP server |

## What v2 does not have yet

Planned, prioritized by demand — open an issue if one of these blocks you:

- a plugin SDK (v2 will use gRPC subprocesses instead of in-process jars);
- journald ingest, OTLP traces and metrics;
- multi-tenant UI and fine-grained (per-app) permissions.

## Migration steps

1. Start v2 next to v1 on different ports (`LOGDOC_HTTP_ADDR`,
   `LOGDOC_NATIVE_TCP_ADDR`, ...), or on another host.
2. Point one appender at v2, verify entries in the UI.
3. Switch the rest, recreate watchdog rules as `notify:` rules — in yaml or
   right in the UI (Rules tab). Composite conditions map to `match` with
   nested `and`/`or`; pipe plugins map to channels (telegram, webhook,
   email, kafka).
4. Retire v1 once its retention window has drained.

Historical data is not migrated: log retention is TTL-bounded, so after one
retention window v2 holds everything that matters. If you do need the old
data, both systems read from ClickHouse — keep the v1 instance around
read-only until its TTL expires.
