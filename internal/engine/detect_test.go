package engine

import "testing"

func TestSubstring(t *testing.T) {
	tests := []struct {
		name string
		m    Substring
		line string
		want bool
	}{
		{"contains", Substring{Text: "Error"}, "TypeError: boom", true},
		{"missing", Substring{Text: "Error"}, "compiled successfully", false},
		{"case matters", Substring{Text: "Error"}, "error: boom", false},
		{"empty text never matches", Substring{}, "anything", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.m.Match(tt.line); got != tt.want {
				t.Fatalf("Match(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

func TestStartsError(t *testing.T) {
	e := New(Options{
		Matchers: []Matcher{Substring{Label: "generic", Text: "Error"}},
		Ignores:  []Matcher{Substring{Label: "noise", Text: "DeprecationWarning"}},
	})
	tests := []struct {
		line string
		want bool
	}{
		{"Error: boom", true},
		{"DeprecationWarning: Error soon", false},
		{"   ", false},
		{"all good", false},
	}
	for _, tt := range tests {
		if got := e.startsError(tt.line); got != tt.want {
			t.Errorf("startsError(%q) = %v, want %v", tt.line, got, tt.want)
		}
	}
}
