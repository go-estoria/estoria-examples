package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/allegro/bigcache/v3"
	"github.com/go-estoria/estoria"
	sqlstore "github.com/go-estoria/estoria-contrib/sqlite/eventstore"
	"github.com/go-estoria/estoria/aggregatestore"
	"github.com/go-estoria/estoria/snapshotstore"
	streamsnapshots "github.com/go-estoria/estoria/snapshotstore/eventstream"
	"github.com/go-estoria/estoria/typeid"
	"github.com/gofrs/uuid/v5"
)

//go:embed all:web
var webFiles embed.FS

type server struct {
	// live is the fully-decorated store (hooks -> cached -> snapshotting ->
	// event-sourced) used for serving reads and for all simulator writes.
	live aggregatestore.Store[Device]

	// snapshotting is the middle of the stack (snapshotting -> event-sourced,
	// no cache): the benchmark's "snapshot" path. Loading through it starts
	// from the latest snapshot and replays only the events after it.
	snapshotting aggregatestore.Store[Device]

	// history is the undecorated event-sourced store: the benchmark's "cold
	// replay" path, hydrating from version 1 every time. It would also be the
	// store for any version-pinned (ToVersion) load: the event-stream snapshot
	// reader always returns the latest snapshot, so pinned reads must bypass
	// the snapshotting decorator.
	history aggregatestore.Store[Device]

	// events is the raw event store, used for stream-level stats.
	events *sqlstore.EventStore

	// snapshots reads each device's latest snapshot directly (for stats).
	snapshots *streamsnapshots.EventStreamStore

	// cache is the bigcache instance backing the CachedStore, held directly
	// so the evict endpoint can delete entries out from under it.
	cache *bigcache.BigCache

	reg *registry
	sim *simulator
	hub *hub

	snapshotEvery int64

	// events/sec is estimated from the change in total event count between
	// consecutive /api/stats calls.
	statsMu   sync.Mutex
	lastTotal int64
	lastAt    time.Time
	lastRate  float64
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/fleet", s.handleFleet)
	mux.HandleFunc("GET /api/devices/{id}", s.handleGetDevice)
	mux.HandleFunc("GET /api/devices/{id}/benchmark", s.handleBenchmark)
	mux.HandleFunc("POST /api/devices/{id}/evict", s.handleEvict)
	mux.HandleFunc("POST /api/sim/start", s.handleSimStart)
	mux.HandleFunc("POST /api/sim/stop", s.handleSimStop)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/watch", s.handleWatch)

	web, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /", http.FileServerFS(web))

	return mux
}

// deviceMessage is the payload for device reads and SSE updates.
type deviceMessage struct {
	Version int64  `json:"version"`
	Device  Device `json:"device"`

	// SnapshotVersion is populated only on single-device reads.
	SnapshotVersion int64 `json:"snapshotVersion,omitempty"`
}

// deviceID parses and validates the {id} path segment against the registry.
func (s *server) deviceID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.FromString(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return uuid.Nil, false
	}
	if !s.reg.contains(id) {
		writeError(w, http.StatusNotFound, "device not found")
		return uuid.Nil, false
	}
	return id, true
}

// handleFleet returns every registered device at its latest version, loaded
// through the full decorated stack (in the steady state, all cache hits).
func (s *server) handleFleet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	fleet := []deviceMessage{}
	for _, id := range s.reg.all() {
		agg, err := s.live.Load(ctx, id, nil)
		if err != nil {
			s.writeLoadError(w, err)
			return
		}
		fleet = append(fleet, deviceMessage{Version: agg.Version(), Device: agg.Entity()})
	}

	sort.Slice(fleet, func(i, j int) bool { return fleet[i].Device.Name < fleet[j].Device.Name })

	writeJSON(w, http.StatusOK, fleet)
}

// handleGetDevice returns one device, plus the version of its latest snapshot
// (0 if no snapshot has been taken yet).
func (s *server) handleGetDevice(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	id, ok := s.deviceID(w, r)
	if !ok {
		return
	}

	agg, err := s.live.Load(ctx, id, nil)
	if err != nil {
		s.writeLoadError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, deviceMessage{
		Version:         agg.Version(),
		Device:          agg.Entity(),
		SnapshotVersion: s.snapshotVersion(r, id),
	})
}

// snapshotVersion returns the aggregate version of the device's most recent
// snapshot, or 0 if none exists.
func (s *server) snapshotVersion(r *http.Request, id uuid.UUID) int64 {
	snap, err := s.snapshots.ReadSnapshot(r.Context(), typeid.New("device", id), snapshotstore.ReadSnapshotOptions{})
	if err != nil {
		return 0
	}
	return snap.AggregateVersion
}

// benchmarkResult reports three timed hydrations of the same aggregate.
type benchmarkResult struct {
	EventCount      int64 `json:"eventCount"`
	SnapshotVersion int64 `json:"snapshotVersion"`
	ColdMicros      int64 `json:"coldMicros"`
	SnapshotMicros  int64 `json:"snapshotMicros"`
	CachedMicros    int64 `json:"cachedMicros"`
}

// handleBenchmark loads the same device three times, once through each level
// of the store stack, and reports how long each hydration took:
//
//  1. cold: the plain EventSourcedStore replays the entire stream.
//  2. snapshot: the SnapshottingStore reads the latest snapshot and replays
//     only the events after it (eventCount - snapshotVersion of them).
//  3. cached: the full stack returns the aggregate straight from bigcache.
//
// The loads run sequentially against the live database, so treat the numbers
// as an illustration, not a controlled microbenchmark — the *ratios* are the
// story, and they widen as the stream grows.
func (s *server) handleBenchmark(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	id, ok := s.deviceID(w, r)
	if !ok {
		return
	}

	start := time.Now()
	cold, err := s.history.Load(ctx, id, nil)
	if err != nil {
		s.writeLoadError(w, err)
		return
	}
	coldMicros := time.Since(start).Microseconds()

	start = time.Now()
	if _, err = s.snapshotting.Load(ctx, id, nil); err != nil {
		s.writeLoadError(w, err)
		return
	}
	snapshotMicros := time.Since(start).Microseconds()

	start = time.Now()
	if _, err = s.live.Load(ctx, id, nil); err != nil {
		s.writeLoadError(w, err)
		return
	}
	cachedMicros := time.Since(start).Microseconds()

	writeJSON(w, http.StatusOK, benchmarkResult{
		EventCount:      cold.Version(),
		SnapshotVersion: s.snapshotVersion(r, id),
		ColdMicros:      coldMicros,
		SnapshotMicros:  snapshotMicros,
		CachedMicros:    cachedMicros,
	})
}

// handleEvict removes a device's aggregate from the cache, so the next load
// through the full stack has to fall through to the snapshotting store (and
// the one after that hits the freshly re-populated cache again). The contrib
// cache interface has no removal method, but the underlying bigcache does, so
// the entry is deleted directly by its "device_<uuid>" key.
func (s *server) handleEvict(w http.ResponseWriter, r *http.Request) {
	id, ok := s.deviceID(w, r)
	if !ok {
		return
	}

	err := s.cache.Delete(typeid.New("device", id).String())
	if err != nil && !errors.Is(err, bigcache.ErrEntryNotFound) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"evicted": err == nil})
}

func (s *server) handleSimStart(w http.ResponseWriter, _ *http.Request) {
	s.sim.start()
	writeJSON(w, http.StatusOK, map[string]any{"running": true})
}

func (s *server) handleSimStop(w http.ResponseWriter, _ *http.Request) {
	s.sim.stop()
	writeJSON(w, http.StatusOK, map[string]any{"running": false})
}

// handleStats exposes the machinery that is normally invisible: every stream
// in the event store (device streams and their parallel snapshot streams),
// event totals, the write rate, and the aggregate store decorator stack.
func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	type streamInfo struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
	}

	stats := struct {
		DeviceCount    int          `json:"deviceCount"`
		DeviceEvents   int64        `json:"deviceEvents"`
		SnapshotEvents int64        `json:"snapshotEvents"`
		TotalEvents    int64        `json:"totalEvents"`
		EventsPerSec   float64      `json:"eventsPerSec"`
		SimRunning     bool         `json:"simRunning"`
		SnapshotEvery  int64        `json:"snapshotEvery"`
		StoreStack     []string     `json:"storeStack"`
		Streams        []streamInfo `json:"streams"`
	}{
		DeviceCount:   s.reg.count(),
		SimRunning:    s.sim.isRunning(),
		SnapshotEvery: s.snapshotEvery,
		StoreStack: []string{
			"HookableStore (SSE broadcast on AfterSave)",
			"CachedStore (bigcache, in-process)",
			"SnapshottingStore (snapshot every " + strconv.FormatInt(s.snapshotEvery, 10) + " events)",
			"EventSourcedStore (optimistic concurrency)",
			"SQLite event store",
		},
		Streams: []streamInfo{},
	}

	streams, err := s.events.ListStreams(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	for _, stream := range streams {
		stats.Streams = append(stats.Streams, streamInfo{ID: stream.StreamID.String(), Version: stream.LastOffset})
		switch stream.StreamID.Type {
		case "device":
			stats.DeviceEvents += stream.LastOffset
		case "devicesnapshot":
			stats.SnapshotEvents += stream.LastOffset
		}
	}
	stats.TotalEvents = stats.DeviceEvents + stats.SnapshotEvents

	sort.Slice(stats.Streams, func(i, j int) bool { return stats.Streams[i].ID < stats.Streams[j].ID })

	stats.EventsPerSec = s.eventRate(stats.TotalEvents)

	writeJSON(w, http.StatusOK, stats)
}

// eventRate estimates events/sec from the growth in total events since the
// previous stats call.
func (s *server) eventRate(total int64) float64 {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()

	now := time.Now()
	if elapsed := now.Sub(s.lastAt).Seconds(); !s.lastAt.IsZero() && elapsed > 0.5 {
		s.lastRate = float64(total-s.lastTotal) / elapsed
		s.lastTotal, s.lastAt = total, now
	} else if s.lastAt.IsZero() {
		s.lastTotal, s.lastAt = total, now
	}

	if s.lastRate < 0 {
		s.lastRate = 0
	}
	return float64(int(s.lastRate*10)) / 10
}

// handleWatch streams device updates to the client over server-sent events.
// Each message is one device's post-save state, pushed by the AfterSave hook.
func (s *server) handleWatch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	rc := http.NewResponseController(w)

	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	// send the current state of the whole fleet immediately so a
	// reconnecting client resyncs
	for _, id := range s.reg.all() {
		agg, err := s.live.Load(r.Context(), id, nil)
		if err != nil {
			continue
		}
		msg, _ := json.Marshal(deviceMessage{Version: agg.Version(), Device: agg.Entity()})
		fmt.Fprintf(w, "data: %s\n\n", msg)
	}
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
		writeError(w, http.StatusNotFound, "device not found")
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
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
