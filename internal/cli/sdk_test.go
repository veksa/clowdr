package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSetDependency(t *testing.T) {
	cases := map[string]string{
		"new": `{
  "name": "shop",
  "dependencies": {
    "express": "^4.0.0"
  }
}
`,
		"existing": `{"dependencies": {"clowdr-node": "file:old.tgz", "express": "^4"}}`,
		"empty":    "{\n    \"dependencies\": {}\n}\n",
	}
	for name, src := range cases {
		out, err := setDependency([]byte(src), "clowdr-node", "file:../vendor/clowdr/clowdr-node-0.2.0.tgz")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var pj struct{ Dependencies map[string]string }
		if err := json.Unmarshal(out, &pj); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
		if pj.Dependencies["clowdr-node"] != "file:../vendor/clowdr/clowdr-node-0.2.0.tgz" {
			t.Fatalf("%s: %s", name, out)
		}
		if name != "empty" && pj.Dependencies["express"] == "" {
			t.Fatalf("%s lost a dependency: %s", name, out)
		}
	}
	// Formatting is kept: two-space indent stays two spaces.
	out, _ := setDependency([]byte(cases["new"]), "clowdr-node", "file:x.tgz")
	if !strings.Contains(string(out), "\n    \"clowdr-node\": \"file:x.tgz\",\n    \"express\"") {
		t.Fatalf("indent: %s", out)
	}
	if _, err := setDependency([]byte(`{"name":"x"}`), "clowdr-node", "file:x.tgz"); err == nil {
		t.Fatal("no dependencies block accepted")
	}
}
