package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The README is the spec, so its example message has to be exactly what the engine
// sends. TestMessageMatchesTheReadme holds the engine to the fixture. This holds the
// README to the same fixture, so neither can drift on its own.
func TestReadmeExampleMatchesTheFixture(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := readmeExample(string(readme))
	if err != nil {
		t.Fatal(err)
	}
	if want := readFixture(t, "message_readme.txt"); got != want {
		t.Fatalf("the README's \"What the agent receives\" example doesn't match testdata/message_readme.txt:\n%s", lineDiff(want, got))
	}
}

func TestReadmeExample(t *testing.T) {
	tests := []struct {
		name    string
		readme  string
		want    string
		wantErr string
	}{
		{
			name:   "outer fence keeps the inner one",
			readme: "## What the agent receives\n\nIntro\n\n````\nhello\n```text\n| x\n```\nbye\n````\n\n## Next\n",
			want:   "hello\n```text\n| x\n```\nbye\n",
		},
		{
			name:   "fence with an info string",
			readme: "## What the agent receives\n\n```text\nhello\n```\n",
			want:   "hello\n",
		},
		{
			name:    "no section",
			readme:  "## Something else\n\n```\nhello\n```\n",
			wantErr: "no \"## What the agent receives\" section",
		},
		{
			name:    "no fence in the section",
			readme:  "## What the agent receives\n\nJust words.\n\n## Next\n\n```\nhello\n```\n",
			wantErr: "no fenced example",
		},
		{
			name:    "fence never closes",
			readme:  "## What the agent receives\n\n````\nhello\n```\n",
			wantErr: "never closes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readmeExample(tt.readme)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("example = %q, want %q", got, tt.want)
			}
		})
	}
}

// readmeExample returns the first fenced block under the README's "What the agent
// receives" heading. The example holds a fence of its own, so the block ends at the
// first fence of the same character that's at least as long as the opening one.
func readmeExample(readme string) (string, error) {
	const heading = "## What the agent receives"
	lines := strings.SplitAfter(readme, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimRight(l, "\r\n") == heading {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return "", fmt.Errorf("README has no %q section", heading)
	}
	for i := start; i < len(lines); i++ {
		l := strings.TrimRight(lines[i], "\r\n")
		if strings.HasPrefix(l, "## ") {
			break
		}
		fence := leadingRun(l, '`')
		if len(fence) < 3 {
			continue
		}
		var body strings.Builder
		for _, inner := range lines[i+1:] {
			trimmed := strings.TrimRight(inner, "\r\n")
			if len(leadingRun(trimmed, '`')) >= len(fence) && strings.TrimLeft(trimmed, "`") == "" {
				return body.String(), nil
			}
			body.WriteString(inner)
		}
		return "", fmt.Errorf("the example under %q never closes its %s fence", heading, fence)
	}
	return "", fmt.Errorf("README has no fenced example under %q", heading)
}

func leadingRun(s string, c byte) string {
	n := 0
	for n < len(s) && s[n] == c {
		n++
	}
	return s[:n]
}

// lineDiff shows each line that differs, so a drift is easy to spot in CI output.
func lineDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	var b strings.Builder
	for i := 0; i < max(len(w), len(g)); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			fmt.Fprintf(&b, "line %d:\n  fixture: %q\n  README:  %q\n", i+1, wl, gl)
		}
	}
	return b.String()
}
