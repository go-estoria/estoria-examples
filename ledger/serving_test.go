package main

import (
	"testing"

	"github.com/go-estoria/estoria/projection"
	"github.com/go-estoria/estoria/projection/lifecycle"
)

func TestDesiredVersion(t *testing.T) {
	t.Parallel()

	v1 := projection.ID{Name: projectionName, Version: 1}
	v2 := projection.ID{Name: projectionName, Version: 2}

	for _, test := range []struct {
		name    string
		live    projection.ID
		attempt lifecycle.AttemptState
		want    projection.ID
	}{
		{
			name: "no live version tails nothing",
		},
		{
			name: "the live version is tailed at steady state",
			live: v1,
			want: v1,
		},
		{
			name:    "the live version is tailed while a rebuild builds alongside it",
			live:    v1,
			attempt: lifecycle.AttemptState{Phase: lifecycle.PhaseBuilding, Target: v2},
			want:    v1,
		},
		{
			name:    "a promoted target is left to the rebuild run",
			live:    v2,
			attempt: lifecycle.AttemptState{Phase: lifecycle.PhasePromoted, Target: v2, Previous: v1},
		},
		{
			name:    "a retiring target is left to the rebuild run",
			live:    v2,
			attempt: lifecycle.AttemptState{Phase: lifecycle.PhaseRetiring, Target: v2, Previous: v1},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := desiredVersion(test.live, test.attempt); got != test.want {
				t.Errorf("want %v, got %v", test.want, got)
			}
		})
	}
}
