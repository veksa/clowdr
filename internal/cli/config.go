package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// UserConfig is ~/.clowdr/config: contexts (server + token), like kubeconfig.
type UserConfig struct {
	Current  string              `yaml:"current,omitempty"`
	Contexts map[string]*Context `yaml:"contexts,omitempty"`
	NPM      *NPMConfig          `yaml:"npm,omitempty"`

	path string
}

type Context struct {
	Server      string `yaml:"server"`
	Token       string `yaml:"token,omitempty"`
	Fingerprint string `yaml:"fingerprint,omitempty"`
}

func userConfigPath() (string, error) {
	if p := os.Getenv("CLOWDR_CONFIG"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".clowdr", "config"), nil
}

func loadUserConfig() (*UserConfig, error) {
	p, err := userConfigPath()
	if err != nil {
		return nil, err
	}
	c := &UserConfig{path: p, Contexts: map[string]*Context{}}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	} else if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	if c.Contexts == nil {
		c.Contexts = map[string]*Context{}
	}
	return c, nil
}

func (c *UserConfig) Save() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	// The file holds admin and npm tokens: always 0600, also when an older clowctl left it 0644.
	return writeFileAtomic(c.path, b, 0o600)
}

// byServer finds a context for a server URL (to reuse its token and fingerprint).
func (c *UserConfig) byServer(server string) *Context {
	server = strings.TrimRight(server, "/")
	for _, ctx := range c.Contexts {
		if strings.TrimRight(ctx.Server, "/") == server {
			return ctx
		}
	}
	return nil
}
