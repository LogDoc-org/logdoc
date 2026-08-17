// Package tail — live-поток записей: fan-out от ingest к WebSocket-подписчикам.
package tail

import (
	"sync"

	"github.com/LogDoc-org/logdoc/internal/model"
	"github.com/LogDoc-org/logdoc/internal/query"
)

const subscriberBuffer = 1024

// Hub рассылает входящие записи подписчикам с фильтрацией по плану.
// Реализует ingest.Appender. Медленный подписчик теряет записи
// (drop), но не тормозит ingest.
type Hub struct {
	mu   sync.RWMutex
	subs map[*subscriber]struct{}
}

type subscriber struct {
	plan query.Plan
	ch   chan model.Entry
}

func NewHub() *Hub {
	return &Hub{subs: make(map[*subscriber]struct{})}
}

func (h *Hub) Append(e model.Entry) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for s := range h.subs {
		if !s.plan.Matches(e) {
			continue
		}
		select {
		case s.ch <- e:
		default: // подписчик не успевает — запись пропускается
		}
	}
}

// Subscribe регистрирует подписчика; cancel обязателен и идемпотентен.
func (h *Hub) Subscribe(p query.Plan) (<-chan model.Entry, func()) {
	s := &subscriber{plan: p, ch: make(chan model.Entry, subscriberBuffer)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, s)
			h.mu.Unlock()
			close(s.ch)
		})
	}
	return s.ch, cancel
}
