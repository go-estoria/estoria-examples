package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-estoria/estoria"
	"github.com/notnil/chess"
)

// Each event below implements estoria.EntityEvent[Game]. The prototypes are
// value-typed (New returns a value, not a pointer); estoria handles making
// them addressable for unmarshaling. ApplyTo implementations are pure state
// transitions — and in chess, they are also the rules engine's gate: an event
// that would produce an illegal position returns an error instead of new
// state. Move legality lives here, in the domain, not in HTTP handlers.

// GameCreated initializes a game with two named players at the standard
// starting position.
type GameCreated struct {
	White string `json:"white"`
	Black string `json:"black"`
}

func (GameCreated) EventType() string              { return "gamecreated" }
func (GameCreated) New() estoria.EntityEvent[Game] { return GameCreated{} }
func (e GameCreated) ApplyTo(_ context.Context, g Game) (Game, error) {
	if g.Created() {
		return g, errors.New("game already created")
	}

	next := g.clone()
	next.White = e.White
	next.Black = e.Black
	next.MovesUCI = []string{}
	next.syncFromEngine(chess.NewGame())
	return next, nil
}

// MoveMade applies one move, given in UCI notation ("e2e4", "e7e8q"). The
// move is validated against the position reached by replaying every prior
// move, so an illegal move — or any move after the game is over — is rejected
// and the game state is unchanged.
type MoveMade struct {
	UCI string `json:"uci"`
}

func (MoveMade) EventType() string              { return "movemade" }
func (MoveMade) New() estoria.EntityEvent[Game] { return MoveMade{} }
func (e MoveMade) ApplyTo(_ context.Context, g Game) (Game, error) {
	if !g.Created() {
		return g, errors.New("game does not exist")
	}
	if g.Over() {
		return g, fmt.Errorf("game is over (%s by %s)", g.Outcome, strings.ToLower(g.Method))
	}

	game, err := g.rebuild()
	if err != nil {
		return g, fmt.Errorf("rebuilding position: %w", err)
	}

	move, err := chess.UCINotation{}.Decode(game.Position(), e.UCI)
	if err != nil {
		return g, fmt.Errorf("invalid move %q", e.UCI)
	}
	if err := game.Move(move); err != nil {
		return g, fmt.Errorf("illegal move %q", e.UCI)
	}

	next := g.clone()
	next.MovesUCI = append(next.MovesUCI, e.UCI)
	next.syncFromEngine(game)
	return next, nil
}

// PlayerResigned ends the game in favor of the opponent. Resigning is only
// possible while the game is in progress.
type PlayerResigned struct {
	Color string `json:"color"` // "white" or "black"
}

func (PlayerResigned) EventType() string              { return "playerresigned" }
func (PlayerResigned) New() estoria.EntityEvent[Game] { return PlayerResigned{} }
func (e PlayerResigned) ApplyTo(_ context.Context, g Game) (Game, error) {
	if !g.Created() {
		return g, errors.New("game does not exist")
	}
	if g.Over() {
		return g, fmt.Errorf("game is over (%s by %s)", g.Outcome, strings.ToLower(g.Method))
	}

	var color chess.Color
	switch e.Color {
	case "white":
		color = chess.White
	case "black":
		color = chess.Black
	default:
		return g, fmt.Errorf("invalid color %q", e.Color)
	}

	game, err := g.rebuild()
	if err != nil {
		return g, fmt.Errorf("rebuilding position: %w", err)
	}
	game.Resign(color)

	next := g.clone()
	next.syncFromEngine(game)
	return next, nil
}

// gameEventPrototypes lists every event type for registration with the
// aggregate store.
func gameEventPrototypes() []estoria.EntityEvent[Game] {
	return []estoria.EntityEvent[Game]{
		GameCreated{},
		MoveMade{},
		PlayerResigned{},
	}
}
