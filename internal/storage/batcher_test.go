package storage

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
)

type fakeStore struct {
	mu      sync.Mutex
	batches [][]model.Entry
	fails   int // how many initial calls return an error
}

func (f *fakeStore) InsertBatch(_ context.Context, entries []model.Entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails > 0 {
		f.fails--
		return errors.New("boom")
	}
	cp := make([]model.Entry, len(entries))
	copy(cp, entries)
	f.batches = append(f.batches, cp)
	return nil
}

func (f *fakeStore) Close() error { return nil }

func (f *fakeStore) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, b := range f.batches {
		n += len(b)
	}
	return n
}

func entry(msg string) model.Entry {
	return model.Entry{TenantID: model.DefaultTenant, Ts: time.Now(), App: "test", Msg: msg}
}

func TestBatcherFlushBySize(t *testing.T) {
	fs := &fakeStore{}
	b := NewBatcher(fs, BatcherOptions{BatchSize: 3, FlushInterval: time.Hour})
	for i := 0; i < 3; i++ {
		b.Append(entry("x"))
	}
	waitFor(t, func() bool { return fs.total() == 3 })
	b.Close()

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.batches) != 1 || len(fs.batches[0]) != 3 {
		t.Fatalf("expected one batch of 3 entries, got %v", fs.batches)
	}
}

func TestBatcherFlushByInterval(t *testing.T) {
	fs := &fakeStore{}
	b := NewBatcher(fs, BatcherOptions{BatchSize: 1000, FlushInterval: 20 * time.Millisecond})
	b.Append(entry("a"))
	b.Append(entry("b"))
	waitFor(t, func() bool { return fs.total() == 2 })
	b.Close()
}

func TestBatcherFlushOnClose(t *testing.T) {
	fs := &fakeStore{}
	b := NewBatcher(fs, BatcherOptions{BatchSize: 1000, FlushInterval: time.Hour})
	b.Append(entry("a"))
	b.Close() // must flush the remainder
	if fs.total() != 1 {
		t.Fatalf("expected 1 entry after Close, got %d", fs.total())
	}
}

func TestBatcherRetrySucceeds(t *testing.T) {
	fs := &fakeStore{fails: 2}
	b := NewBatcher(fs, BatcherOptions{BatchSize: 1, FlushInterval: time.Hour, MaxRetries: 3, Backoff: time.Millisecond})
	b.Append(entry("a"))
	waitFor(t, func() bool { return fs.total() == 1 })
	b.Close()
	if b.Dropped() != 0 {
		t.Fatalf("there must be no losses, dropped=%d", b.Dropped())
	}
}

func TestBatcherDropsAfterRetriesExhausted(t *testing.T) {
	fs := &fakeStore{fails: 100}
	b := NewBatcher(fs, BatcherOptions{BatchSize: 1, FlushInterval: time.Hour, MaxRetries: 2, Backoff: time.Millisecond})
	b.Append(entry("a"))
	waitFor(t, func() bool { return b.Dropped() == 1 })
	b.Close()
	if fs.total() != 0 {
		t.Fatalf("there must be no entries, got %d", fs.total())
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within the allotted time")
}
