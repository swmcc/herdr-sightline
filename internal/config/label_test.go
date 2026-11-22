package config

import (
	"testing"

	"github.com/swmcc/herdr-sightline/internal/engine"
)

// config can't import the engine, so it keeps its own copy of the label
// shapes. If the engine learns a new label, a project's ignore rule could name
// that label to hide everything a process prints, so the two must agree.
func TestLabelShapeMatchesTheEngine(t *testing.T) {
	if want := engine.LabelPrefix(); labelShape+"?" != want {
		t.Errorf("labelShape is %q, but the engine's labelPrefix is %q", labelShape+"?", want)
	}
}
