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
		Matchers: []Matcher{Substring{Label: "generic", Text: "Error"}, Substring{Label: "panic", Text: "panic:"}},
		Ignores:  []Matcher{Substring{Label: "noise", Text: "DeprecationWarning"}},
	})
	tests := []struct {
		line     string
		wantName string
		want     bool
	}{
		{"Error: boom", "generic", true},
		{"panic: oh no", "panic", true},
		{"DeprecationWarning: Error soon", "", false},
		{"   ", "", false},
		{"all good", "", false},
	}
	for _, tt := range tests {
		if name, ok := e.startsError(tt.line); ok != tt.want || name != tt.wantName {
			t.Errorf("startsError(%q) = %q, %v, want %q, %v", tt.line, name, ok, tt.wantName, tt.want)
		}
	}
}
