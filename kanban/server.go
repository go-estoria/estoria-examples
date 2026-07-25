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
	"strconv"
	"strings"
	"time"

	"github.com/go-estoria/estoria"
	sqlstore "github.com/go-estoria/estoria-contrib/sqlite/eventstore"
	"github.com/go-estoria/estoria/aggregatestore"
	"github.com/go-estoria/estoria/eventstore"
	"github.com/go-estoria/estoria/eventstore/projection"
	"github.com/go-estoria/estoria/typeid"
	"github.com/gofrs/uuid/v5"
)

//go:embed all:web
var webFiles embed.FS

type server struct {
	boardID uuid.UUID

	// live is the fully-decorated store (hooks -> snapshotting -> event-sourced)
	// used for latest-state reads and for saving commands.
	live aggregatestore.Store[Board]

	// history is the undecorated event-sourced store, used for loading the
	// board at a pinned historical version (time travel and stale-base loads).
	// It bypasses the snapshot fast-path, which always returns the latest
	// snapshot and therefore cannot serve reads pinned to an older version.
	history aggregatestore.Store[Board]

	// events is the raw event store, used for stream-level reads (activity
	// feed, stats) that don't need an aggregate.
	events *sqlstore.EventStore

	hub           *hub
	snapshotEvery int64
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/board", s.handleGetBoard)
	mux.HandleFunc("POST /api/board/rename", s.handleRenameBoard)
	mux.HandleFunc("POST /api/columns", s.handleAddColumn)
	mux.HandleFunc("POST /api/columns/{id}/rename", s.handleRenameColumn)
	mux.HandleFunc("POST /api/cards", s.handleAddCard)
	mux.HandleFunc("POST /api/cards/{id}/edit", s.handleEditCard)
	mux.HandleFunc("POST /api/cards/{id}/move", s.handleMoveCard)
	mux.HandleFunc("POST /api/cards/{id}/delete", s.handleDeleteCard)
	mux.HandleFunc("GET /api/activity", s.handleActivity)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/watch", s.handleWatch)

	web, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /", http.FileServerFS(web))

	return mux
}

// boardMessage is the payload for board reads and SSE updates.
type boardMessage struct {
	Version int64 `json:"version"`
	Live    bool  `json:"live"`
	Board   Board `json:"board"`
}

// handleGetBoard returns the board at its latest version, or, when the
// "version" query parameter is provided, at that historical version.
func (s *server) handleGetBoard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var agg *aggregatestore.Aggregate[Board]
	var err error
	live := true

	if v := r.URL.Query().Get("version"); v != "" {
		version, parseErr := strconv.ParseInt(v, 10, 64)
		if parseErr != nil || version < 1 {
			writeError(w, http.StatusBadRequest, "version must be a positive integer")
			return
		}

		// time travel: hydrate the aggregate only up to the requested version
		live = false
		agg, err = s.history.Load(ctx, s.boardID, &aggregatestore.LoadOptions{ToVersion: version})
	} else {
		agg, err = s.live.Load(ctx, s.boardID, nil)
	}

	if err != nil {
		s.writeLoadError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, boardMessage{Version: agg.Version(), Live: live, Board: agg.Entity()})
}

// A commandFunc validates a command against the board state it is based on
// and returns the resulting event.
type commandFunc func(board Board) (estoria.EntityEvent[Board], error)

// runCommand is the write path shared by all commands:
//
//  1. Load the board at the version the client last saw (baseVersion). When
//     the stream has advanced past it, saving will fail the ExpectVersion
//     check — real optimistic concurrency, not a simulated check.
//  2. Validate the command against that state and derive an event.
//  3. Append the event and save. On a version conflict, respond 409 so the
//     client can refresh and retry.
func (s *server) runCommand(w http.ResponseWriter, r *http.Request, baseVersion int64, cmd commandFunc) {
	ctx := r.Context()

	var agg *aggregatestore.Aggregate[Board]
	var err error
	if baseVersion > 0 {
		agg, err = s.history.Load(ctx, s.boardID, &aggregatestore.LoadOptions{ToVersion: baseVersion})
	} else {
		agg, err = s.live.Load(ctx, s.boardID, nil)
	}
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

	if err := s.live.Save(ctx, agg, nil); err != nil {
		var mismatch eventstore.StreamVersionMismatchError
		if errors.As(err, &mismatch) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":           "version_conflict",
				"expectedVersion": mismatch.ExpectedVersion,
				"actualVersion":   mismatch.ActualVersion,
				"message": fmt.Sprintf(
					"the board has changed since version %d (it is now at version %d)",
					mismatch.ExpectedVersion, mismatch.ActualVersion),
			})
			return
		}

		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"version": agg.Version()})
}

func (s *server) handleRenameBoard(w http.ResponseWriter, r *http.Request) {
	req, err := readJSON[struct {
		BaseVersion int64  `json:"baseVersion"`
		Name        string `json:"name"`
	}](r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.runCommand(w, r, req.BaseVersion, func(Board) (estoria.EntityEvent[Board], error) {
		name, err := requireTitle(req.Name, "board name")
		if err != nil {
			return nil, err
		}
		return BoardRenamed{Name: name}, nil
	})
}

func (s *server) handleAddColumn(w http.ResponseWriter, r *http.Request) {
	req, err := readJSON[struct {
		BaseVersion int64  `json:"baseVersion"`
		Title       string `json:"title"`
	}](r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.runCommand(w, r, req.BaseVersion, func(Board) (estoria.EntityEvent[Board], error) {
		title, err := requireTitle(req.Title, "column title")
		if err != nil {
			return nil, err
		}
		return ColumnAdded{ColumnID: typeid.NewV7("column").String(), Title: title}, nil
	})
}

func (s *server) handleRenameColumn(w http.ResponseWriter, r *http.Request) {
	columnID := r.PathValue("id")
	req, err := readJSON[struct {
		BaseVersion int64  `json:"baseVersion"`
		Title       string `json:"title"`
	}](r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.runCommand(w, r, req.BaseVersion, func(board Board) (estoria.EntityEvent[Board], error) {
		title, err := requireTitle(req.Title, "column title")
		if err != nil {
			return nil, err
		}
		if !board.HasColumn(columnID) {
			return nil, fmt.Errorf("column %s does not exist", columnID)
		}
		return ColumnRenamed{ColumnID: columnID, Title: title}, nil
	})
}

func (s *server) handleAddCard(w http.ResponseWriter, r *http.Request) {
	req, err := readJSON[struct {
		BaseVersion int64  `json:"baseVersion"`
		ColumnID    string `json:"columnId"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Color       string `json:"color"`
	}](r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.runCommand(w, r, req.BaseVersion, func(board Board) (estoria.EntityEvent[Board], error) {
		title, err := requireTitle(req.Title, "card title")
		if err != nil {
			return nil, err
		}
		if !board.HasColumn(req.ColumnID) {
			return nil, fmt.Errorf("column %s does not exist", req.ColumnID)
		}
		return CardAdded{
			CardID:      typeid.NewV7("card").String(),
			ColumnID:    req.ColumnID,
			Title:       title,
			Description: strings.TrimSpace(req.Description),
			Color:       req.Color,
		}, nil
	})
}

func (s *server) handleEditCard(w http.ResponseWriter, r *http.Request) {
	cardID := r.PathValue("id")
	req, err := readJSON[struct {
		BaseVersion int64  `json:"baseVersion"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Color       string `json:"color"`
	}](r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.runCommand(w, r, req.BaseVersion, func(board Board) (estoria.EntityEvent[Board], error) {
		title, err := requireTitle(req.Title, "card title")
		if err != nil {
			return nil, err
		}
		if !board.HasCard(cardID) {
			return nil, fmt.Errorf("card %s does not exist", cardID)
		}
		return CardEdited{
			CardID:      cardID,
			Title:       title,
			Description: strings.TrimSpace(req.Description),
			Color:       req.Color,
		}, nil
	})
}

func (s *server) handleMoveCard(w http.ResponseWriter, r *http.Request) {
	cardID := r.PathValue("id")
	req, err := readJSON[struct {
		BaseVersion int64  `json:"baseVersion"`
		ToColumnID  string `json:"toColumnId"`
		ToIndex     int    `json:"toIndex"`
	}](r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.runCommand(w, r, req.BaseVersion, func(board Board) (estoria.EntityEvent[Board], error) {
		if !board.HasCard(cardID) {
			return nil, fmt.Errorf("card %s does not exist", cardID)
		}
		if !board.HasColumn(req.ToColumnID) {
			return nil, fmt.Errorf("column %s does not exist", req.ToColumnID)
		}
		return CardMoved{CardID: cardID, ToColumn: req.ToColumnID, ToIndex: req.ToIndex}, nil
	})
}

func (s *server) handleDeleteCard(w http.ResponseWriter, r *http.Request) {
	cardID := r.PathValue("id")
	req, err := readJSON[struct {
		BaseVersion int64 `json:"baseVersion"`
	}](r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.runCommand(w, r, req.BaseVersion, func(board Board) (estoria.EntityEvent[Board], error) {
		if !board.HasCard(cardID) {
			return nil, fmt.Errorf("card %s does not exist", cardID)
		}
		return CardRemoved{CardID: cardID}, nil
	})
}

// activityEntry is one row in the activity feed: a stream event rendered as a
// human-readable description.
type activityEntry struct {
	Version     int64     `json:"version"`
	Type        string    `json:"type"`
	Timestamp   time.Time `json:"timestamp"`
	Description string    `json:"description"`
}

// handleActivity projects the board's event stream into a human-readable
// history. Titles are tracked as the projection advances so that each entry
// describes cards and columns by the names they had at that moment.
func (s *server) handleActivity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	streamID := typeid.New("board", s.boardID)

	iter, err := s.events.ReadStream(ctx, streamID, eventstore.ReadStreamOptions{})
	if errors.Is(err, eventstore.ErrStreamNotFound) {
		writeJSON(w, http.StatusOK, []activityEntry{})
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer iter.Close(ctx)

	proj, err := projection.New(iter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	titles := map[string]string{} // card/column ID -> title as of the current event
	entries := []activityEntry{}

	if _, err := proj.Project(ctx, projection.EventHandlerFunc(func(_ context.Context, evt *eventstore.Event) error {
		entries = append(entries, activityEntry{
			Version:     evt.StreamVersion,
			Type:        evt.ID.Type,
			Timestamp:   evt.Timestamp,
			Description: describeEvent(evt, titles),
		})
		return nil
	})); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, entries)
}

// describeEvent renders a stream event as prose, using (and updating) the
// running ID->title map so descriptions use historically-correct names.
func describeEvent(evt *eventstore.Event, titles map[string]string) string {
	unmarshal := func(dst any) bool { return json.Unmarshal(evt.Data, dst) == nil }

	switch evt.ID.Type {
	case BoardCreated{}.EventType():
		var e BoardCreated
		if unmarshal(&e) {
			return fmt.Sprintf("created the board %q", e.Name)
		}
	case BoardRenamed{}.EventType():
		var e BoardRenamed
		if unmarshal(&e) {
			return fmt.Sprintf("renamed the board to %q", e.Name)
		}
	case ColumnAdded{}.EventType():
		var e ColumnAdded
		if unmarshal(&e) {
			titles[e.ColumnID] = e.Title
			return fmt.Sprintf("added column %q", e.Title)
		}
	case ColumnRenamed{}.EventType():
		var e ColumnRenamed
		if unmarshal(&e) {
			old := titleOr(titles, e.ColumnID, "a column")
			titles[e.ColumnID] = e.Title
			return fmt.Sprintf("renamed column %q to %q", old, e.Title)
		}
	case CardAdded{}.EventType():
		var e CardAdded
		if unmarshal(&e) {
			titles[e.CardID] = e.Title
			return fmt.Sprintf("added %q to %q", e.Title, titleOr(titles, e.ColumnID, "a column"))
		}
	case CardEdited{}.EventType():
		var e CardEdited
		if unmarshal(&e) {
			old := titleOr(titles, e.CardID, "a card")
			titles[e.CardID] = e.Title
			if old != e.Title {
				return fmt.Sprintf("renamed %q to %q", old, e.Title)
			}
			return fmt.Sprintf("edited %q", e.Title)
		}
	case CardMoved{}.EventType():
		var e CardMoved
		if unmarshal(&e) {
			return fmt.Sprintf("moved %q to %q",
				titleOr(titles, e.CardID, "a card"), titleOr(titles, e.ToColumn, "a column"))
		}
	case CardRemoved{}.EventType():
		var e CardRemoved
		if unmarshal(&e) {
			return fmt.Sprintf("removed %q", titleOr(titles, e.CardID, "a card"))
		}
	}

	return evt.ID.Type
}

func titleOr(titles map[string]string, id, fallback string) string {
	if title, ok := titles[id]; ok {
		return title
	}
	return fallback
}

// handleStats exposes the machinery that is normally invisible: the streams
// in the event store (including the snapshot stream), the latest snapshot,
// and the aggregate store decorator stack.
func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	type streamInfo struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
	}

	stats := struct {
		BoardVersion        int64        `json:"boardVersion"`
		Streams             []streamInfo `json:"streams"`
		SnapshotCount       int64        `json:"snapshotCount"`
		LastSnapshotVersion int64        `json:"lastSnapshotVersion"`
		SnapshotEvery       int64        `json:"snapshotEvery"`
		StoreStack          []string     `json:"storeStack"`
	}{
		Streams:       []streamInfo{},
		SnapshotEvery: s.snapshotEvery,
		StoreStack: []string{
			"HookableStore (SSE broadcast on AfterSave)",
			"SnapshottingStore (snapshot every " + strconv.FormatInt(s.snapshotEvery, 10) + " events)",
			"EventSourcedStore (optimistic concurrency)",
			"SQLite event store",
		},
	}

	boardStreamID := typeid.New("board", s.boardID)
	snapshotStreamID := typeid.New("boardsnapshot", s.boardID)

	streams, err := s.events.ListStreams(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	for _, stream := range streams {
		stats.Streams = append(stats.Streams, streamInfo{ID: stream.StreamID.String(), Version: stream.LastOffset})
		switch stream.StreamID {
		case boardStreamID:
			stats.BoardVersion = stream.LastOffset
		case snapshotStreamID:
			stats.SnapshotCount = stream.LastOffset
		}
	}

	// the latest snapshot is the last event in the snapshot stream
	if stats.SnapshotCount > 0 {
		iter, err := s.events.ReadStream(ctx, snapshotStreamID, eventstore.ReadStreamOptions{
			Direction: eventstore.Reverse,
			Count:     1,
		})
		if err == nil {
			defer iter.Close(ctx)
			if evt, err := iter.Next(ctx); err == nil {
				var snap struct {
					AggregateVersion int64
				}
				if json.Unmarshal(evt.Data, &snap) == nil {
					stats.LastSnapshotVersion = snap.AggregateVersion
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, stats)
}

// handleWatch streams board updates to the client over server-sent events.
func (s *server) handleWatch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	rc := http.NewResponseController(w)

	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	// send the current state immediately so a reconnecting client resyncs
	if agg, err := s.live.Load(r.Context(), s.boardID, nil); err == nil {
		msg, _ := json.Marshal(boardMessage{Version: agg.Version(), Live: true, Board: agg.Entity()})
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
		writeError(w, http.StatusNotFound, "board not found")
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

func requireTitle(s, what string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New(what + " is required")
	}
	if len(s) > 200 {
		return "", errors.New(what + " is too long")
	}
	return s, nil
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
