package config_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/swmcc/herdr-sightline/internal/config"
)

// tomlBlock is a fenced toml block in a Markdown file.
var tomlBlock = regexp.MustCompile("(?s)```toml\n(.*?)```")

// Every .sightline.toml example in the docs has to be one Sightline would
// accept, or people would copy a file that's refused.
func TestProjectFileExamplesInTheDocs(t *testing.T) {
	found := 0
	for _, doc := range []string{"README.md", "CONTRIBUTING.md"} {
		data, err := os.ReadFile(filepath.Join("..", "..", doc))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range tomlBlock.FindAllStringSubmatch(string(data), -1) {
			first, _, _ := strings.Cut(m[1], "\n")
			if !strings.Contains(first, config.ProjectFileName) {
				continue
			}
			found++
			if _, err := config.ParseProject(doc, []byte(m[1])); err != nil {
				t.Errorf("%s has a %s example that's refused: %v", doc, config.ProjectFileName, err)
			}
		}
	}
	if found == 0 {
		t.Error("found no project file examples, so the test checks nothing")
	}
}
