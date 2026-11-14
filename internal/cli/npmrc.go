package cli

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Editing of the files yarn and npm read their registry settings from: .yarnrc.yml (YAML, edited
// as a node tree so comments and unrelated keys survive) and .npmrc (ini lines).

// readYAMLDoc parses a YAML file into a document node; a missing or empty file gives an empty
// mapping.
func readYAMLDoc(path string) (*yaml.Node, os.FileMode, error) {
	b, err := os.ReadFile(path)
	mode := os.FileMode(0)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(bytes.TrimSpace(b)) == 0) {
		return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}, mode, nil
	}
	if err != nil {
		return nil, 0, err
	}
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, 0, fmt.Errorf("%s: %w", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, 0, fmt.Errorf("%s: expected a mapping at the top", path)
	}
	return &doc, mode, nil
}

// yamlChild returns the value node of key in mapping m, creating an empty mapping when create
// is set.
func yamlChild(m *yaml.Node, key string, create bool) (*yaml.Node, error) {
	if m.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%q: not a mapping", key)
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1], nil
		}
	}
	if !create {
		return nil, nil
	}
	v := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
	return v, nil
}

// yamlGet reads the string at keys, "" when absent.
func yamlGet(path string, keys ...string) (string, error) {
	doc, _, err := readYAMLDoc(path)
	if err != nil {
		return "", err
	}
	n := doc.Content[0]
	for _, k := range keys {
		if n, err = yamlChild(n, k, false); err != nil || n == nil {
			return "", err
		}
	}
	if n.Kind != yaml.ScalarNode {
		return "", nil
	}
	return n.Value, nil
}

// yamlMap reads the mapping at keys as key → value node, empty when absent.
func yamlMap(path string, keys ...string) (map[string]*yaml.Node, error) {
	doc, _, err := readYAMLDoc(path)
	if err != nil {
		return nil, err
	}
	n := doc.Content[0]
	for _, k := range keys {
		if n, err = yamlChild(n, k, false); err != nil || n == nil {
			return map[string]*yaml.Node{}, err
		}
	}
	out := map[string]*yaml.Node{}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			out[n.Content[i].Value] = n.Content[i+1]
		}
	}
	return out, nil
}

// yamlSet sets the string at keys, creating mappings on the way, and writes the file with mode
// (an existing file keeps its mode unless private is set, which forces 0600).
func yamlSet(path string, value string, private bool, keys ...string) error {
	doc, mode, err := readYAMLDoc(path)
	if err != nil {
		return err
	}
	n := doc.Content[0]
	for _, k := range keys[:len(keys)-1] {
		if n, err = yamlChild(n, k, true); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	last := keys[len(keys)-1]
	v, err := yamlChild(n, last, false)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if v == nil {
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: last}, &yaml.Node{Kind: yaml.ScalarNode})
		v = n.Content[len(n.Content)-1]
	}
	v.Kind, v.Tag, v.Value, v.Style, v.Content = yaml.ScalarNode, "!!str", value, 0, nil
	if strings.ContainsAny(value, ":#") || value == "" {
		v.Style = yaml.DoubleQuotedStyle
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return err
	}
	enc.Close()
	switch {
	case private:
		mode = 0o600
	case mode == 0:
		mode = 0o644
	}
	return writeFileAtomic(path, buf.Bytes(), mode)
}

// npmrcSet sets "key=value" in an .npmrc, replacing a line with the same key, keeping the
// others, and writes it with mode 0600.
func npmrcSet(path, key, value string) error {
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var out []string
	found := false
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if k, _, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == key {
			if !found {
				out = append(out, key+"="+value)
				found = true
			}
			continue
		}
		out = append(out, line)
	}
	if !found {
		out = append(out, key+"="+value)
	}
	return writeFileAtomic(path, []byte(strings.Join(out, "\n")+"\n"), 0o600)
}

// npmrcGet reads the value of key in an .npmrc, "" when absent.
func npmrcGet(path, key string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// writeFileAtomic replaces path with data: a temp file in the same directory, then rename, so a
// crash never leaves half a file. The mode is applied to the new file even if path existed.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
