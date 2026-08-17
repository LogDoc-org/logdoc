// Package storage — the log storage interface.
// v2 invariant: all code works only through Store; the concrete backend
// (ClickHouse now, LogDoc Engine in the future) is an implementation detail.
package storage

import (
	"context"

	"github.com/LogDoc-org/logdoc/internal/model"
	"github.com/LogDoc-org/logdoc/internal/query"
)

// Inserter — synchronous batch writes (the Batcher's minimal dependency).
type Inserter interface {
	// InsertBatch atomically writes a batch of entries.
	InsertBatch(ctx context.Context, entries []model.Entry) error
}

// Store — the full storage contract: writes + logical-plan execution.
type Store interface {
	Inserter
	Query(ctx context.Context, p query.Plan) ([]model.Entry, error)
	RecordQueryStats(ctx context.Context, s query.Stats)
	Close() error
}
