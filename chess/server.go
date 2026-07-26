package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-estoria/estoria"
	sqlstore "github.com/go-estoria/estoria-contrib/sqlite/eventstore"
	"github.com/go-estoria/estoria/aggregatestore"
	"github.com/go-estoria/estoria/eventstore"
	"github.com/gofrs/uuid/v5"
	"github.com/notnil/chess"
)

//go:embed all:web
var webFiles embed.FS

type server struct {
	// live is the decorated store (hooks -> event-sourced) used for
	// latest-state reads and for saving commands.
	live aggregatestore.Store[Game]

	// history is the undecorated event-sourced store, used for loading a game
	// at a pinned historical version (the replay slider and stale-base loads).
	history aggregatestore.Store[Game]

	// events is the raw event store, used to list game streams for the lobby.
	events *sqlstore.EventStore

	hub *hub
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/games", s.handleListGames)
	mux.HandleFunc("POST /api/games", s.handleCreateGame)
	mux.HandleFunc("GET /api/games/{id}", s.handleGetGame)
	mux.HandleFunc("GET /api/games/{id}/legal-moves", s.handleLegalMoves)
	mux.HandleFunc("POST /api/games/{id}/move", s.handleMove)
	mux.HandleFunc("POST /api/games/{id}/resign", s.handleResign)
	mux.HandleFunc("GET /api/games/{id}/pgn", s.handlePGN)
	mux.HandleFunc("GET /api/watch", s.handleWatch)

	web, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /", http.FileServerFS(web))

	return mux
}

// gameMessage is the payload for game reads and SSE updates. Every message is
// tagged with the game ID so SSE clients can filter for the game they are
// watching.
type gameMessage struct {
	GameID  string   `json:"gameId"`
	Version int64    `json:"version"`
	Live    bool     `json:"live"`
	Game    Game     `json:"game"`
	SAN     []string `json:"san"`
}

// newGameMessage assembles the standard game payload, including the move list
// rendered in algebraic notation.
func newGameMessage(agg *aggregatestore.Aggregate[Game], live bool) gameMessage {
	game := agg.Entity()
	san, err := sanHistory(game.MovesUCI)
	if err != nil {
		// the stream already applied cleanly, so this should be unreachable
		estoria.GetLogger().Error("rendering SAN history", "game_id", game.ID, "error", err)
		san = []string{}
	}
	return gameMessage{
		GameID:  game.ID.String(),
		Version: agg.Version(),
		Live:    live,
		Game:    game,
		SAN:     san,
	}
}

// gameSummary is one row in the lobby list.
type gameSummary struct {
	GameID    string `json:"gameId"`
	White     string `json:"white"`
	Black     string `json:"black"`
	MoveCount int    `json:"moveCount"`
	Outcome   string `json:"outcome"`
	Method    string `json:"method"`
	Turn      string `json:"turn"`
	Check     bool   `json:"check"`
	Version   int64  `json:"version"`
}

// handleListGames builds the lobby by listing every "game" stream in the
// event store and loading each aggregate. A load per game is fine at demo
// scale; a production lobby would maintain a read model projected from the
// streams instead (see the README).
func (s *server) handleListGames(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	streams, err := s.events.ListStreams(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	summaries := []gameSummary{}
	for _, stream := range streams {
		if stream.StreamID.Type != "game" {
			continue
		}

		agg, err := s.live.Load(ctx, stream.StreamID.UUID, nil)
		if err != nil {
			estoria.GetLogger().Error("loading game for lobby", "stream_id", stream.StreamID, "error", err)
			continue
		}

		game := agg.Entity()
		summaries = append(summaries, gameSummary{
			GameID:    game.ID.String(),
			White:     game.White,
			Black:     game.Black,
			MoveCount: len(game.MovesUCI),
			Outcome:   game.Outcome,
			Method:    game.Method,
			Turn:      game.Turn,
			Check:     game.Check,
			Version:   agg.Version(),
		})
	}

	// game IDs are UUIDv7 (time-ordered), so sorting descending puts the
	// newest games first
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].GameID > summaries[j].GameID })

	writeJSON(w, http.StatusOK, summaries)
}

func (s *server) handleCreateGame(w http.ResponseWriter, r *http.Request) {
	req, err := readJSON[struct {
		White string `json:"white"`
		Black string `json:"black"`
	}](r)
	if err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	white, err := playerName(req.White, "White")
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	black, err := playerName(req.Black, "Black")
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	gameID, err := uuid.NewV7()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	agg := s.live.New(gameID)
	if err := agg.Append(GameCreated{White: white, Black: black}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.live.Save(r.Context(), agg, nil); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, newGameMessage(agg, true))
}

// handleGetGame returns the game at its latest version, or, when the
// "version" query parameter is provided, at that historical version. Version
// 1 is the freshly created game; version k is the position after k-1 moves.
func (s *server) handleGetGame(w http.ResponseWriter, r *http.Request) {
	gameID, ok := pathGameID(w, r)
	if !ok {
		return
	}

	var agg *aggregatestore.Aggregate[Game]
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
		agg, err = s.history.Load(r.Context(), gameID, &aggregatestore.LoadOptions{ToVersion: version})
	} else {
		agg, err = s.live.Load(r.Context(), gameID, nil)
	}

	if err != nil {
		writeLoadError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, newGameMessage(agg, live))
}

// legalTarget is one destination square a piece can move to. Promotion
// targets require a fifth UCI character selecting the promotion piece
// ("e7e8q"), which the UI surfaces as a piece picker.
type legalTarget struct {
	To        string `json:"to"`
	Promotion bool   `json:"promotion"`
}

// handleLegalMoves returns every legal move in the game's live position,
// grouped by origin square. The rules engine derives them from the position,
// which is itself derived from the event stream.
func (s *server) handleLegalMoves(w http.ResponseWriter, r *http.Request) {
	gameID, ok := pathGameID(w, r)
	if !ok {
		return
	}

	agg, err := s.live.Load(r.Context(), gameID, nil)
	if err != nil {
		writeLoadError(w, err)
		return
	}

	game := agg.Entity()
	moves := map[string][]legalTarget{}

	if !game.Over() {
		engine, err := game.rebuild()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		for _, move := range engine.ValidMoves() {
			from, to := move.S1().String(), move.S2().String()
			promotion := move.Promo() != chess.NoPieceType

			// the four promotion moves per target square collapse into one
			// entry; the client picks the piece and appends its UCI letter
			if promotion && hasTarget(moves[from], to) {
				continue
			}
			moves[from] = append(moves[from], legalTarget{To: to, Promotion: promotion})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"version": agg.Version(),
		"turn":    game.Turn,
		"moves":   moves,
	})
}

func hasTarget(targets []legalTarget, to string) bool {
	for _, t := range targets {
		if t.To == to {
			return true
		}
	}
	return false
}

// A commandFunc validates a command against the game state it is based on
// and returns the resulting event.
type commandFunc func(game Game) (estoria.EntityEvent[Game], error)

// runCommand is the write path shared by all commands:
//
//  1. Load the game at the version the client last saw (baseVersion). When
//     the stream has advanced past it, saving will fail the ExpectVersion
//     check — real optimistic concurrency, not a simulated check.
//  2. Derive the event and pre-flight it through its own ApplyTo against that
//     state. Chess legality lives in ApplyTo, so an illegal move (or a move
//     after the game is over) is rejected here with a 422 — before anything
//     is written to the stream.
//  3. Append the event and save. On a version conflict, respond 409 so the
//     client can refresh; in chess a conflict means the position changed
//     under you — the other player moved first.
func (s *server) runCommand(w http.ResponseWriter, r *http.Request, gameID uuid.UUID, baseVersion int64, cmd commandFunc) {
	ctx := r.Context()

	var agg *aggregatestore.Aggregate[Game]
	var err error
	if baseVersion > 0 {
		agg, err = s.history.Load(ctx, gameID, &aggregatestore.LoadOptions{ToVersion: baseVersion})
	} else {
		agg, err = s.live.Load(ctx, gameID, nil)
	}
	if err != nil {
		writeLoadError(w, err)
		return
	}

	event, err := cmd(agg.Entity())
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	// pre-flight: estoria applies events on save, after they are written, so
	// an event the domain rejects must never reach the stream
	if _, err := event.ApplyTo(ctx, agg.Entity()); err != nil {
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
					"the game has changed since version %d (it is now at version %d)",
					mismatch.ExpectedVersion, mismatch.ActualVersion),
			})
			return
		}

		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"version": agg.Version()})
}

func (s *server) handleMove(w http.ResponseWriter, r *http.Request) {
	gameID, ok := pathGameID(w, r)
	if !ok {
		return
	}

	req, err := readJSON[struct {
		BaseVersion int64  `json:"baseVersion"`
		UCI         string `json:"uci"`
	}](r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.runCommand(w, r, gameID, req.BaseVersion, func(Game) (estoria.EntityEvent[Game], error) {
		uci := strings.ToLower(strings.TrimSpace(req.UCI))
		if uci == "" {
			return nil, errors.New("uci move is required")
		}
		return MoveMade{UCI: uci}, nil
	})
}

func (s *server) handleResign(w http.ResponseWriter, r *http.Request) {
	gameID, ok := pathGameID(w, r)
	if !ok {
		return
	}

	req, err := readJSON[struct {
		BaseVersion int64  `json:"baseVersion"`
		Color       string `json:"color"`
	}](r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.runCommand(w, r, gameID, req.BaseVersion, func(Game) (estoria.EntityEvent[Game], error) {
		return PlayerResigned{Color: strings.ToLower(strings.TrimSpace(req.Color))}, nil
	})
}

// handlePGN renders the game's event stream as a PGN document — the standard
// interchange format for chess games, importable into any chess tool.
func (s *server) handlePGN(w http.ResponseWriter, r *http.Request) {
	gameID, ok := pathGameID(w, r)
	if !ok {
		return
	}

	agg, err := s.live.Load(r.Context(), gameID, nil)
	if err != nil {
		writeLoadError(w, err)
		return
	}

	game := agg.Entity()
	engine, err := game.rebuild()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// replaying moves recovers on-board outcomes (checkmate, stalemate);
	// resignation lives only in the event stream, so re-apply it here
	if game.Method == chess.Resignation.String() {
		switch game.Outcome {
		case string(chess.BlackWon):
			engine.Resign(chess.White)
		case string(chess.WhiteWon):
			engine.Resign(chess.Black)
		}
	}

	engine.AddTagPair("Event", "Estoria Chess")
	engine.AddTagPair("Site", "estoria-examples/chess")
	engine.AddTagPair("Date", time.Now().Format("2006.01.02"))
	engine.AddTagPair("White", game.White)
	engine.AddTagPair("Black", game.Black)
	engine.AddTagPair("Result", game.Outcome)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "chess-"+game.ID.String()+".pgn"))
	fmt.Fprintln(w, strings.TrimSpace(engine.String()))
}

// handleWatch streams game updates to the client over server-sent events.
// Every message carries a gameId; the game view filters for its own game and
// the lobby uses every message to keep its list fresh.
func (s *server) handleWatch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	rc := http.NewResponseController(w)

	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	// an initial comment confirms the connection so clients can flip their
	// status pill immediately
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

// pathGameID parses the {id} path segment as a game UUID, writing a 400 when
// it is malformed.
func pathGameID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.FromString(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid game ID")
		return uuid.Nil, false
	}
	return id, true
}

func writeLoadError(w http.ResponseWriter, err error) {
	if errors.Is(err, aggregatestore.ErrAggregateNotFound) {
		writeError(w, http.StatusNotFound, "game not found")
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

// playerName trims and bounds a player name, falling back to a default when
// it is empty.
func playerName(s, fallback string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback, nil
	}
	if len(s) > 40 {
		return "", errors.New("player name is too long")
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
