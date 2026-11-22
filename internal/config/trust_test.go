package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swmcc/herdr-sightline/internal/config"
)

func TestTrustIsForOneVersionOfOneFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	root := "/home/me/code/web"
	v1 := config.SumOf([]byte("[[ignore]]\nregex = 'x'\n"))
	v2 := config.SumOf([]byte("[[ignore]]\nregex = 'x' \n"))

	trust, err := config.LoadTrust(dir)
	if err != nil || trust.Trusted(root, v1) {
		t.Fatalf("a missing trust file trusts something (err %v)", err)
	}
	if err := config.TrustProject(dir, root, v1); err != nil {
		t.Fatal(err)
	}
	trust, err = config.LoadTrust(dir)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		root string
		sum  config.Sum
		want bool
	}{
		{"the version trusted", root, v1, true},
		{"the file with one byte changed", root, v2, false},
		{"the same file in another repo", "/home/me/code/other", v1, false},
		{"a path inside the repo", root + "/sub", v1, false},
	}
	for _, tt := range tests {
		if got := trust.Trusted(tt.root, tt.sum); got != tt.want {
			t.Errorf("%s: trusted = %v, want %v", tt.name, got, tt.want)
		}
	}

	// Trusting the new version replaces the old one.
	if err := config.TrustProject(dir, root, v2); err != nil {
		t.Fatal(err)
	}
	trust, _ = config.LoadTrust(dir)
	if trust.Trusted(root, v1) || !trust.Trusted(root, v2) {
		t.Errorf("trusting a new version didn't replace the old one")
	}

	if err := config.ForgetProject(dir, root); err != nil {
		t.Fatal(err)
	}
	trust, _ = config.LoadTrust(dir)
	if trust.Trusted(root, v2) || len(trust.Roots()) != 0 {
		t.Errorf("forgetting left %v trusted", trust.Roots())
	}

	info, err := os.Stat(config.TrustPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("trust file mode = %v, want 0600", perm)
	}
}

// A trust file that can't be understood trusts nothing, and isn't overwritten,
// so the person sees the problem rather than losing it.
func TestBrokenTrustFileTrustsNothing(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{"not json", "{", "reading trusted projects"},
		{"another version", `{"version": 2, "projects": []}`, "version 2"},
		{"a relative root", `{"version": 1, "projects": [{"root": "web", "sha256": "` + strings.Repeat("ab", 32) + `"}]}`, `"web" isn't a repo path`},
		{"a short sum", `{"version": 1, "projects": [{"root": "/web", "sha256": "abcd"}]}`, `"/web" isn't a repo path`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(config.TrustPath(dir), []byte(tt.data), 0o600); err != nil {
				t.Fatal(err)
			}
			trust, err := config.LoadTrust(dir)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %v, want one saying %q", err, tt.want)
			}
			if len(trust.Roots()) != 0 {
				t.Errorf("a broken file trusted %v", trust.Roots())
			}
			if err := config.TrustProject(dir, "/web", config.SumOf(nil)); err == nil {
				t.Errorf("trusting over a broken file worked")
			}
			if got, _ := os.ReadFile(config.TrustPath(dir)); string(got) != tt.data {
				t.Errorf("the broken file was overwritten with %q", got)
			}
		})
	}
}

func TestTrustNeedsAStateDir(t *testing.T) {
	if err := config.TrustProject("", "/web", config.SumOf(nil)); err == nil {
		t.Error("trusted with nowhere to record it")
	}
}
