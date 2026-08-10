package main

import (
	"github.com/go-estoria/estoria"
)

// Each event below implements estoria.DomainEvent[Board]. The prototypes are
// value-typed (New returns a value, not a pointer); estoria handles making
// them addressable for unmarshaling. ApplyTo implementations are pure, total
// state transitions: they clone the board, apply the change, and return the
// result. Commands are validated in the HTTP handlers before events are
// appended (see server.go); an event that references a card or column that no
// longer exists leaves the board unchanged.

// BoardCreated initializes a board with a name.
type BoardCreated struct {
	Name string `json:"name"`
}

func (BoardCreated) EventType() string               { return "boardcreated" }
func (BoardCreated) New() estoria.DomainEvent[Board] { return BoardCreated{} }
func (e BoardCreated) ApplyTo(b Board) Board {
	next := b.clone()
	next.Name = e.Name
	return next
}

// BoardRenamed changes the board's name.
type BoardRenamed struct {
	Name string `json:"name"`
}

func (BoardRenamed) EventType() string               { return "boardrenamed" }
func (BoardRenamed) New() estoria.DomainEvent[Board] { return BoardRenamed{} }
func (e BoardRenamed) ApplyTo(b Board) Board {
	next := b.clone()
	next.Name = e.Name
	return next
}

// ColumnAdded appends a new empty column to the board.
type ColumnAdded struct {
	ColumnID string `json:"columnId"`
	Title    string `json:"title"`
}

func (ColumnAdded) EventType() string               { return "columnadded" }
func (ColumnAdded) New() estoria.DomainEvent[Board] { return ColumnAdded{} }
func (e ColumnAdded) ApplyTo(b Board) Board {
	next := b.clone()
	next.Columns = append(next.Columns, Column{ID: e.ColumnID, Title: e.Title, Cards: []Card{}})
	return next
}

// ColumnRenamed changes a column's title.
type ColumnRenamed struct {
	ColumnID string `json:"columnId"`
	Title    string `json:"title"`
}

func (ColumnRenamed) EventType() string               { return "columnrenamed" }
func (ColumnRenamed) New() estoria.DomainEvent[Board] { return ColumnRenamed{} }
func (e ColumnRenamed) ApplyTo(b Board) Board {
	next := b.clone()
	col := next.column(e.ColumnID)
	if col == nil {
		return b
	}

	col.Title = e.Title
	return next
}

// CardAdded places a new card at the end of a column.
type CardAdded struct {
	CardID      string `json:"cardId"`
	ColumnID    string `json:"columnId"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Color       string `json:"color,omitempty"`
}

func (CardAdded) EventType() string               { return "cardadded" }
func (CardAdded) New() estoria.DomainEvent[Board] { return CardAdded{} }
func (e CardAdded) ApplyTo(b Board) Board {
	next := b.clone()
	col := next.column(e.ColumnID)
	if col == nil {
		return b
	}

	col.Cards = append(col.Cards, Card{
		ID:          e.CardID,
		Title:       e.Title,
		Description: e.Description,
		Color:       e.Color,
	})
	return next
}

// CardEdited replaces a card's title, description, and color.
type CardEdited struct {
	CardID      string `json:"cardId"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Color       string `json:"color,omitempty"`
}

func (CardEdited) EventType() string               { return "cardedited" }
func (CardEdited) New() estoria.DomainEvent[Board] { return CardEdited{} }
func (e CardEdited) ApplyTo(b Board) Board {
	next := b.clone()
	colIdx, cardIdx := next.findCard(e.CardID)
	if colIdx < 0 {
		return b
	}

	card := &next.Columns[colIdx].Cards[cardIdx]
	card.Title = e.Title
	card.Description = e.Description
	card.Color = e.Color
	return next
}

// CardMoved relocates a card to a position within a column (possibly the same one).
type CardMoved struct {
	CardID   string `json:"cardId"`
	ToColumn string `json:"toColumnId"`
	ToIndex  int    `json:"toIndex"`
}

func (CardMoved) EventType() string               { return "cardmoved" }
func (CardMoved) New() estoria.DomainEvent[Board] { return CardMoved{} }
func (e CardMoved) ApplyTo(b Board) Board {
	next := b.clone()

	colIdx, cardIdx := next.findCard(e.CardID)
	if colIdx < 0 {
		return b
	}

	dest := next.column(e.ToColumn)
	if dest == nil {
		return b
	}

	src := &next.Columns[colIdx]
	card := src.Cards[cardIdx]
	src.Cards = append(src.Cards[:cardIdx], src.Cards[cardIdx+1:]...)

	idx := min(max(e.ToIndex, 0), len(dest.Cards))
	dest.Cards = append(dest.Cards[:idx], append([]Card{card}, dest.Cards[idx:]...)...)
	return next
}

// CardRemoved deletes a card from the board.
type CardRemoved struct {
	CardID string `json:"cardId"`
}

func (CardRemoved) EventType() string               { return "cardremoved" }
func (CardRemoved) New() estoria.DomainEvent[Board] { return CardRemoved{} }
func (e CardRemoved) ApplyTo(b Board) Board {
	next := b.clone()
	colIdx, cardIdx := next.findCard(e.CardID)
	if colIdx < 0 {
		return b
	}

	col := &next.Columns[colIdx]
	col.Cards = append(col.Cards[:cardIdx], col.Cards[cardIdx+1:]...)
	return next
}

// boardEventPrototypes lists every event type for registration with the
// aggregate store and for decoding raw stream events in the activity feed.
func boardEventPrototypes() []estoria.DomainEvent[Board] {
	return []estoria.DomainEvent[Board]{
		BoardCreated{},
		BoardRenamed{},
		ColumnAdded{},
		ColumnRenamed{},
		CardAdded{},
		CardEdited{},
		CardMoved{},
		CardRemoved{},
	}
}
