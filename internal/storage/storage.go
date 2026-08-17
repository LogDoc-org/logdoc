// Package storage — интерфейс хранилища логов.
// Инвариант v2: весь код работает только через Store, конкретный backend
// (ClickHouse сейчас, LogDoc Engine в будущем) — деталь реализации.
package storage

import (
	"context"

	"github.com/LogDoc-org/logdoc/internal/model"
	"github.com/LogDoc-org/logdoc/internal/query"
)

// Inserter — синхронная запись батчей (минимальная зависимость Batcher'а).
type Inserter interface {
	// InsertBatch атомарно пишет батч записей.
	InsertBatch(ctx context.Context, entries []model.Entry) error
}

// Store — полный контракт хранилища: запись + исполнение логического плана.
type Store interface {
	Inserter
	Query(ctx context.Context, p query.Plan) ([]model.Entry, error)
	RecordQueryStats(ctx context.Context, s query.Stats)
	Close() error
}
