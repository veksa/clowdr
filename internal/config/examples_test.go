package config

import (
	"path/filepath"
	"testing"
)

// The projects in examples/ are documentation and the E2E fixture: they must stay valid.
func TestExamples(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "examples", "*", "deploy.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no examples: %v", err)
	}
	for _, p := range files {
		d, err := LoadDeploy(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, env := range d.EnvNames() {
			if _, err := d.Specs(env); err != nil {
				t.Fatalf("%s, env %s: %v", p, env, err)
			}
		}
	}
}
