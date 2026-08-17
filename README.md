# LogDoc v2

Structured-log-first платформа: один Go-бинарь поверх ClickHouse.
Конвейер: **gather → pipe → sink → view → understand**.

> S1 (хребет): ingest HTTP/native, поиск, live tail, мини-UI. В работе.

## Быстрый старт

```bash
docker compose -f deploy/docker-compose.yml up -d --build
```

UI: http://localhost:9001 · Health: `GET /healthz`

Первый лог:

```bash
curl -X POST localhost:9001/api/v1/ingest \
  -d '[{"msg":"hello logdoc","app":"demo","lvl":"INFO","fields":{"env":"dev"}}]'
```

Поиск:

```bash
curl 'localhost:9001/api/v1/query?app=demo&lvl=INFO&q=hello'
```

Live tail: WebSocket `GET /api/v1/tail` (те же параметры фильтров).

Совместимость с v1: TCP/UDP `:9999` принимает протокол `ld_format` —
существующие `logdoc-go-appender` и `logback-appenders` работают без изменений.

## Разработка

```bash
make up      # ClickHouse для разработки (порты 8124/9010)
make ui      # сборка фронтенда (ui/dist, идёт в go:embed)
make build   # bin/logdoc
make test
./bin/logdoc # конфиг: флаги / env LOGDOC_* / -config logdoc.yml (см. logdoc.example.yml)
```

Авторизация: `LOGDOC_API_KEY` (заголовок `X-API-Key`, `Authorization: Bearer` или `?api_key=`).
Пустой ключ — dev-режим без авторизации.

## Лицензия

Apache 2.0
