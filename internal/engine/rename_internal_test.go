package engine

import (
	"testing"
	"time"
)

// After a rename the grouping index has no key under the old id, and every
// entry is filed under the key it can be rebuilt from.
func TestRenameLeavesNoStaleIndex(t *testing.T) {
	e := New(Options{})
	e.Watch(Source{ID: "web"})
	e.Watch(Source{ID: "api"})
	now := time.Now()
	file(e, "a", StateNew, now)
	file(e, "b", StateSent, now)
	other := e.add(Entry{Source: Source{ID: "api"}, Fingerprint: "a", State: StateNew})
	e.groups.newest[keyOf(other)] = other

	if !e.Rename("web", "moved") {
		t.Fatal("Rename moved nothing")
	}
	checkIndex(t, e)
	for k := range e.groups.newest {
		if k.source == "web" {
			t.Fatalf("the index still has %q under the old id", k.fingerprint)
		}
	}
	if len(e.groups.newest) != 3 {
		t.Fatalf("index has %d keys, want 3", len(e.groups.newest))
	}
	if _, ok := e.sources["web"]; ok {
		t.Fatal("the old id is still watched")
	}
	if s, ok := e.sources["moved"]; !ok || s.info.ID != "moved" {
		t.Fatal("the source isn't watched under its new id")
	}
	if e.groups.newest[groupKey{"api", "a"}] != other {
		t.Fatal("another source's entry was moved")
	}
}
