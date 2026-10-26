package engine

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestScanned(t *testing.T) {
	tests := []struct {
		name, line, want string
	}{
		{"short line is untouched", "Error: boom", "Error: boom"},
		{"exactly the head is untouched", strings.Repeat("x", maxScannedLine), strings.Repeat("x", maxScannedLine)},
		{"one byte over", strings.Repeat("x", maxScannedLine+1), strings.Repeat("x", maxScannedLine) + moreText},
		// The cut never splits a character, so a wide one straddling it goes.
		{"wide character at the cut", strings.Repeat("x", maxScannedLine-1) + "é", strings.Repeat("x", maxScannedLine-1) + moreText},
		{"four byte character at the cut", strings.Repeat("x", maxScannedLine-2) + "😀", strings.Repeat("x", maxScannedLine-2) + moreText},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scanned(tt.line)
			if got != tt.want {
				t.Fatalf("scanned = %d bytes ending %q, want %d bytes ending %q", len(got), got[max(0, len(got)-8):], len(tt.want), tt.want[max(0, len(tt.want)-8):])
			}
			if !utf8.ValidString(got) {
				t.Fatalf("scanned split a character")
			}
		})
	}
	// What's kept for the message is the head that was matched.
	line := strings.Repeat("é", maxScannedLine)
	if kept, head := capStored(line), scanned(line); !strings.HasPrefix(kept, strings.TrimSuffix(head, moreText)+" [") {
		t.Fatalf("kept line doesn't start with the matched head")
	}
}
