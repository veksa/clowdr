// Package config parses deploy.yml (project side) and platform.yml (server side).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/veksa/clowdr/internal/spec"
)

// DeployFile names looked up next to the project, in order.
var DeployFile = []string{"deploy.yml", "deploy.yaml", "clowdr.yml"}

type Deploy struct {
	Project  string             `yaml:"project"`
	Envs     map[string]EnvConf `yaml:"envs"`
	Addons   []string           `yaml:"addons"`
	Services map[string]Service `yaml:"services"`
	Logs     *LogsConf          `yaml:"logs"`

	// Dir is the directory deploy.yml was loaded from; service paths are relative to it.
	Dir string `yaml:"-"`
	// order keeps services in file order so `clowctl deploy` without arguments is predictable.
	order []string
}

type EnvConf struct {
	Server  string `yaml:"server"`
	Context string `yaml:"context"`
}

type Service struct {
	Path       string              `yaml:"path"`
	Build      string              `yaml:"build"`
	Dockerfile string              `yaml:"dockerfile"`
	BuildArgs  map[string]string   `yaml:"build_args"`
	Image      string              `yaml:"image"` // prebuilt image instead of docker build
	Platform   string              `yaml:"platform"`
	Port       int                 `yaml:"port"`
	Health     HealthConf          `yaml:"health"`
	Memory     Bytes               `yaml:"memory"`
	Replicas   int                 `yaml:"replicas"`
	Drain      Duration            `yaml:"drain"`
	StopGrace  Duration            `yaml:"stop_grace"`
	Switch     string              `yaml:"switch"`
	Domains    map[string][]string `yaml:"domains"`
	Routes     []Route             `yaml:"routes"`
	TLS        string              `yaml:"tls"`
	MaxBody    *Bytes              `yaml:"max_body"`
	Timeout    Duration            `yaml:"timeout"`
	NginxExtra string              `yaml:"nginx_extra"`
	Volumes    []string            `yaml:"volumes"`
	Env        []string            `yaml:"env"`
	Uses       []string            `yaml:"uses"`
	ExposeTo   []string            `yaml:"expose_to"`
}

// HealthConf accepts either `health: /health` or a mapping.
type HealthConf struct {
	Path      string   `yaml:"path"`
	Interval  Duration `yaml:"interval"`
	Timeout   Duration `yaml:"timeout"`
	Successes int      `yaml:"successes"`
}

func (h *HealthConf) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		h.Path = n.Value
		return nil
	}
	type plain HealthConf
	return n.Decode((*plain)(h))
}

type Route struct {
	Path       string   `yaml:"path"`
	Service    string   `yaml:"service"`
	Timeout    Duration `yaml:"timeout"`
	Buffering  *bool    `yaml:"buffering"`
	MaxBody    *Bytes   `yaml:"max_body"`
	NginxExtra string   `yaml:"nginx_extra"`
}

type LogsConf struct {
	App    *Retention `yaml:"app"`
	Trace  *Retention `yaml:"trace"`
	Metric *Retention `yaml:"metric"`
	Access *Retention `yaml:"access"`
}

type Retention struct {
	Retention Duration `yaml:"retention"`
	Max       Bytes    `yaml:"max"`
}

// FindDeploy looks for deploy.yml in dir and its parents.
func FindDeploy(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		for _, name := range DeployFile {
			p := filepath.Join(dir, name)
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("deploy.yml not found in this directory or its parents")
		}
		dir = parent
	}
}

func LoadDeploy(path string) (*Deploy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	d, err := ParseDeploy(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	d.Dir = filepath.Dir(path)
	return d, nil
}

func ParseDeploy(b []byte) (*Deploy, error) {
	var d Deploy
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&d); err != nil {
		return nil, err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(b, &root); err == nil {
		d.order = serviceOrder(&root)
	}
	if err := d.validate(); err != nil {
		return nil, err
	}
	return &d, nil
}

func serviceOrder(root *yaml.Node) []string {
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	m := root.Content[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == "services" && m.Content[i+1].Kind == yaml.MappingNode {
			var out []string
			for j := 0; j+1 < len(m.Content[i+1].Content); j += 2 {
				out = append(out, m.Content[i+1].Content[j].Value)
			}
			return out
		}
	}
	return nil
}

func (d *Deploy) validate() error {
	if !spec.ValidName(d.Project) {
		return fmt.Errorf("project: invalid or missing name %q", d.Project)
	}
	if len(d.Services) == 0 {
		return errors.New("no services")
	}
	for name, e := range d.Envs {
		if !spec.ValidName(name) {
			return fmt.Errorf("envs: invalid env name %q", name)
		}
		if e.Server == "" && e.Context == "" {
			return fmt.Errorf("envs.%s: server or context is required", name)
		}
	}
	for _, a := range d.Addons {
		if !spec.ValidName(a) {
			return fmt.Errorf("addons: invalid name %q", a)
		}
	}
	for name, s := range d.Services {
		for env := range s.Domains {
			if _, ok := d.Envs[env]; !ok && len(d.Envs) > 0 {
				return fmt.Errorf("service %s: domains for unknown env %q", name, env)
			}
		}
		for _, u := range s.Uses {
			if !contains(d.Addons, u) {
				return fmt.Errorf("service %s: uses %q which is not in project addons", name, u)
			}
		}
	}
	// Validate every env's resolved specs so mistakes show up before building anything.
	envs := d.EnvNames()
	if len(envs) == 0 {
		envs = []string{""}
	}
	for _, env := range envs {
		specs, err := d.Specs(env)
		if err != nil {
			return err
		}
		if err := spec.ValidateProject(specs); err != nil {
			if env != "" {
				return fmt.Errorf("env %s: %w", env, err)
			}
			return err
		}
	}
	return nil
}

func (d *Deploy) EnvNames() []string {
	out := make([]string, 0, len(d.Envs))
	for k := range d.Envs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ServiceNames returns services in file order.
func (d *Deploy) ServiceNames() []string {
	if len(d.order) == len(d.Services) {
		return append([]string(nil), d.order...)
	}
	out := make([]string, 0, len(d.Services))
	for k := range d.Services {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Specs resolves all services for env.
func (d *Deploy) Specs(env string) ([]spec.Service, error) {
	var out []spec.Service
	for _, name := range d.ServiceNames() {
		s, err := d.Spec(name, env)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// Spec resolves one service for env.
func (d *Deploy) Spec(name, env string) (spec.Service, error) {
	s, ok := d.Services[name]
	if !ok {
		return spec.Service{}, fmt.Errorf("service %q is not in deploy.yml", name)
	}
	out := spec.Service{
		Name:       name,
		Port:       s.Port,
		Memory:     int64(s.Memory),
		Replicas:   s.Replicas,
		Drain:      s.Drain.D(),
		StopGrace:  s.StopGrace.D(),
		Switch:     s.Switch,
		Domains:    s.Domains[env],
		TLS:        s.TLS,
		Timeout:    s.Timeout.D(),
		NginxExtra: s.NginxExtra,
		Env:        s.Env,
		Uses:       s.Uses,
		ExposeTo:   s.ExposeTo,
		Health: spec.Health{
			Path:      s.Health.Path,
			Interval:  s.Health.Interval.D(),
			Timeout:   s.Health.Timeout.D(),
			Successes: s.Health.Successes,
		},
	}
	if s.MaxBody != nil {
		v := int64(*s.MaxBody)
		out.MaxBody = &v
	}
	for _, r := range s.Routes {
		sr := spec.Route{
			Path:       r.Path,
			Service:    r.Service,
			Timeout:    r.Timeout.D(),
			Buffering:  r.Buffering,
			NginxExtra: r.NginxExtra,
		}
		if r.MaxBody != nil {
			v := int64(*r.MaxBody)
			sr.MaxBody = &v
		}
		out.Routes = append(out.Routes, sr)
	}
	for _, v := range s.Volumes {
		vol, err := parseVolume(v)
		if err != nil {
			return spec.Service{}, fmt.Errorf("service %s: %w", name, err)
		}
		out.Volumes = append(out.Volumes, vol)
	}
	out.Defaults()
	return out, nil
}

// parseVolume parses "name:/target[:ro]". Host paths are not allowed: a project token must not
// be able to mount the host filesystem.
func parseVolume(s string) (spec.Volume, error) {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return spec.Volume{}, fmt.Errorf("volume %q: expected name:/path[:ro]", s)
	}
	v := spec.Volume{Name: parts[0], Target: parts[1]}
	if strings.ContainsAny(v.Name, "/\\.") {
		return spec.Volume{}, fmt.Errorf("volume %q: host paths are not allowed, use a named volume", s)
	}
	if len(parts) == 3 {
		if parts[2] != "ro" {
			return spec.Volume{}, fmt.Errorf("volume %q: unknown option %q", s, parts[2])
		}
		v.RO = true
	}
	return v, nil
}

// Expand replaces ${ENV} in s. Other variables are left as they are, so shell
// syntax in build commands keeps working.
func Expand(s, env string) string {
	return strings.NewReplacer("${ENV}", env).Replace(s)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
