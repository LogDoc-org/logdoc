package tail

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/LogDoc-org/logdoc/internal/query"
)

// NewWSHandler — handler for GET /api/v1/tail: a WebSocket stream of entries
// filtered by the same parameters as /api/v1/query (except time/limit).
func NewWSHandler(hub *Hub) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plan, err := query.ParsePlan(r.URL.Query())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			slog.Warn("tail: websocket accept", "err", err)
			return
		}
		defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

		entries, cancel := hub.Subscribe(plan)
		defer cancel()

		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-entries:
				if !ok {
					return
				}
				payload, err := json.Marshal(query.ToDTO(e))
				if err != nil {
					continue
				}
				writeCtx, cancelWrite := context.WithTimeout(ctx, 10*time.Second)
				err = conn.Write(writeCtx, websocket.MessageText, payload)
				cancelWrite()
				if err != nil {
					return // client went away
				}
			}
		}
	})
}
