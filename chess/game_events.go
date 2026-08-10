package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/go-estoria/estoria"
	"github.com/notnil/chess"
)

// Each event below implements estoria.DomainEvent[Game]. The prototypes are
// value-typed (New returns a value, not a pointer); estoria handles making
// them addressable for unmarshaling. ApplyTo implementations are total, pure
// state transitions: a persisted event is a fact, and applying one cannot
// fail. Move legality still lives here, in the domain, not in HTTP handlers —
// as Validate methods that command handlers run before appending an event, so
// an event that would produce an illegal position never reaches the stream.

// A gameEvent is a domain event that can validate itself against the state it
// would apply to. Command handlers call Validate before appending the event.
type gameEvent interface {
	estoria.DomainEvent[Game]
	Validate(g Game) error
}

// GameCreated initializes a game with two named players at the standard
// starting position.
type GameCreated struct {
	White string `json:"white"`
	Black string `json:"black"`
}

func (GameCreated) EventType() string              { return "gamecreated" }
func (GameCreated) New() estoria.DomainEvent[Game] { return GameCreated{} }

// Validate rejects creating a game that already exists.
func (GameCreated) Validate(g Game) error {
	if g.Created() {
		return errors.New("game already created")
	}
	return nil
}

func (e GameCreated) ApplyTo(g Game) Game {
	if g.Created() {
		return g
	}

	next := g.clone()
	next.White = e.White
	next.Black = e.Black
	next.MovesUCI = []string{}
	next.syncFromEngine(chess.NewGame())
	return next
}

// MoveMade applies one move, given in UCI notation ("e2e4", "e7e8q"). The
// move is validated against the position reached by replaying every prior
// move, so an illegal move — or any move after the game is over — is rejected
// by Validate before it is appended.
type MoveMade struct {
	UCI string `json:"uci"`
}

func (MoveMade) EventType() string              { return "movemade" }
func (MoveMade) New() estoria.DomainEvent[Game] { return MoveMade{} }

// Validate rejects a move that is illegal in the game's current position, or
// any move on a game that does not exist or is already over.
func (e MoveMade) Validate(g Game) error {
	if err := inProgress(g); err != nil {
		return err
	}

	_, err := e.engineAfterMove(g)
	return err
}

func (e MoveMade) ApplyTo(g Game) Game {
	if inProgress(g) != nil {
		return g
	}

	game, err := e.engineAfterMove(g)
	if err != nil {
		return g
	}

	next := g.clone()
	next.MovesUCI = append(next.MovesUCI, e.UCI)
	next.syncFromEngine(game)
	return next
}

// engineAfterMove replays the game's prior moves and applies this move,
// returning the resulting rules-engine state. Each step is validated by the
// rules engine, so an illegal or undecodable move surfaces as an error.
func (e MoveMade) engineAfterMove(g Game) (*chess.Game, error) {
	game, err := g.rebuild()
	if err != nil {
		return nil, fmt.Errorf("rebuilding position: %w", err)
	}

	move, err := chess.UCINotation{}.Decode(game.Position(), e.UCI)
	if err != nil {
		return nil, fmt.Errorf("invalid move %q", e.UCI)
	}
	if err := game.Move(move); err != nil {
		return nil, fmt.Errorf("illegal move %q", e.UCI)
	}
	return game, nil
}

// PlayerResigned ends the game in favor of the opponent. Resigning is only
// possible while the game is in progress.
type PlayerResigned struct {
	Color string `json:"color"` // "white" or "black"
}

func (PlayerResigned) EventType() string              { return "playerresigned" }
func (PlayerResigned) New() estoria.DomainEvent[Game] { return PlayerResigned{} }

// Validate rejects resigning with an unknown color, or resigning a game that
// does not exist or is already over.
func (e PlayerResigned) Validate(g Game) error {
	if err := inProgress(g); err != nil {
		return err
	}

	_, err := resigningColor(e.Color)
	return err
}

func (e PlayerResigned) ApplyTo(g Game) Game {
	if inProgress(g) != nil {
		return g
	}

	color, err := resigningColor(e.Color)
	if err != nil {
		return g
	}

	game, err := g.rebuild()
	if err != nil {
		return g
	}
	game.Resign(color)

	next := g.clone()
	next.syncFromEngine(game)
	return next
}

// resigningColor parses a resignation color name.
func resigningColor(name string) (chess.Color, error) {
	switch name {
	case "white":
		return chess.White, nil
	case "black":
		return chess.Black, nil
	default:
		return chess.NoColor, fmt.Errorf("invalid color %q", name)
	}
}

// inProgress reports whether the game can accept a move or a resignation,
// returning the reason it cannot.
func inProgress(g Game) error {
	if !g.Created() {
		return errors.New("game does not exist")
	}
	if g.Over() {
		return fmt.Errorf("game is over (%s by %s)", g.Outcome, strings.ToLower(g.Method))
	}
	return nil
}

// gameEventPrototypes lists every event type for registration with the
// aggregate store.
func gameEventPrototypes() []estoria.DomainEvent[Game] {
	return []estoria.DomainEvent[Game]{
		GameCreated{},
		MoveMade{},
		PlayerResigned{},
	}
}
