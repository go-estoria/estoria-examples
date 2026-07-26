package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-estoria/estoria"
	pgeventstore "github.com/go-estoria/estoria-contrib/postgres/eventstore"
	"github.com/go-estoria/estoria/aggregatestore"
	"github.com/go-estoria/estoria/eventstore"
	"github.com/go-estoria/estoria/eventstore/projection"
	"github.com/go-estoria/estoria/typeid"
	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed all:web
var webFiles embed.FS

type server struct {
	// orders is the decorated store (hooks -> event-sourced) used for saving
	// commands and for single-order detail reads.
	orders aggregatestore.Store[Order]

	// events is the raw event store, used for stream-level reads (the order
	// timeline) that don't need an aggregate.
	events *pgeventstore.EventStore

	// readModel serves all list-shaped queries. It is populated exclusively
	// by the outbox processor — the HTTP layer never writes to it.
	readModel *readModel

	// pool is the underlying connection pool, used only by the demo reset
	// (see demo.go) to clear storage directly.
	pool *pgxpool.Pool

	// resetMu is held for writing while the demo reset clears the database,
	// and for reading while a command runs. It is uncontended in normal
	// operation: without -hourly-reset nothing ever takes the write side.
	resetMu sync.RWMutex

	hub *hub
	log *deliveryLog
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/orders", s.handleListOrders)
	mux.HandleFunc("POST /api/orders", s.handleCreateOrder)
	mux.HandleFunc("GET /api/orders/{id}", s.handleGetOrder)
	mux.HandleFunc("POST /api/orders/{id}/pay", s.handlePay)
	mux.HandleFunc("POST /api/orders/{id}/pick", s.handlePick)
	mux.HandleFunc("POST /api/orders/{id}/ship", s.handleShip)
	mux.HandleFunc("POST /api/orders/{id}/deliver", s.handleDeliver)
	mux.HandleFunc("POST /api/orders/{id}/cancel", s.handleCancel)
	mux.HandleFunc("GET /api/outbox", s.handleOutbox)
	mux.HandleFunc("GET /api/watch", s.handleWatch)

	web, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /", http.FileServerFS(web))

	return mux
}

// orderMessage is the SSE payload for a saved command: the write side has
// advanced. The read model hasn't necessarily caught up yet — that's what the
// "delivery" messages announce.
type orderMessage struct {
	Type    string `json:"type"`
	Version int64  `json:"version"`
	Order   Order  `json:"order"`
}

// handleListOrders serves the order list and header badge counts FROM THE
// READ MODEL. No aggregates are loaded here; a just-saved command won't
// appear until the outbox processor delivers its events.
func (s *server) handleListOrders(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	orders, err := s.readModel.list(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	counts, err := s.readModel.statusCounts(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"orders": orders, "counts": counts})
}

// handleCreateOrder places a demo order: a random customer buying random
// catalog items. One OrderPlaced event starts a brand-new stream.
func (s *server) handleCreateOrder(w http.ResponseWriter, r *http.Request) {
	s.resetMu.RLock()
	defer s.resetMu.RUnlock()

	agg := s.orders.New(typeid.NewV7("order").UUID)

	if err := agg.Append(randomOrder()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := s.orders.Save(r.Context(), agg, nil); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":      agg.Entity().ID,
		"version": agg.Version(),
	})
}

// handleGetOrder loads the full aggregate (the current entity plus its
// version) and the raw event stream rendered as a human-readable timeline.
// This is the query that justifies event sourcing: the list shows what an
// order is; the timeline shows how it got there.
func (s *server) handleGetOrder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	id, err := uuid.FromString(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid order ID")
		return
	}

	agg, err := s.orders.Load(ctx, id, nil)
	if err != nil {
		s.writeLoadError(w, err)
		return
	}

	timeline, err := s.orderTimeline(ctx, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"version":  agg.Version(),
		"order":    agg.Entity(),
		"timeline": timeline,
	})
}

// A commandFunc validates a command against the order state it is based on
// and returns the resulting event.
type commandFunc func(order Order) (estoria.EntityEvent[Order], error)

// runCommand is the write path shared by all fulfillment commands:
//
//  1. Load the order at the version the client last saw (baseVersion). When
//     the stream has advanced past it, saving will fail the ExpectVersion
//     check — real optimistic concurrency, not a simulated check.
//  2. Validate the command against that state and derive an event.
//  3. Append the event and save. The save commits the event AND its outbox
//     row in one transaction. On a version conflict, respond 409 so the
//     client can refresh and retry.
func (s *server) runCommand(w http.ResponseWriter, r *http.Request, baseVersion int64, cmd commandFunc) {
	// Held for the whole load-validate-save cycle so a demo reset can't clear
	// the stream out from under it. Uncontended unless -hourly-reset is on.
	s.resetMu.RLock()
	defer s.resetMu.RUnlock()

	ctx := r.Context()

	id, err := uuid.FromString(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid order ID")
		return
	}

	var opts *aggregatestore.LoadOptions
	if baseVersion > 0 {
		opts = &aggregatestore.LoadOptions{ToVersion: baseVersion}
	}

	agg, err := s.orders.Load(ctx, id, opts)
	if err != nil {
		s.writeLoadError(w, err)
		return
	}

	event, err := cmd(agg.Entity())
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	if err := agg.Append(event); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := s.orders.Save(ctx, agg, nil); err != nil {
		var mismatch eventstore.StreamVersionMismatchError
		if errors.As(err, &mismatch) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":           "version_conflict",
				"expectedVersion": mismatch.ExpectedVersion,
				"actualVersion":   mismatch.ActualVersion,
				"message": fmt.Sprintf(
					"the order has changed since version %d (it is now at version %d)",
					mismatch.ExpectedVersion, mismatch.ActualVersion),
			})
			return
		}

		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"version": agg.Version()})
}

// baseVersionRequest is the JSON body shared by all fulfillment commands.
type baseVersionRequest struct {
	BaseVersion int64 `json:"baseVersion"`
}

func (s *server) handlePay(w http.ResponseWriter, r *http.Request) {
	req, err := readJSON[baseVersionRequest](r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.runCommand(w, r, req.BaseVersion, func(o Order) (estoria.EntityEvent[Order], error) {
		if o.Status != StatusPlaced {
			return nil, fmt.Errorf("cannot pay an order in status %q", o.Status)
		}
		return randomPayment(), nil
	})
}

func (s *server) handlePick(w http.ResponseWriter, r *http.Request) {
	req, err := readJSON[baseVersionRequest](r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.runCommand(w, r, req.BaseVersion, func(o Order) (estoria.EntityEvent[Order], error) {
		if o.Status != StatusPaid {
			return nil, fmt.Errorf("cannot pick an order in status %q", o.Status)
		}
		return OrderPicked{}, nil
	})
}

func (s *server) handleShip(w http.ResponseWriter, r *http.Request) {
	req, err := readJSON[baseVersionRequest](r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.runCommand(w, r, req.BaseVersion, func(o Order) (estoria.EntityEvent[Order], error) {
		if o.Status != StatusPicked {
			return nil, fmt.Errorf("cannot ship an order in status %q", o.Status)
		}
		return randomShipment(), nil
	})
}

func (s *server) handleDeliver(w http.ResponseWriter, r *http.Request) {
	req, err := readJSON[baseVersionRequest](r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.runCommand(w, r, req.BaseVersion, func(o Order) (estoria.EntityEvent[Order], error) {
		if o.Status != StatusShipped {
			return nil, fmt.Errorf("cannot deliver an order in status %q", o.Status)
		}
		return OrderDelivered{}, nil
	})
}

func (s *server) handleCancel(w http.ResponseWriter, r *http.Request) {
	req, err := readJSON[struct {
		BaseVersion int64  `json:"baseVersion"`
		Reason      string `json:"reason"`
	}](r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.runCommand(w, r, req.BaseVersion, func(o Order) (estoria.EntityEvent[Order], error) {
		switch o.Status {
		case StatusPlaced, StatusPaid, StatusPicked:
			reason := strings.TrimSpace(req.Reason)
			if reason == "" {
				reason = "customer request"
			}
			return OrderCancelled{Reason: reason}, nil
		default:
			return nil, fmt.Errorf("cannot cancel an order in status %q", o.Status)
		}
	})
}

// timelineEntry is one row in an order's timeline: a stream event rendered as
// a human-readable description.
type timelineEntry struct {
	Version     int64     `json:"version"`
	Type        string    `json:"type"`
	Timestamp   time.Time `json:"timestamp"`
	Description string    `json:"description"`
}

// orderTimeline projects an order's event stream into readable history using
// a raw stream read — no aggregate hydration, just events.
func (s *server) orderTimeline(ctx context.Context, id uuid.UUID) ([]timelineEntry, error) {
	iter, err := s.events.ReadStream(ctx, typeid.New("order", id), eventstore.ReadStreamOptions{})
	if errors.Is(err, eventstore.ErrStreamNotFound) {
		return []timelineEntry{}, nil
	} else if err != nil {
		return nil, err
	}
	defer iter.Close(ctx)

	proj, err := projection.New(iter)
	if err != nil {
		return nil, err
	}

	entries := []timelineEntry{}
	if _, err := proj.Project(ctx, projection.EventHandlerFunc(func(_ context.Context, evt *eventstore.Event) error {
		entries = append(entries, timelineEntry{
			Version:     evt.StreamVersion,
			Type:        evt.ID.Type,
			Timestamp:   evt.Timestamp,
			Description: describeEvent(evt),
		})
		return nil
	})); err != nil {
		return nil, err
	}

	return entries, nil
}

// describeEvent renders a stream event as prose for the timeline.
func describeEvent(evt *eventstore.Event) string {
	unmarshal := func(dst any) bool { return json.Unmarshal(evt.Data, dst) == nil }

	switch evt.ID.Type {
	case OrderPlaced{}.EventType():
		var e OrderPlaced
		if unmarshal(&e) {
			units := 0
			var total int64
			for _, item := range e.Items {
				units += item.Qty
				total += int64(item.Qty) * item.PriceCents
			}
			return fmt.Sprintf("placed by %s — %d %s, %s",
				e.Customer, units, plural(units, "item"), fmtMoney(total))
		}
	case OrderPaid{}.EventType():
		var e OrderPaid
		if unmarshal(&e) {
			return fmt.Sprintf("paid via %s", e.Method)
		}
	case OrderPicked{}.EventType():
		return "picked and packed at the warehouse"
	case OrderShipped{}.EventType():
		var e OrderShipped
		if unmarshal(&e) {
			return fmt.Sprintf("shipped via %s (tracking %s)", e.Carrier, e.Tracking)
		}
	case OrderDelivered{}.EventType():
		return "delivered to the customer"
	case OrderCancelled{}.EventType():
		var e OrderCancelled
		if unmarshal(&e) {
			return fmt.Sprintf("cancelled: %s", e.Reason)
		}
	}

	return evt.ID.Type
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// handleOutbox serves the monitor panel: how many outbox rows are awaiting
// delivery (straight from the outbox table) and the recent webhook log.
func (s *server) handleOutbox(w http.ResponseWriter, r *http.Request) {
	pending, err := s.readModel.pendingOutbox(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"pending":    pending,
		"deliveries": s.log.recent(),
	})
}

// handleWatch streams updates to the client over server-sent events: "order"
// messages when a command is saved, "delivery" messages when the outbox
// processor lands an event in the read model.
func (s *server) handleWatch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	rc := http.NewResponseController(w)

	ch, ok := s.hub.subscribe()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "too many live connections right now — try again shortly")
		return
	}
	defer s.hub.unsubscribe(ch)

	// an initial comment forces headers out so the client sees the stream
	// open immediately (and refetches state in its onopen handler)
	fmt.Fprint(w, ": connected\n\n")
	if err := rc.Flush(); err != nil {
		return
	}

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}

func (s *server) writeLoadError(w http.ResponseWriter, err error) {
	if errors.Is(err, aggregatestore.ErrAggregateNotFound) {
		writeError(w, http.StatusNotFound, "order not found")
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

func readJSON[T any](r *http.Request) (T, error) {
	var v T
	err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&v)
	return v, err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		estoria.GetLogger().Error("encoding response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
