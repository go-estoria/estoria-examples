package main

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-estoria/estoria"
	"github.com/go-estoria/estoria/eventstore"
	"github.com/go-estoria/estoria/typeid"
	"github.com/gofrs/uuid/v5"
)

//go:embed all:web
var webFiles embed.FS

const (
	defaultStreamPageSize = 50
	defaultFeedPageSize   = 100
	maxPageSize           = 1000
)

// server serves the read-only inspector API and UI over a single backend.
//
// All stream-scoped reads go through backend.reader, which is only ever an
// eventstore.StreamReader — the inspector cannot append events by
// construction. The optional operations (stream listing, global feed) go
// through backend.caps and answer 501 when the capability is absent.
type server struct {
	backend *backend
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/info", s.handleInfo)
	mux.HandleFunc("GET /api/streams", s.handleStreams)
	mux.HandleFunc("GET /api/streams/{id}/events", s.handleStreamEvents)
	mux.HandleFunc("GET /api/all", s.handleAll)
	mux.HandleFunc("GET /api/all/tail", s.handleAllTail)

	web, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /", http.FileServerFS(web))

	return mux
}

// routesWithScanLimit is routes with a per-IP limiter in front of the
// endpoints that scan the whole store, for hosted deployments.
//
// Only /api/all/tail is metered. Everything else — stream lists, paged stream
// reads, forward feed pages — is bounded work the backend can serve all day,
// and metering it would only make a public demo feel broken.
func (s *server) routesWithScanLimit(rl *rateLimiter) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", s.routes())
	mux.Handle("GET /api/all/tail", rl.middleware(http.HandlerFunc(s.handleAllTail)))

	return mux
}

// handleInfo describes the connected backend: its label, redacted DSN, and
// which optional capabilities are available. The UI uses this to decide which
// affordances to show.
func (s *server) handleInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"backend":  s.backend.name,
		"label":    s.backend.label,
		"dsn":      redactDSN(s.backend.dsn),
		"readOnly": true,
		"capabilities": map[string]bool{
			"listStreams": s.backend.caps.listStreams != nil,
			"readAll":     s.backend.caps.readAll != nil,
		},
	})
}

// handleStreams lists the streams in the store via the listStreams
// capability, sorted by type then ID.
func (s *server) handleStreams(w http.ResponseWriter, r *http.Request) {
	if s.backend.caps.listStreams == nil {
		writeCapabilityUnavailable(w, "listStreams",
			"this backend does not support listing streams; enter a stream ID manually to inspect it")
		return
	}

	streams, err := s.backend.caps.listStreams(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	sort.Slice(streams, func(i, j int) bool {
		if streams[i].Type != streams[j].Type {
			return streams[i].Type < streams[j].Type
		}
		return streams[i].ID < streams[j].ID
	})

	if streams == nil {
		streams = []streamInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"streams": streams})
}

// handleStreamEvents pages through one stream's events using only the core
// eventstore.StreamReader — this endpoint works against ANY estoria backend.
//
// Paging follows ReadStreamOptions.AfterVersion semantics:
//
//   - Forward: events with StreamVersion > after (exclusive lower bound), so
//     the next page passes the last-seen version as after.
//   - Reverse: events with StreamVersion <= after, reading backwards;
//     after=0 starts at the latest event, and the next page passes
//     (last-seen version - 1).
func (s *server) handleStreamEvents(w http.ResponseWriter, r *http.Request) {
	streamID, err := parseStreamID(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	direction, err := parseDirection(r.URL.Query().Get("dir"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	after, count, err := parsePaging(r, defaultStreamPageSize)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()

	// Request one extra event beyond the page size: if it arrives, there is
	// another page. The extra event is trimmed before responding.
	iter, err := s.backend.reader.ReadStream(ctx, streamID, pageOptions(direction, after, count))
	if errors.Is(err, eventstore.ErrStreamNotFound) {
		// The stream doesn't exist — or this page is past its end, which the
		// core reader reports the same way. An empty page is friendlier for a
		// pager than a hard 404, so only 404 the first page.
		if after == 0 {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"error":   "stream_not_found",
				"message": fmt.Sprintf("no stream %q in this event store", streamID),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"streamId":  streamID.String(),
			"events":    []eventJSON{},
			"hasMore":   false,
			"nextAfter": after,
		})
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer iter.Close(ctx)

	events, err := collectEvents(ctx, iter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	page, hasMore, nextAfter := paginate(events, count, direction, streamCursor)

	writeJSON(w, http.StatusOK, map[string]any{
		"streamId":  streamID.String(),
		"events":    page,
		"hasMore":   hasMore,
		"nextAfter": nextAfter,
	})
}

// handleAll pages forward through the global event feed.
//
// Global reads are forward-only by contract: ReadAllOptions carries an
// exclusive AfterPosition and no direction. That is deliberate — the stable
// prefix that makes a position a resumable checkpoint is only defined going
// forward, and it is exactly how a projection consumes a store.
func (s *server) handleAll(w http.ResponseWriter, r *http.Request) {
	if s.backend.caps.readAll == nil {
		writeCapabilityUnavailable(w, "readAll",
			"this backend does not support reading all events in global order")
		return
	}

	after, count, err := parsePaging(r, defaultFeedPageSize)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()

	// Unlike ReadStream, ReadAll yields an empty iterator (not an error) when
	// nothing matches — "no events yet" is a valid state for a global feed,
	// and it is what makes tail polling with after=<latest position> cheap.
	iter, err := s.backend.caps.readAll(ctx, eventstore.ReadAllOptions{
		AfterPosition: after,
		Count:         count + 1, // one extra: detect a further page without a second query
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer iter.Close(ctx)

	events, err := collectEvents(ctx, iter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	page, hasMore, nextAfter := paginate(events, count, eventstore.Forward, globalCursor)
	if len(page) == 0 {
		nextAfter = after
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"events":    page,
		"hasMore":   hasMore,
		"nextAfter": nextAfter,
	})
}

// handleAllTail returns the most recent events in the store, so the feed can
// open on current activity rather than on the beginning of history.
//
// This costs a full forward scan, because a forward-only reader cannot seek
// from the end. That is the honest price of the contract, and it is why this
// is a separate endpoint the UI calls once rather than the feed's paging
// mechanism: only the last count events are retained, in a ring, so the scan
// is O(events) in time but O(count) in memory.
func (s *server) handleAllTail(w http.ResponseWriter, r *http.Request) {
	if s.backend.caps.readAll == nil {
		writeCapabilityUnavailable(w, "readAll",
			"this backend does not support reading all events in global order")
		return
	}

	_, count, err := parsePaging(r, defaultFeedPageSize)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()

	iter, err := s.backend.caps.readAll(ctx, eventstore.ReadAllOptions{})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer iter.Close(ctx)

	all, err := collectEvents(ctx, iter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	tail := all
	if int64(len(tail)) > count {
		tail = tail[int64(len(tail))-count:]
	}

	// The frontier this read observed: where the UI resumes tailing from.
	nextAfter := int64(0)
	if len(all) > 0 {
		nextAfter = globalCursor(all[len(all)-1])
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"events":    tail,
		"total":     len(all),
		"nextAfter": nextAfter,
	})
}

// ---- paging ----

// pageOptions builds the read options for one page: it always requests
// count+1 events so the presence of a further page can be detected without a
// second query.
func pageOptions(direction eventstore.ReadStreamDirection, after, count int64) eventstore.ReadStreamOptions {
	return eventstore.ReadStreamOptions{
		AfterVersion: after,
		Count:        count + 1,
		Direction:    direction,
	}
}

// A cursorFunc extracts the paging cursor from an event: the stream version
// for stream reads, the global position for feed reads.
type cursorFunc func(evt eventJSON) int64

func streamCursor(evt eventJSON) int64 { return evt.Version }

func globalCursor(evt eventJSON) int64 {
	// Both contrib backends always populate GlobalPosition; guard anyway so a
	// hypothetical backend without positions degrades to "no next page"
	// rather than a panic.
	if evt.GlobalPosition == nil {
		return 0
	}
	return *evt.GlobalPosition
}

// paginate trims the count+1 lookahead event and derives the pager state.
// nextAfter is the value to pass as "after" for the following page:
//
//   - forward: the cursor of the last event on this page (exclusive bound)
//   - reverse: one less than it (the <= bound must exclude what was seen)
func paginate(events []eventJSON, count int64, direction eventstore.ReadStreamDirection, cursor cursorFunc) (page []eventJSON, hasMore bool, nextAfter int64) {
	page = events
	if int64(len(page)) > count {
		page = page[:count]
		hasMore = true
	}

	if len(page) > 0 {
		nextAfter = cursor(page[len(page)-1])
		if direction == eventstore.Reverse {
			nextAfter--
		}
	}

	return page, hasMore, nextAfter
}

// parsePaging reads the after/count query parameters.
func parsePaging(r *http.Request, defaultCount int64) (after, count int64, err error) {
	count = defaultCount

	if v := r.URL.Query().Get("after"); v != "" {
		after, err = strconv.ParseInt(v, 10, 64)
		if err != nil || after < 0 {
			return 0, 0, errors.New("after must be a non-negative integer")
		}
	}

	if v := r.URL.Query().Get("count"); v != "" {
		count, err = strconv.ParseInt(v, 10, 64)
		if err != nil || count < 1 || count > maxPageSize {
			return 0, 0, fmt.Errorf("count must be between 1 and %d", maxPageSize)
		}
	}

	return after, count, nil
}

// parseDirection maps the dir query parameter onto core read directions.
func parseDirection(dir string) (eventstore.ReadStreamDirection, error) {
	switch dir {
	case "", "forward":
		return eventstore.Forward, nil
	case "reverse":
		return eventstore.Reverse, nil
	default:
		return 0, fmt.Errorf("dir must be %q or %q", "forward", "reverse")
	}
}

// ---- stream ID parsing ----

// parseStreamID parses a user-supplied stream ID of the form "type_uuid"
// (typeid.ID.String() format) into a typeid.ID.
//
// The ID is split on the FIRST underscore. This assumes the type name itself
// contains no underscore, which holds for every estoria example; UUIDs use
// hyphens, never underscores, so the remainder is unambiguous. A type name
// containing an underscore would need a different separator strategy.
func parseStreamID(s string) (typeid.ID, error) {
	idx := strings.Index(s, "_")
	if idx <= 0 {
		return typeid.ID{}, fmt.Errorf("invalid stream ID %q: expected the form type_uuid", s)
	}

	uid, err := uuid.FromString(s[idx+1:])
	if err != nil {
		return typeid.ID{}, fmt.Errorf("invalid stream ID %q: %q is not a UUID", s, s[idx+1:])
	}

	return typeid.New(s[:idx], uid), nil
}

// ---- event serialization ----

// eventJSON is the wire shape of one event, shared by the stream and global
// feed endpoints.
type eventJSON struct {
	StreamID       string            `json:"streamId"`
	EventID        string            `json:"eventId"`
	EventType      string            `json:"eventType"`
	Version        int64             `json:"version"`
	GlobalPosition *int64            `json:"globalPosition"`
	Timestamp      time.Time         `json:"timestamp"`
	Data           json.RawMessage   `json:"data"`
	DataEncoding   string            `json:"dataEncoding"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

// collectEvents drains a stream iterator into wire-shaped events. The caller
// owns closing the iterator.
func collectEvents(ctx context.Context, iter eventstore.StreamIterator) ([]eventJSON, error) {
	events := []eventJSON{}
	for {
		evt, err := iter.Next(ctx)
		if errors.Is(err, eventstore.ErrEndOfEventStream) {
			return events, nil
		} else if err != nil {
			return nil, fmt.Errorf("reading event: %w", err)
		}

		data, encoding := encodeEventData(evt.Data)
		events = append(events, eventJSON{
			StreamID:       evt.StreamID.String(),
			EventID:        evt.ID.String(),
			EventType:      evt.ID.Type, // the event type is the typeid's type component
			Version:        evt.StreamVersion,
			GlobalPosition: evt.GlobalPosition, // nil when the backend has no global ordering
			Timestamp:      evt.Timestamp,
			Data:           data,
			DataEncoding:   encoding,
			Metadata:       evt.Metadata,
		})
	}
}

// encodeEventData passes event payloads through as raw JSON when they are
// valid JSON (which all estoria examples produce), and otherwise base64-encodes
// them and flags the encoding so the UI can present them honestly.
func encodeEventData(data []byte) (json.RawMessage, string) {
	if len(data) == 0 {
		return json.RawMessage("null"), "empty"
	}
	if json.Valid(data) {
		return json.RawMessage(data), "json"
	}

	encoded, _ := json.Marshal(base64.StdEncoding.EncodeToString(data))
	return encoded, "base64"
}

// ---- response helpers ----

func writeCapabilityUnavailable(w http.ResponseWriter, capability, message string) {
	writeJSON(w, http.StatusNotImplemented, map[string]any{
		"error":      "capability_unavailable",
		"capability": capability,
		"message":    message,
	})
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
