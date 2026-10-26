package engine

import (
	"fmt"
	"testing"
	"time"
)

func TestMaxEntriesIsClamped(t *testing.T) {
	tests := []struct {
		name string
		in   int
		want int
	}{
		{"zero means the default", 0, 200},
		{"less than zero means the default", -5, 200},
		{"too small is raised", 1, 10},
		{"in range is kept", 500, 500},
		{"too big is lowered", 1_000_000, 10000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := New(Options{MaxEntries: tt.in}).opts.MaxEntries; got != tt.want {
				t.Fatalf("MaxEntries %d, want %d", got, tt.want)
			}
		})
	}
}

// file adds an entry the way group does, index and all.
func file(e *Engine, fp string, state State, lastSeen time.Time) *record {
	r := e.add(Entry{
		Source:      Source{ID: "web"},
		Fingerprint: fp,
		Count:       1,
		FirstSeen:   lastSeen,
		LastSeen:    lastSeen,
		State:       state,
	})
	if e.groups.newest == nil {
		e.groups.newest = make(map[groupKey]*record)
	}
	e.groups.newest[keyOf(r)] = r
	return r
}

// checkIndex fails if records, byID and the grouping index disagree.
func checkIndex(t *testing.T, e *Engine) {
	t.Helper()
	if len(e.byID) != len(e.records) {
		t.Fatalf("%d ids for %d records", len(e.byID), len(e.records))
	}
	for _, r := range e.records {
		if e.byID[r.entry.ID] != r {
			t.Fatalf("byID doesn't point at %s", r.entry.ID)
		}
	}
	for k, r := range e.groups.newest {
		if e.byID[r.entry.ID] != r {
			t.Fatalf("the index keeps %s for %q after it was dropped", r.entry.ID, k.fingerprint)
		}
		if keyOf(r) != k {
			t.Fatalf("%s is filed under the wrong key", r.entry.ID)
		}
	}
}

func TestDropOrder(t *testing.T) {
	type existing struct {
		state State
		ago   time.Duration // LastSeen, before t0
	}
	// Nine fillers that are never dropped make room for one more before the cap.
	tests := []struct {
		name     string
		existing []existing // filed in this order after the fillers
		wantGone int        // index into existing, or -1 for nothing
	}{
		{
			name:     "dismissed before an older finished entry",
			existing: []existing{{StateUnverified, time.Hour}, {StateDismissed, time.Minute}},
			wantGone: 1,
		},
		{
			name:     "fixed before an older new entry",
			existing: []existing{{StateNew, time.Hour}, {StateFixed, time.Minute}},
			wantGone: 1,
		},
		{
			name:     "unverified before an older new entry",
			existing: []existing{{StateNew, time.Hour}, {StateUnverified, time.Minute}},
			wantGone: 1,
		},
		{
			name:     "oldest new entry by last seen, not by when it was caught",
			existing: []existing{{StateNew, time.Minute}, {StateNew, time.Hour}},
			wantGone: 1,
		},
		{
			name:     "on a tie the one caught first goes",
			existing: []existing{{StateNew, time.Minute}, {StateNew, time.Minute}},
			wantGone: 0,
		},
		{
			name:     "sent and still failing are kept, over the cap",
			existing: []existing{{StateSent, time.Hour}, {StateStillFailing, time.Hour}},
			wantGone: -1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := New(Options{MaxEntries: 10})
			for i := range 10 - len(tt.existing) {
				file(e, fmt.Sprintf("filler %d", i), StateSent, t0.Add(-24*time.Hour))
			}
			var recs []*record
			for i, x := range tt.existing {
				recs = append(recs, file(e, fmt.Sprintf("existing %d", i), x.state, t0.Add(-x.ago)))
			}
			if e.dropped != 0 {
				t.Fatalf("dropped %d before the cap was reached", e.dropped)
			}

			just := file(e, "just caught", StateNew, t0)

			if _, ok := e.byID[just.entry.ID]; !ok {
				t.Fatalf("the entry just caught was dropped")
			}
			for i, r := range recs {
				_, kept := e.byID[r.entry.ID]
				if kept == (i == tt.wantGone) {
					t.Fatalf("existing %d (%v) kept %v", i, r.entry.State, kept)
				}
			}
			wantLen, wantDropped := 10, 1
			if tt.wantGone < 0 {
				wantLen, wantDropped = 11, 0
			}
			if len(e.records) != wantLen || e.dropped != wantDropped {
				t.Fatalf("%d records and %d dropped, want %d and %d", len(e.records), e.dropped, wantLen, wantDropped)
			}
			checkIndex(t, e)
		})
	}
}

// Dropping an old entry mustn't take the index away from a newer entry for the
// same failure, and dropping the newest must leave nothing behind.
func TestDropKeepsTheIndexFresh(t *testing.T) {
	e := New(Options{MaxEntries: 10})
	old := file(e, "boom", StateUnverified, t0)
	newer := file(e, "boom", StateNew, t0.Add(time.Second))
	key := keyOf(newer)
	for i := range 8 {
		file(e, fmt.Sprintf("other %d", i), StateNew, t0.Add(time.Duration(2+i)*time.Second))
	}

	file(e, "next", StateNew, t0.Add(time.Minute))
	if _, ok := e.byID[old.entry.ID]; ok {
		t.Fatalf("the finished entry wasn't dropped first")
	}
	if e.groups.newest[key] != newer {
		t.Fatalf("dropping the old entry took the index from the newer one")
	}
	checkIndex(t, e)

	file(e, "after", StateNew, t0.Add(2*time.Minute))
	if _, ok := e.byID[newer.entry.ID]; ok {
		t.Fatalf("the oldest new entry wasn't dropped")
	}
	if _, ok := e.groups.newest[key]; ok {
		t.Fatalf("the index still has a key for a dropped entry")
	}
	checkIndex(t, e)
}

// A flood through the whole engine leaves records, ids and the index in step.
func TestFloodLeavesNoStaleIndex(t *testing.T) {
	e := New(Options{
		Matchers:    []Matcher{Substring{Label: "generic", Text: "Error"}},
		CaptureWait: time.Second,
		Cooldown:    time.Minute,
		MaxEntries:  10,
	})
	e.Watch(Source{ID: "web"})
	now := t0
	for i := range 2000 {
		// Every fifth error is one seen before, so repeats and drops mix.
		line := fmt.Sprintf("Error: flood %d", i)
		if i%5 == 0 {
			line = fmt.Sprintf("Error: again %d", i%15)
		}
		e.Feed("web", []string{line}, now)
		now = now.Add(time.Second)
		e.Tick(now)
		if i%3 == 0 && len(e.records) > 0 {
			e.Dismiss(e.records[len(e.records)-1].entry.ID)
		}
	}
	if len(e.records) != 10 {
		t.Fatalf("%d records, want 10", len(e.records))
	}
	if len(e.groups.newest) > len(e.records) {
		t.Fatalf("the index has %d keys for %d records", len(e.groups.newest), len(e.records))
	}
	checkIndex(t, e)
}
