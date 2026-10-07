package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildShell(t *testing.T) {
	noEnv := func(string) string { return "" }
	found := func(string) (string, error) { return `C:\Program Files\Git\usr\bin\sh.exe`, nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }

	if name, args := buildShell("linux", noEnv, missing); name != "sh" || args[0] != "-c" {
		t.Errorf("linux: %s %v", name, args)
	}
	// Git Bash: sh is on PATH.
	if name, _ := buildShell("windows", noEnv, found); name != `C:\Program Files\Git\usr\bin\sh.exe` {
		t.Errorf("git bash: %s", name)
	}
	// cmd or PowerShell with Git for Windows installed.
	pf := t.TempDir()
	os.MkdirAll(filepath.Join(pf, "Git", "bin"), 0o755)
	os.WriteFile(filepath.Join(pf, "Git", "bin", "sh.exe"), nil, 0o755)
	env := func(k string) string {
		if k == "ProgramFiles" {
			return pf
		}
		return ""
	}
	if name, _ := buildShell("windows", env, missing); name != filepath.Join(pf, "Git", "bin", "sh.exe") {
		t.Errorf("git for windows: %s", name)
	}
	// Nothing: cmd.
	if name, args := buildShell("windows", noEnv, missing); name != "cmd" || args[0] != "/C" {
		t.Errorf("no sh: %s %v", name, args)
	}
}
