# Getting started

From zero to the first log in under five minutes, then the map and the agent.

## 1. Run the stack

Requirements: Docker with the compose plugin.

```bash
git clone https://github.com/LogDoc-org/logdoc && cd logdoc
docker compose -f deploy/docker-compose.yml up -d --build
```

This starts ClickHouse and the LogDoc binary. Open http://localhost:9001 —
the UI is served from the same port as the API.

| Port | What |
|---|---|
| 9001 | HTTP API + UI + MCP |
| 9999/tcp, 9999/udp | native `ld_format` (v1 appenders) |
| 4317 | OTLP/gRPC logs |
| 5140/tcp, 5140/udp | syslog (off by default, see below) |

## 2. Send the first log

```bash
curl -X POST localhost:9001/api/v1/ingest \
  -d '[{"msg":"hello logdoc","app":"demo","lvl":"INFO","fields":{"env":"dev"}}]'
```

Refresh the UI — the entry is there. Levels are the v1 seven:
`DEBUG INFO LOG WARN ERROR SEVERE PANIC` (or digits 0–6).

## 3. Connect your services

Pick whichever fits your stack; all of them can run at the same time.

**HTTP JSON** — anything that can POST:

```bash
POST /api/v1/ingest
[{"msg":"...","app":"my-service","src":"http","lvl":"ERROR","fields":{"user":"u1"}}]
```

**OpenTelemetry** — point an OTLP exporter (SDK or Collector) at
`localhost:4317`, logs over gRPC. Mapping: `service.name` → app,
body → msg, severity → lvl, attributes → fields.

**LogDoc v1 appenders** — [`logdoc-go-appender`](https://github.com/LogDoc-org/logdoc-go-appender)
(logrus/zap) and [`logback-appenders`](https://github.com/LogDoc-org/logback-appenders)
(Java) speak the native protocol on `:9999` unchanged.

**Syslog** — routers, NAS boxes, legacy daemons. RFC 3164 and RFC 5424 are
auto-detected; TCP accepts both newline-delimited and octet-counting
(RFC 6587) framing. Off by default; enable with:

```bash
LOGDOC_SYSLOG_UDP_ADDR=:5140 LOGDOC_SYSLOG_TCP_ADDR=:5140 ./logdoc
```

Try it: `logger -n localhost -P 5140 -d "hello from syslog"`. Facility and
severity map to fields/levels; entries arrive as app from the syslog tag,
`src` = `syslog.<facility>.<app>`. For rsyslog forwarding:

```
# /etc/rsyslog.d/90-logdoc.conf
*.* action(type="omfwd" target="logdoc-host" port="5140" protocol="tcp")
```

## 4. Search and tail

The **Logs** tab: query by app, message substring, levels, period presets,
click any app/source/field value to filter by it. Live tail streams over
WebSocket and pauses when you scroll down.

The same over HTTP:

```bash
curl 'localhost:9001/api/v1/query?app=demo&lvl=ERROR,SEVERE&q=timeout&from=2026-08-19T00:00:00Z&limit=100'
```

## 5. Structure raw logs (pipelines)

If a service emits raw text (nginx, anything legacy), server-side pipelines
parse it on ingest — before search, tail, the map and alerts see the entry.
In `logdoc.yml`:

```yaml
pipelines:
  - name: nginx access
    when: { app: nginx }
    steps:
      - grok: '%{COMBINEDAPACHELOG} upstream=%{WORD:upstream}'
      - severity:
          from: response
          rules:
            - { prefix: "5", lvl: ERROR }
            - { prefix: "4", lvl: WARN }
            - { lvl: INFO }
      - set:
          fields: { peer.service: $upstream }
```

One raw access line in — a structured entry out: `clientip`, `request`,
`response` and friends become searchable fields, a `502` becomes a real
`ERROR`, and `peer.service` puts the nginx→upstream edge on the map. Other
steps: `regex` (RE2 named groups), `json` (JSON messages → fields), `drop`
(discard noise like health checks). See the `pipelines` section of
[`logdoc.example.yml`](../logdoc.example.yml).

## 6. The map

Send logs from two or more services that share a `trace_id` (or
`correlation_id`, or peer fields like `peer.service`) and open the
**Topology** tab: the services connect themselves into a directed map with
per-edge rates. Click an edge to see the live logs of that connection.

No services of your own yet? Inject the demo:

```bash
deploy/demo-traffic.sh     # three services, one calls another
deploy/demo-incident.sh    # adds a database failure cascading through them
```

Deploys are detected from the logs too: log a `version` field (or a message
like `deploy billing 2.3.1`) and the version change appears on the service
card in the Topology tab — next to whatever happened right after it.

The **Changes** toggle on the map answers "what changed?": new and silent
services and links, error-rate jumps versus the previous window, and deploys.
The same over HTTP:

```bash
curl 'localhost:9001/api/v1/topology/diff?window=1h'
```

Export the current architecture for your docs:

```bash
curl 'localhost:9001/api/v1/topology/export?format=mermaid'
```

## 7. Let an agent in (MCP)

LogDoc is an MCP server — `query_logs`, `get_topology`, `get_topology_diff`,
`get_service_card` over Streamable HTTP:

```bash
claude mcp add --transport http logdoc http://localhost:9001/mcp \
  --header "X-API-Key: <your key>"
```

Then ask: *"why is checkout failing?"* — the agent walks the map, follows
the error edges and reads the logs itself.

## 8. Alerts

Two rule kinds evaluated on the live stream: an error burst and a service
going silent. An error rule can carry a composite `match` condition — nested
`and`/`or` over app/level/message/fields, with substring, prefix/suffix and
regex operators. Delivery: Telegram, webhook, email, Kafka. See the `notify`
section of [`logdoc.example.yml`](../logdoc.example.yml).

Once at least one channel is configured, manage rules from the **Rules** tab
in the UI — every rule with live fire counters, create/edit/delete at runtime
— or over the API:

```bash
curl -X POST localhost:9001/api/v1/notify/rules -d '{
  "name": "billing cascade", "type": "error_threshold",
  "threshold": 2, "window": "1m",
  "match": {"app": "billing",
            "or": [{"lvl": "ERROR"}, {"msg": {"contains": "pool exhausted"}}]}
}'
```

## 9. Add your team

Out of the box everything is open — fine on a laptop. To lock it down, set
`LOGDOC_API_KEY` (the bootstrap admin credential), open the UI, sign in with
the key and create accounts on the **Access** tab: login, password, role.
**member** can search, tail and browse the map; **admin** additionally
manages notification rules and users. Each user issues personal `ldt_...`
tokens there for scripts and CI — revocable one by one, carrying the user's
role. Creating the first user turns auth on across the whole surface (API,
UI, OTLP); the bootstrap key stays valid as the recovery path.

```bash
# the same over the API
curl -X POST localhost:9001/api/v1/users -H "X-API-Key: $LOGDOC_API_KEY" \
  -d '{"login": "alice", "password": "correct horse", "role": "admin"}'
```

## Configuration

Precedence: defaults → yaml (`-config` / `LOGDOC_CONFIG`) → env `LOGDOC_*` →
flags. Every option with its default lives in
[`logdoc.example.yml`](../logdoc.example.yml).

Auth: any credential — API key, personal `ldt_...` token or session JWT — is
passed as `X-API-Key`, `Authorization: Bearer` or `?api_key=`. An empty key
with no users = open dev mode.

Retention: `clickhouse.ttl_days` (default 30).
