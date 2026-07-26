package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-estoria/estoria"
	"github.com/go-estoria/estoria/aggregatestore"
	"github.com/go-estoria/estoria/eventstore"
	"github.com/go-estoria/estoria/eventstore/memory"
	"github.com/gofrs/uuid/v5"
)

// Event-sourced domains are easy to test: given a game, apply an event,
// assert on the resulting state. No storage, no mocks — and because the rules
// engine lives in ApplyTo, move legality is tested the same way.

// scholarsMate is 1.e4 e5 2.Bc4 Nc6 3.Qh5 Nf6?? 4.Qxf7# in UCI notation.
var scholarsMate = []string{"e2e4", "e7e5", "f1c4", "b8c6", "d1h5", "g8f6", "h5f7"}

// apply runs a sequence of events through ApplyTo, failing the test on error.
func apply(t *testing.T, game Game, events ...estoria.EntityEvent[Game]) Game {
	t.Helper()
	for _, event := range events {
		var err error
		if game, err = event.ApplyTo(context.Background(), game); err != nil {
			t.Fatalf("applying %T: %v", event, err)
		}
	}
	return game
}

func newTestGame(t *testing.T) Game {
	t.Helper()
	game := NewGame(uuid.Must(uuid.NewV4()))
	return apply(t, game, GameCreated{White: "Alice", Black: "Bob"})
}

func TestEventApplication(t *testing.T) {
	t.Parallel()

	t.Run("creates a game at the starting position", func(t *testing.T) {
		t.Parallel()
		game := newTestGame(t)

		if game.White != "Alice" || game.Black != "Bob" {
			t.Errorf("players = %q vs %q, want Alice vs Bob", game.White, game.Black)
		}
		if game.Turn != "white" {
			t.Errorf("turn = %q, want white", game.Turn)
		}
		if game.Outcome != "*" {
			t.Errorf("outcome = %q, want *", game.Outcome)
		}
		if !strings.HasPrefix(game.FEN, "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w") {
			t.Errorf("FEN = %q, want the standard starting position", game.FEN)
		}
	})

	t.Run("a legal move advances the position and the turn", func(t *testing.T) {
		t.Parallel()
		game := apply(t, newTestGame(t), MoveMade{UCI: "e2e4"})

		if !strings.HasPrefix(game.FEN, "rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR b") {
			t.Errorf("FEN = %q, want the position after 1.e4", game.FEN)
		}
		if game.Turn != "black" {
			t.Errorf("turn = %q, want black", game.Turn)
		}
		if len(game.MovesUCI) != 1 || game.MovesUCI[0] != "e2e4" {
			t.Errorf("moves = %v, want [e2e4]", game.MovesUCI)
		}
	})

	t.Run("rejects illegal moves without changing state", func(t *testing.T) {
		t.Parallel()
		before := apply(t, newTestGame(t), MoveMade{UCI: "e2e4"})

		for name, uci := range map[string]string{
			"pawn moving three squares": "a2a5",
			"moving the opponent turn":  "e4e5", // white pawn, but it is black's move
			"moving an empty square":    "d4d5",
			"gibberish":                 "zz99",
		} {
			after, err := MoveMade{UCI: uci}.ApplyTo(context.Background(), before)
			if err == nil {
				t.Errorf("%s (%s): expected an error", name, uci)
			}
			if after.FEN != before.FEN || len(after.MovesUCI) != len(before.MovesUCI) {
				t.Errorf("%s (%s): state changed on a rejected move", name, uci)
			}
		}
	})

	t.Run("scholar's mate ends the game by checkmate", func(t *testing.T) {
		t.Parallel()
		game := newTestGame(t)
		for _, uci := range scholarsMate {
			game = apply(t, game, MoveMade{UCI: uci})
		}

		if game.Outcome != "1-0" {
			t.Errorf("outcome = %q, want 1-0", game.Outcome)
		}
		if game.Method != "Checkmate" {
			t.Errorf("method = %q, want Checkmate", game.Method)
		}

		san, err := sanHistory(game.MovesUCI)
		if err != nil {
			t.Fatal(err)
		}
		if got := san[len(san)-1]; got != "Qxf7#" {
			t.Errorf("final SAN = %q, want Qxf7#", got)
		}
	})

	t.Run("rejects moves after the game is over", func(t *testing.T) {
		t.Parallel()
		game := newTestGame(t)
		for _, uci := range scholarsMate {
			game = apply(t, game, MoveMade{UCI: uci})
		}

		if _, err := (MoveMade{UCI: "e8e7"}).ApplyTo(context.Background(), game); err == nil {
			t.Error("expected an error moving after checkmate")
		}
	})

	t.Run("resignation ends the game in the opponent's favor", func(t *testing.T) {
		t.Parallel()
		game := apply(t, newTestGame(t), MoveMade{UCI: "e2e4"}, PlayerResigned{Color: "white"})

		if game.Outcome != "0-1" {
			t.Errorf("outcome = %q, want 0-1", game.Outcome)
		}
		if game.Method != "Resignation" {
			t.Errorf("method = %q, want Resignation", game.Method)
		}

		if _, err := (PlayerResigned{Color: "black"}).ApplyTo(context.Background(), game); err == nil {
			t.Error("expected an error resigning a finished game")
		}
	})

	t.Run("rejects invalid transitions", func(t *testing.T) {
		t.Parallel()
		uncreated := NewGame(uuid.Must(uuid.NewV4()))

		for name, tc := range map[string]struct {
			game  Game
			event estoria.EntityEvent[Game]
		}{
			"move before creation":   {uncreated, MoveMade{UCI: "e2e4"}},
			"resign before creation": {uncreated, PlayerResigned{Color: "white"}},
			"double creation":        {newTestGame(t), GameCreated{White: "X", Black: "Y"}},
			"resign a bad color":     {newTestGame(t), PlayerResigned{Color: "purple"}},
		} {
			if _, err := tc.event.ApplyTo(context.Background(), tc.game); err == nil {
				t.Errorf("%s: expected an error", name)
			}
		}
	})

	t.Run("does not mutate the input game", func(t *testing.T) {
		t.Parallel()
		before := apply(t, newTestGame(t), MoveMade{UCI: "e2e4"})
		beforeFEN, beforeMoves := before.FEN, len(before.MovesUCI)

		if _, err := (MoveMade{UCI: "e7e5"}).ApplyTo(context.Background(), before); err != nil {
			t.Fatal(err)
		}

		if before.FEN != beforeFEN || len(before.MovesUCI) != beforeMoves {
			t.Errorf("input game was mutated: %+v", before)
		}
	})
}

// TestGameRoundTrip runs the full aggregate lifecycle against estoria's
// in-memory event store: save, load, replay to a mid-game version, and
// conflict detection when both players race to move.
func TestGameRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	eventStore, err := memory.NewEventStore()
	if err != nil {
		t.Fatal(err)
	}

	store, err := aggregatestore.New(eventStore, NewGame,
		aggregatestore.WithEventTypes(gameEventPrototypes()...))
	if err != nil {
		t.Fatal(err)
	}

	gameID := uuid.Must(uuid.NewV4())

	agg := store.New(gameID)
	if err := agg.Append(
		GameCreated{White: "Alice", Black: "Bob"},
		MoveMade{UCI: "e2e4"},
		MoveMade{UCI: "e7e5"},
		MoveMade{UCI: "g1f3"},
	); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, agg, nil); err != nil {
		t.Fatal(err)
	}

	// load the latest state
	loaded, err := store.Load(ctx, gameID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v := loaded.Version(); v != 4 {
		t.Fatalf("loaded version = %d, want 4", v)
	}
	if game := loaded.Entity(); len(game.MovesUCI) != 3 || game.Turn != "black" {
		t.Fatalf("loaded game = %+v, want 3 moves with black to play", game)
	}

	// replay: version 3 is the position after two plies (1.e4 e5)
	past, err := store.Load(ctx, gameID, &aggregatestore.LoadOptions{ToVersion: 3})
	if err != nil {
		t.Fatal(err)
	}
	midGame := apply(t, NewGame(gameID),
		GameCreated{White: "Alice", Black: "Bob"},
		MoveMade{UCI: "e2e4"},
		MoveMade{UCI: "e7e5"},
	)
	if got := past.Entity(); got.FEN != midGame.FEN {
		t.Fatalf("FEN at v3 = %q, want %q", got.FEN, midGame.FEN)
	}

	// optimistic concurrency: both players save from the same version
	first, err := store.Load(ctx, gameID, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Load(ctx, gameID, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := first.Append(MoveMade{UCI: "b8c6"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, first, nil); err != nil {
		t.Fatal(err)
	}

	if err := second.Append(MoveMade{UCI: "g8f6"}); err != nil {
		t.Fatal(err)
	}
	err = store.Save(ctx, second, nil)
	if err == nil {
		t.Fatal("expected a version conflict saving from a stale version")
	}
	if !errors.Is(err, eventstore.StreamVersionMismatchError{}) {
		t.Fatalf("expected StreamVersionMismatchError, got: %v", err)
	}
}
