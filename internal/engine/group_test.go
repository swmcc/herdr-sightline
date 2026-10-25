package engine_test

import (
	"slices"
	"testing"
	"time"

	"github.com/swmcc/herdr-sightline/internal/engine/enginetest"
)

func TestGrouping(t *testing.T) {
	tests := []struct {
		name      string
		script    []enginetest.Step
		dismiss   bool // dismiss the first entry after the first step
		wantCount []int
	}{
		{
			name: "a crash loop is one entry with a count",
			script: []enginetest.Step{
				{Source: web, Lines: []string{"Error: EADDRINUSE"}},
				{Wait: 5 * time.Second, Source: web, Lines: []string{"Error: EADDRINUSE"}},
				{Wait: 5 * time.Second, Source: web, Lines: []string{"Error: EADDRINUSE"}},
				{Wait: time.Second},
			},
			wantCount: []int{3},
		},
		{
			name: "a different error gets its own entry",
			script: []enginetest.Step{
				{Source: web, Lines: []string{"Error: one"}},
				{Wait: 5 * time.Second, Source: web, Lines: []string{"Error: two"}},
				{Wait: time.Second},
			},
			wantCount: []int{1, 1},
		},
		{
			name: "a dismissed error repeating inside the cooldown stays dismissed",
			script: []enginetest.Step{
				{Source: web, Lines: []string{"Error: loop"}},
				{Wait: 10 * time.Second, Source: web, Lines: []string{"Error: loop"}},
				{Wait: time.Second},
			},
			dismiss:   true,
			wantCount: []int{2},
		},
		{
			name: "a dismissed error coming back after the cooldown is new",
			script: []enginetest.Step{
				{Source: web, Lines: []string{"Error: loop"}},
				{Wait: 2 * time.Minute, Source: web, Lines: []string{"Error: loop"}},
				{Wait: time.Second},
			},
			dismiss:   true,
			wantCount: []int{1, 1},
		},
		{
			name: "ignored lines never alert",
			script: []enginetest.Step{
				{Source: web, Lines: []string{"DeprecationWarning: Error handling is changing"}},
				{Wait: time.Second},
			},
		},
		{
			name: "lines from an unwatched source are dropped",
			script: []enginetest.Step{
				{Source: "pane-2", Lines: []string{"Error: elsewhere"}},
				{Wait: time.Second},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newAdapter(t)
			a.Run(tt.script[:1])
			a.Advance(time.Second)
			if tt.dismiss {
				a.Engine.Dismiss(only(t, a).ID)
			}
			a.Run(tt.script[1:])
			var counts []int
			for _, en := range a.Engine.Entries() {
				counts = append(counts, en.Count)
			}
			if !slices.Equal(counts, tt.wantCount) {
				t.Fatalf("counts newest first = %v, want %v", counts, tt.wantCount)
			}
		})
	}
}

func TestRepeatUpdatesLastSeen(t *testing.T) {
	a := newAdapter(t)
	a.Print(web, "Error: again")
	a.Advance(5 * time.Second)
	a.Print(web, "Error: again")
	a.Advance(time.Second)
	en := only(t, a)
	if want := enginetest.Start().Add(5 * time.Second); !en.LastSeen.Equal(want) || !en.FirstSeen.Equal(enginetest.Start()) {
		t.Fatalf("FirstSeen %v LastSeen %v, want LastSeen %v", en.FirstSeen, en.LastSeen, want)
	}
}
