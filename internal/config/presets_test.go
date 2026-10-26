package config_test

import (
	"slices"
	"testing"

	"github.com/swmcc/herdr-sightline/internal/config"
	"github.com/swmcc/herdr-sightline/internal/engine"
)

// TestPresetsMatchEngine keeps the names sightline.toml accepts in step with the
// presets the engine has. The engine can't import config, so the check lives here.
func TestPresetsMatchEngine(t *testing.T) {
	got := config.Presets()
	want := engine.PresetNames()
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("config.Presets() = %v, engine.PresetNames() = %v", got, want)
	}
	if _, err := engine.Presets(config.Presets()...); err != nil {
		t.Fatal(err)
	}
}
