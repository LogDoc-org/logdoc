# Contributing to LogDoc

Спасибо за интерес к проекту! Issues, вопросы и pull request'ы приветствуются.

## Окружение

Нужны: Go 1.25+, Node 22+ (для UI), Docker.

```bash
make up      # ClickHouse для разработки (порты 8124/9010)
make ui      # сборка фронтенда (ui/dist, идёт в go:embed)
make build   # bin/logdoc
make test    # юнит-тесты
make lint    # golangci-lint (или go vet, если не установлен)
```

Полный стек: `docker compose -f deploy/docker-compose.yml up -d --build`.

## Pull requests

- Небольшие сфокусированные PR проще ревьюить и быстрее мерджатся.
- Новая логика — с тестами. Особенно парсинг протоколов (`internal/ingest`)
  и генерация SQL (`internal/storage/clickhouse` — golden-тесты).
- Перед отправкой: `make test` и `make lint` должны быть зелёными.
- `ui/dist` коммитится вместе с изменениями UI (нужен для `go:embed` и CI).

## Архитектурные инварианты

Три вещи, которые не режутся ни в каком PR (детали — в коде и README):

1. `tenant_id` присутствует в схеме и во всех запросах.
2. Хранилище — только за интерфейсом `storage.Store`; никаких прямых
   обращений к ClickHouse из ingest/query/UI-кода.
3. Логический план запроса (`query.Plan`) отделён от SQL-генерации.

## Багрепорты

Указывайте версию (`GET /healthz`), способ ingest (HTTP / native TCP/UDP),
и по возможности минимальный воспроизводимый пример.

## Лицензия

Отправляя вклад, вы соглашаетесь лицензировать его под Apache 2.0.
