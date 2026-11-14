package cli

import (
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
	out, mode, changed, err := yamlEdit(path, value, keys...)
	if err != nil || !changed {
		if err == nil && private && mode != 0 && mode != 0o600 {
			return chmodTarget(path, 0o600)
		}
		return err
	}
	switch {
	case private:
		mode = 0o600
	case mode == 0:
		mode = 0o644
	}
	return writeFileAtomic(path, out, mode)
}

// yamlEdit computes the new content of path with the string at keys set, without writing it.
// changed is false when the value is already there, so a file is not rewritten (and reformatted)
// for nothing. Line endings (CRLF) and a header comment's spacing are kept.
func yamlEdit(path string, value string, keys ...string) (out []byte, mode os.FileMode, changed bool, err error) {
	doc, mode, err := readYAMLDoc(path)
	if err != nil {
		return nil, 0, false, err
	}
	orig, _ := os.ReadFile(path)
	n := doc.Content[0]
	for _, k := range keys[:len(keys)-1] {
		if n, err = yamlChild(n, k, true); err != nil {
			return nil, 0, false, fmt.Errorf("%s: %w", path, err)
		}
	}
	last := keys[len(keys)-1]
	v, err := yamlChild(n, last, false)
	if err != nil {
		return nil, 0, false, fmt.Errorf("%s: %w", path, err)
	}
	if v != nil && v.Kind == yaml.ScalarNode && v.Value == value && len(orig) > 0 {
		return orig, mode, false, nil
	}
	if v == nil {
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: last}, &yaml.Node{Kind: yaml.ScalarNode})
		v = n.Content[len(n.Content)-1]
	}
	v.Kind, v.Tag, v.Value, v.Style, v.Content = yaml.ScalarNode, "!!str", value, 0, nil
	if strings.ContainsAny(value, ":#") || value == "" {
		v.Style = yaml.DoubleQuotedStyle
	}
	if out, err = encodeYAML(orig, doc); err != nil {
		return nil, 0, false, err
	}
	return out, mode, true, nil
}

// keepHeaderSpacing drops the blank line the encoder puts after a file's leading comment block
// when the original had none there.
func keepHeaderSpacing(orig, out []byte) []byte {
	lines := strings.SplitAfter(string(orig), "\n")
	i := 0
	for i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "#") {
		i++
	}
	if i == 0 || i >= len(lines) || strings.TrimSpace(lines[i]) == "" {
		return out
	}
	header := strings.Join(lines[:i], "")
	if rest, ok := strings.CutPrefix(string(out), header+"\n"); ok {
		return []byte(header + rest)
	}
	return out
}

// yamlAddToList adds value to the list at keys, creating it (and mappings on the way) when
// missing; a value already in the list is not added again.
func yamlAddToList(path, value string, private bool, keys ...string) error {
	doc, mode, err := readYAMLDoc(path)
	if err != nil {
		return err
	}
	orig, _ := os.ReadFile(path)
	n := doc.Content[0]
	for _, k := range keys[:len(keys)-1] {
		if n, err = yamlChild(n, k, true); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	last := keys[len(keys)-1]
	list, err := yamlChild(n, last, false)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if list == nil {
		list = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: last}, list)
	}
	if list.Kind != yaml.SequenceNode {
		return fmt.Errorf("%s: %s is not a list", path, last)
	}
	for _, item := range list.Content {
		if item.Kind == yaml.ScalarNode && item.Value == value {
			return nil
		}
	}
	list.Content = append(list.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value, Style: yaml.DoubleQuotedStyle})
	out, err := encodeYAML(orig, doc)
	if err != nil {
		return err
	}
	switch {
	case private:
		mode = 0o600
	case mode == 0:
		mode = 0o644
	}
	return writeFileAtomic(path, out, mode)
}

// encodeYAML writes doc back in the shape of the original file: indent 2, its line endings
// and header comment spacing.
func encodeYAML(orig []byte, doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	enc.Close()
	out := keepHeaderSpacing(bytes.ReplaceAll(orig, []byte("\r\n"), []byte("\n")), buf.Bytes())
	if bytes.Contains(orig, []byte("\r\n")) {
		out = bytes.ReplaceAll(out, []byte("\n"), []byte("\r\n"))
	}
	return out, nil
}

// npmrcSet sets "key=value" in an .npmrc, replacing a line with the same key, keeping the
// others (and their line endings), and writes it with mode 0600.
func npmrcSet(path, key, value string) error {
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	eol := "\n"
	if bytes.Contains(b, []byte("\r\n")) {
		eol = "\r\n"
	}
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	var out []string
	found := false
	if text != "" {
		for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
			if k, _, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == key {
				if !found {
					out = append(out, key+"="+value)
					found = true
				}
				continue
			}
			out = append(out, line)
		}
	}
	if !found {
		out = append(out, key+"="+value)
	}
	return writeFileAtomic(path, []byte(strings.Join(out, eol)+eol), 0o600)
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

// resolveTarget follows symlinks at path, so a dotfile linked into a dotfiles repository is
// updated where it lives instead of being replaced by a regular file. A missing path is itself.
func resolveTarget(path string) (string, error) {
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return path, nil
	}
	t, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return t, nil
}

func chmodTarget(path string, mode os.FileMode) error {
	t, err := resolveTarget(path)
	if err != nil {
		return err
	}
	return os.Chmod(t, mode)
}

// writeFileAtomic replaces path (the file a symlink at path points at) with data: a temp file in
// the same directory, then rename, so a crash never leaves half a file. The mode is applied to
// the new file even if path existed.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	path, err := resolveTarget(path)
	if err != nil {
		return err
	}
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

// checkWritable fails when a file could not be written to dir (path's directory, created if
// missing), by creating and removing a temp file there.
func checkWritable(path string) error {
	path, err := resolveTarget(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".check-*")
	if err != nil {
		return fmt.Errorf("%s is not writable: %w", dir, err)
	}
	f.Close()
	return os.Remove(f.Name())
}
