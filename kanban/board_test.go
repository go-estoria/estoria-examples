package main

import (
	"context"
	"errors"
	"testing"

	"github.com/go-estoria/estoria"
	"github.com/go-estoria/estoria/aggregatestore"
	"github.com/go-estoria/estoria/eventstore"
	"github.com/go-estoria/estoria/eventstore/memory"
	"github.com/gofrs/uuid/v5"
)

// Event-sourced domains are easy to test: given a board, apply an event,
// assert on the resulting state. No storage, no mocks.

func TestEventApplication(t *testing.T) {
	t.Parallel()

	base := func() Board {
		board := NewBoard(uuid.Must(uuid.NewV4()))
		for _, event := range []estoria.EntityEvent[Board]{
			BoardCreated{Name: "Test"},
			ColumnAdded{ColumnID: "todo", Title: "To Do"},
			ColumnAdded{ColumnID: "done", Title: "Done"},
			CardAdded{CardID: "c1", ColumnID: "todo", Title: "first"},
			CardAdded{CardID: "c2", ColumnID: "todo", Title: "second"},
		} {
			var err error
			if board, err = event.ApplyTo(context.Background(), board); err != nil {
				t.Fatalf("applying setup event: %v", err)
			}
		}
		return board
	}

	t.Run("moves a card between columns", func(t *testing.T) {
		t.Parallel()
		board, err := CardMoved{CardID: "c1", ToColumn: "done", ToIndex: 0}.ApplyTo(context.Background(), base())
		if err != nil {
			t.Fatal(err)
		}

		if got := board.column("todo").Cards; len(got) != 1 || got[0].ID != "c2" {
			t.Errorf("todo column = %+v, want only c2", got)
		}
		if got := board.column("done").Cards; len(got) != 1 || got[0].ID != "c1" {
			t.Errorf("done column = %+v, want only c1", got)
		}
	})

	t.Run("clamps an out-of-range move index", func(t *testing.T) {
		t.Parallel()
		board, err := CardMoved{CardID: "c1", ToColumn: "todo", ToIndex: 99}.ApplyTo(context.Background(), base())
		if err != nil {
			t.Fatal(err)
		}

		if got := board.column("todo").Cards; len(got) != 2 || got[1].ID != "c1" {
			t.Errorf("todo column = %+v, want c1 moved to the end", got)
		}
	})

	t.Run("edits a card in place", func(t *testing.T) {
		t.Parallel()
		board, err := CardEdited{CardID: "c2", Title: "renamed", Color: "teal"}.ApplyTo(context.Background(), base())
		if err != nil {
			t.Fatal(err)
		}

		card := board.column("todo").Cards[1]
		if card.Title != "renamed" || card.Color != "teal" {
			t.Errorf("card = %+v, want title 'renamed' and color 'teal'", card)
		}
	})

	t.Run("removes a card", func(t *testing.T) {
		t.Parallel()
		board, err := CardRemoved{CardID: "c1"}.ApplyTo(context.Background(), base())
		if err != nil {
			t.Fatal(err)
		}

		if board.HasCard("c1") {
			t.Error("card c1 still present after removal")
		}
	})

	t.Run("rejects invalid transitions", func(t *testing.T) {
		t.Parallel()
		for name, event := range map[string]estoria.EntityEvent[Board]{
			"add to unknown column":  CardAdded{CardID: "c9", ColumnID: "nope", Title: "x"},
			"add duplicate card":     CardAdded{CardID: "c1", ColumnID: "todo", Title: "x"},
			"move unknown card":      CardMoved{CardID: "nope", ToColumn: "done"},
			"move to unknown column": CardMoved{CardID: "c1", ToColumn: "nope"},
			"edit unknown card":      CardEdited{CardID: "nope", Title: "x"},
			"remove unknown card":    CardRemoved{CardID: "nope"},
			"add duplicate column":   ColumnAdded{ColumnID: "todo", Title: "x"},
			"rename unknown column":  ColumnRenamed{ColumnID: "nope", Title: "x"},
		} {
			if _, err := event.ApplyTo(context.Background(), base()); err == nil {
				t.Errorf("%s: expected an error", name)
			}
		}
	})

	t.Run("does not mutate the input board", func(t *testing.T) {
		t.Parallel()
		before := base()
		if _, err := (CardMoved{CardID: "c1", ToColumn: "done", ToIndex: 0}).ApplyTo(context.Background(), before); err != nil {
			t.Fatal(err)
		}

		if got := before.column("todo").Cards; len(got) != 2 {
			t.Errorf("input board was mutated: todo column = %+v", got)
		}
	})
}

// TestBoardRoundTrip runs the full aggregate lifecycle against estoria's
// in-memory event store: save, load, time-travel, and conflict detection.
func TestBoardRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	eventStore, err := memory.NewEventStore()
	if err != nil {
		t.Fatal(err)
	}

	store, err := aggregatestore.New(eventStore, NewBoard,
		aggregatestore.WithEventTypes(boardEventPrototypes()...))
	if err != nil {
		t.Fatal(err)
	}

	boardID := uuid.Must(uuid.NewV4())

	agg := store.New(boardID)
	if err := agg.Append(
		BoardCreated{Name: "Round Trip"},
		ColumnAdded{ColumnID: "todo", Title: "To Do"},
		CardAdded{CardID: "c1", ColumnID: "todo", Title: "hello"},
	); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, agg, nil); err != nil {
		t.Fatal(err)
	}

	// load the latest state
	loaded, err := store.Load(ctx, boardID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v := loaded.Version(); v != 3 {
		t.Fatalf("loaded version = %d, want 3", v)
	}
	if board := loaded.Entity(); !board.HasCard("c1") || board.Name != "Round Trip" {
		t.Fatalf("loaded board = %+v, want name 'Round Trip' with card c1", board)
	}

	// time travel: before the card was added
	past, err := store.Load(ctx, boardID, &aggregatestore.LoadOptions{ToVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	if board := past.Entity(); board.HasCard("c1") || !board.HasColumn("todo") {
		t.Fatalf("board at v2 = %+v, want the column but not the card", board)
	}

	// optimistic concurrency: two writers save from the same version
	first, err := store.Load(ctx, boardID, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Load(ctx, boardID, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := first.Append(CardAdded{CardID: "c2", ColumnID: "todo", Title: "winner"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, first, nil); err != nil {
		t.Fatal(err)
	}

	if err := second.Append(CardAdded{CardID: "c3", ColumnID: "todo", Title: "loser"}); err != nil {
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
