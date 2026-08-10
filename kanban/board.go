package main

import (
	"github.com/gofrs/uuid/v5"
)

// A Board is the aggregate root for a kanban board. Its state is derived
// entirely by applying events; it is never mutated directly.
type Board struct {
	ID      uuid.UUID `json:"id"`
	Name    string    `json:"name"`
	Columns []Column  `json:"columns"`
}

// A Column is an ordered lane of cards on a board.
type Column struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Cards []Card `json:"cards"`
}

// A Card is a single work item on a board.
type Card struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Color       string `json:"color,omitempty"`
}

// NewBoard is the estoria.StateFactory for Board aggregates.
func NewBoard(id uuid.UUID) Board {
	return Board{ID: id}
}

// clone returns a deep copy of the board so that ApplyTo implementations can
// return new state without mutating slices shared with previous versions.
func (b Board) clone() Board {
	c := b
	c.Columns = make([]Column, len(b.Columns))
	for i, col := range b.Columns {
		c.Columns[i] = col
		c.Columns[i].Cards = make([]Card, len(col.Cards))
		copy(c.Columns[i].Cards, col.Cards)
	}
	return c
}

// column returns a pointer to the column with the given ID, or nil.
func (b *Board) column(id string) *Column {
	for i := range b.Columns {
		if b.Columns[i].ID == id {
			return &b.Columns[i]
		}
	}
	return nil
}

// findCard returns the column index and card index of the card with the given
// ID, or (-1, -1) if the card does not exist on the board.
func (b *Board) findCard(id string) (colIdx, cardIdx int) {
	for i := range b.Columns {
		for j := range b.Columns[i].Cards {
			if b.Columns[i].Cards[j].ID == id {
				return i, j
			}
		}
	}
	return -1, -1
}

// HasCard reports whether a card with the given ID exists on the board.
func (b Board) HasCard(id string) bool {
	colIdx, _ := b.findCard(id)
	return colIdx >= 0
}

// HasColumn reports whether a column with the given ID exists on the board.
func (b Board) HasColumn(id string) bool {
	return b.column(id) != nil
}
