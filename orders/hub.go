package main

import (
	"encoding/json"
	"sync"

	"github.com/go-estoria/estoria"
)

// A hub fans out updates to connected SSE clients. It is fed from two places:
// an AfterSave hook on the aggregate store (command accepted) and the outbox
// handler (read model updated), so every browser sees both halves of the CQRS
// flow as they happen.
type hub struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
}

func newHub() *hub {
	return &hub{clients: make(map[chan []byte]struct{})}
}

// subscribe registers a new client and returns its message channel.
func (h *hub) subscribe() chan []byte {
	ch := make(chan []byte, 8)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

// unsubscribe removes a client registered via subscribe.
func (h *hub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
}

// broadcast marshals v and sends it to every connected client. A client whose
// buffer is full skips the update; it will catch up on the next one.
func (h *hub) broadcast(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		estoria.GetLogger().Error("marshaling broadcast message", "error", err)
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- data:
		default:
		}
	}
}
