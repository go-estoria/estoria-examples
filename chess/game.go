package main

import (
	"fmt"

	"github.com/go-estoria/estoria/typeid"
	"github.com/gofrs/uuid/v5"
	"github.com/notnil/chess"
)

// A Game is the aggregate root for a single chess game. Its state is derived
// entirely by applying events — a game IS its sequence of moves, so replaying
// the stream to any version reproduces the exact position on the board.
type Game struct {
	ID       uuid.UUID `json:"id"`
	White    string    `json:"white"`
	Black    string    `json:"black"`
	MovesUCI []string  `json:"movesUci"`
	FEN      string    `json:"fen"`
	Turn     string    `json:"turn"`    // "white" or "black"
	Outcome  string    `json:"outcome"` // "*", "1-0", "0-1", or "1/2-1/2"
	Method   string    `json:"method"`  // "Checkmate", "Stalemate", "Resignation", ...
	Check    bool      `json:"check"`   // the side to move is in check
}

// NewGame is the estoria.EntityFactory for Game aggregates.
func NewGame(id uuid.UUID) Game {
	return Game{ID: id}
}

// EntityID implements estoria.Entity.
func (g Game) EntityID() typeid.ID {
	return typeid.New("game", g.ID)
}

// Created reports whether the game has been initialized by a GameCreated event.
func (g Game) Created() bool {
	return g.FEN != ""
}

// Over reports whether the game has concluded (by checkmate, stalemate,
// resignation, or any other method).
func (g Game) Over() bool {
	return g.Created() && g.Outcome != string(chess.NoOutcome)
}

// clone returns a copy of the game with its own moves slice, so that ApplyTo
// implementations can return new state without mutating slices shared with
// previous versions.
func (g Game) clone() Game {
	c := g
	c.MovesUCI = make([]string, len(g.MovesUCI))
	copy(c.MovesUCI, g.MovesUCI)
	return c
}

// rebuild reconstructs the full rules-engine state by replaying the game's
// moves from the starting position. Rebuilding from scratch on every apply is
// O(n) per event, which is fine: chess streams are short, and it keeps the
// entity free of unexported engine state (it stays a plain, marshalable value).
func (g Game) rebuild() (*chess.Game, error) {
	return replayUCI(g.MovesUCI)
}

// replayUCI builds a chess game by applying UCI moves from the standard
// starting position. Each move is validated by the rules engine as it is
// applied, so a corrupt or illegal sequence surfaces as an error.
func replayUCI(movesUCI []string) (*chess.Game, error) {
	game := chess.NewGame()
	for i, uci := range movesUCI {
		move, err := chess.UCINotation{}.Decode(game.Position(), uci)
		if err != nil {
			return nil, fmt.Errorf("decoding move %d (%q): %w", i+1, uci, err)
		}
		if err := game.Move(move); err != nil {
			return nil, fmt.Errorf("applying move %d (%q): %w", i+1, uci, err)
		}
	}
	return game, nil
}

// sanHistory replays a UCI move sequence and renders each move in standard
// algebraic notation ("e4", "Nf3", "Qxf7#", ...) for display.
func sanHistory(movesUCI []string) ([]string, error) {
	game := chess.NewGame()
	san := make([]string, 0, len(movesUCI))
	for i, uci := range movesUCI {
		pos := game.Position()
		move, err := chess.UCINotation{}.Decode(pos, uci)
		if err != nil {
			return nil, fmt.Errorf("decoding move %d (%q): %w", i+1, uci, err)
		}
		if err := game.Move(move); err != nil {
			return nil, fmt.Errorf("applying move %d (%q): %w", i+1, uci, err)
		}
		// encode the engine-validated move, which carries the tags ("+", "#")
		// that algebraic notation renders
		applied := game.Moves()
		san = append(san, chess.AlgebraicNotation{}.Encode(pos, applied[len(applied)-1]))
	}
	return san, nil
}

// syncFromEngine refreshes the entity's derived fields (FEN, turn, outcome,
// method, check) from a rebuilt rules-engine game.
func (g *Game) syncFromEngine(game *chess.Game) {
	g.FEN = game.FEN()
	g.Turn = colorName(game.Position().Turn())
	g.Outcome = string(game.Outcome())
	if game.Method() == chess.NoMethod {
		g.Method = ""
	} else {
		g.Method = game.Method().String()
	}

	// "check" is only meaningful while the game is in progress; a mating move
	// ends the game rather than leaving a check pending.
	g.Check = false
	if game.Outcome() == chess.NoOutcome {
		if moves := game.Moves(); len(moves) > 0 {
			g.Check = moves[len(moves)-1].HasTag(chess.Check)
		}
	}
}

// colorName renders a chess color as the lowercase name used throughout the
// API ("white" or "black").
func colorName(c chess.Color) string {
	if c == chess.White {
		return "white"
	}
	return "black"
}
