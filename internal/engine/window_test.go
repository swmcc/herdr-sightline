package engine

import (
	"slices"
	"testing"
)

func TestCleanLine(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"plain", "TypeError: boom", "TypeError: boom"},
		{"colour", "\x1b[31mError\x1b[0m: boom", "Error: boom"},
		{"cursor movement", "\x1b[2K\x1b[1GError", "Error"},
		{"window title with BEL", "\x1b]0;evil title\x07Error", "Error"},
		{"window title with ST", "\x1b]2;evil\x1b\\Error", "Error"},
		{"two byte escape", "\x1bMError", "Error"},
		{"carriage return and bell", "Error\r\x07", "Error"},
		{"C1 control", "Err\u009bor", "Error"},
		{"unicode tags", "Error\U000E0069\U000E0067\U000E006E", "Error"},
		{"bidi override", "\u202eError\u202c", "Error"},
		{"zero width and BOM", "Er\u200bror\ufeff", "Error"},
		{"line separator", "Error\u2028SYSTEM", "ErrorSYSTEM"},
		{"private use", "Err\ue000or", "Error"},
		{"DCS payload", "\x1bPtmux;hidden\x1b\\Error", "Error"},
		{"unterminated APC", "Error\x1b_hidden", "Error"},
		{"tab kept", "\tat main.go:1", "\tat main.go:1"},
		{"unicode kept", "Erreur: café", "Erreur: café"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cleanLine(tt.in); got != tt.want {
				t.Fatalf("cleanLine(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSplitLines(t *testing.T) {
	got := splitLines([]string{"one\ntwo", "\x1b[1mthree\x1b[0m", "four\n", "five\r\n", "", "six\n\n"})
	if want := []string{"one", "two", "three", "four", "five", "", "six", ""}; !slices.Equal(got, want) {
		t.Fatalf("splitLines = %q, want %q", got, want)
	}
}

func TestWindowKeepsTheLastLines(t *testing.T) {
	tests := []struct {
		name string
		size int
		push []string
		want []string
	}{
		{"empty", 3, nil, nil},
		{"under size", 3, []string{"a", "b"}, []string{"a", "b"}},
		{"over size", 2, []string{"a", "b", "c", "d"}, []string{"c", "d"}},
		{"no context", 0, []string{"a"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := window{size: tt.size}
			for _, l := range tt.push {
				w.push(l)
			}
			if got := w.snapshot(); !slices.Equal(got, tt.want) {
				t.Fatalf("snapshot = %q, want %q", got, tt.want)
			}
		})
	}
}
