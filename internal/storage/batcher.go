package storage

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
)

// Batcher accumulates entries and flushes them to the Store in batches —
// by batch size or by interval, whichever comes first.
// A failed flush is retried with exponential backoff; once the attempts
// are exhausted the batch is dropped with logging (an S1 decision).
type Batcher struct {
	store    Inserter
	in       chan model.Entry
	size     int
	interval time.Duration

	maxRetries int
	backoff    time.Duration

	done chan struct{}
	once sync.Once

	mu      sync.Mutex
	dropped uint64
}

type BatcherOptions struct {
	BatchSize     int
	FlushInterval time.Duration
	MaxRetries    int           // total attempts (incl. the first); 0 → 3
	Backoff       time.Duration // base backoff; 0 → 250ms
}

func NewBatcher(store Inserter, opts BatcherOptions) *Batcher {
	if opts.BatchSize <= 0 {
		opts.BatchSize = 10000
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = time.Second
	}
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = 3
	}
	if opts.Backoff <= 0 {
		opts.Backoff = 250 * time.Millisecond
	}
	b := &Batcher{
		store:      store,
		in:         make(chan model.Entry, opts.BatchSize*4),
		size:       opts.BatchSize,
		interval:   opts.FlushInterval,
		maxRetries: opts.MaxRetries,
		backoff:    opts.Backoff,
		done:       make(chan struct{}),
	}
	go b.loop()
	return b
}

// Append enqueues an entry. Blocks if the queue is full (backpressure).
func (b *Batcher) Append(e model.Entry) {
	b.in <- e
}

// TryAppend — a non-blocking variant for self-logging: when the queue is full
// the entry is dropped (self-logs must never be able to block the batcher itself).
func (b *Batcher) TryAppend(e model.Entry) bool {
	select {
	case b.in <- e:
		return true
	default:
		return false
	}
}

// Dropped — counter of entries lost after the retries were exhausted.
func (b *Batcher) Dropped() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

// Close waits for everything accumulated to be flushed and stops the Batcher.
func (b *Batcher) Close() {
	b.once.Do(func() {
		close(b.in)
		<-b.done
	})
}

func (b *Batcher) loop() {
	defer close(b.done)

	batch := make([]model.Entry, 0, b.size)
	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		b.flushWithRetry(batch)
		batch = batch[:0]
	}

	for {
		select {
		case e, ok := <-b.in:
			if !ok {
				flush()
				return
			}
			batch = append(batch, e)
			if len(batch) >= b.size {
				flush()
				ticker.Reset(b.interval)
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (b *Batcher) flushWithRetry(batch []model.Entry) {
	delay := b.backoff
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := b.store.InsertBatch(ctx, batch)
		cancel()
		if err == nil {
			return
		}
		if attempt >= b.maxRetries {
			b.mu.Lock()
			b.dropped += uint64(len(batch))
			b.mu.Unlock()
			slog.Error("batch dropped after retries", "entries", len(batch), "attempts", attempt, "err", err)
			return
		}
		slog.Warn("batch flush failed, retrying", "attempt", attempt, "err", err)
		time.Sleep(delay)
		delay *= 2
	}
}
