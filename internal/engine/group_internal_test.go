package engine

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCut(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		n       int
		wantLen int
	}{
		{"short lines are left alone", "abc", 10, 3},
		{"long lines are cut", strings.Repeat("a", 20), 10, 10},
		{"a character across the cut is dropped whole", strings.Repeat("a", 9) + "é", 10, 9},
		{"bytes that aren't UTF-8 are cut, not thrown away", strings.Repeat("\x80", 20), 10, 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cut(tt.in, tt.n)
			if len(got) != tt.wantLen {
				t.Fatalf("cut to %d bytes, want %d", len(got), tt.wantLen)
			}
			if utf8.ValidString(tt.in) && !utf8.ValidString(got) {
				t.Fatalf("cut split a character: %q", got)
			}
		})
	}
}
