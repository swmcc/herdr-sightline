package engine

import "testing"

// Reconfigure clamps exactly as New does.
func TestReconfigureIsClamped(t *testing.T) {
	tests := []Options{
		{ContextLines: -5, MaxEntries: -1},
		{ContextLines: 20, MaxEntries: 0},
		{ContextLines: 100000, MaxEntries: 3},
		{ContextLines: 0, MaxEntries: 1 << 20},
	}
	for _, in := range tests {
		e := New(Options{ContextLines: 7, MaxEntries: 50})
		e.Reconfigure(in)
		want := New(in).opts
		if e.opts.ContextLines != want.ContextLines || e.opts.MaxEntries != want.MaxEntries {
			t.Errorf("Reconfigure(%+v) gave ContextLines %d MaxEntries %d, want %d and %d",
				in, e.opts.ContextLines, e.opts.MaxEntries, want.ContextLines, want.MaxEntries)
		}
	}
}
