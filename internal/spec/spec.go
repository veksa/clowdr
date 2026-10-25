// Package spec holds the env-resolved service description that the CLI sends to clowdrd
// with every deploy and that clowdrd keeps in its state.
package spec

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Service is one service of a project, resolved for one env.
type Service struct {
	Name       string        `json:"name"`
	Port       int           `json:"port,omitempty"`
	Health     Health        `json:"health"`
	Memory     int64         `json:"memory,omitempty"` // bytes, 0 = unlimited
	Replicas   int           `json:"replicas"`
	Drain      time.Duration `json:"drain"`
	StopGrace  time.Duration `json:"stop_grace"`
	Switch     string        `json:"switch,omitempty"` // "" (start-first) | "stop-first"
	Domains    []string      `json:"domains,omitempty"`
	Routes     []Route       `json:"routes,omitempty"`
	TLS        string        `json:"tls,omitempty"` // "" (behind Cloudflare) | "direct"
	MaxBody    *int64        `json:"max_body,omitempty"`
	Timeout    time.Duration `json:"timeout,omitempty"`
	NginxExtra string        `json:"nginx_extra,omitempty"`
	Volumes    []Volume      `json:"volumes,omitempty"`
	Env        []string      `json:"env,omitempty"` // names whose values come from `clowctl env set`
	Uses       []string      `json:"uses,omitempty"`
	ExposeTo   []string      `json:"expose_to,omitempty"`
	// AcceptFrom lists projects whose services may join this project's network (their
	// expose_to). It is project-wide in deploy.yml and copied into every service.
	AcceptFrom []string          `json:"accept_from,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
}

type Health struct {
	Path      string        `json:"path,omitempty"` // "" = container running is enough
	Interval  time.Duration `json:"interval"`
	Timeout   time.Duration `json:"timeout"`   // total time to become healthy
	Successes int           `json:"successes"` // consecutive successful probes required
}

type Route struct {
	Path       string        `json:"path"`
	Service    string        `json:"service,omitempty"` // another service of the same project
	Timeout    time.Duration `json:"timeout,omitempty"`
	Buffering  *bool         `json:"buffering,omitempty"`
	MaxBody    *int64        `json:"max_body,omitempty"`
	NginxExtra string        `json:"nginx_extra,omitempty"`
}

type Volume struct {
	Name   string `json:"name"`
	Target string `json:"target"`
	RO     bool   `json:"ro,omitempty"`
}

const (
	SwitchStartFirst = ""
	SwitchStopFirst  = "stop-first"
	TLSDirect        = "direct"
)

var (
	nameRe   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	domainRe = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	envRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// pathRe: URL path characters that mean the same to nginx and in an HTTP request line.
	pathRe = regexp.MustCompile(`^/[A-Za-z0-9._~!&()*+,=:@%/-]*$`)
)

// ValidName reports whether s can be used as a project, service, env or volume name.
// Names end up in container names, nginx upstream names and URL paths.
func ValidName(s string) bool { return nameRe.MatchString(s) }

func ValidDomain(s string) bool { return domainRe.MatchString(s) }

func ValidEnvName(s string) bool { return envRe.MatchString(s) }

// Defaults fills zero values.
func (s *Service) Defaults() {
	if s.Replicas == 0 {
		s.Replicas = 1
	}
	if s.Drain == 0 {
		s.Drain = 30 * time.Second
	}
	if s.StopGrace == 0 {
		s.StopGrace = 10 * time.Second
	}
	if s.Health.Interval == 0 {
		s.Health.Interval = time.Second
	}
	if s.Health.Timeout == 0 {
		s.Health.Timeout = 60 * time.Second
	}
	if s.Health.Successes == 0 {
		s.Health.Successes = 2
	}
}

// Validate checks the spec on its own. Cross-service checks (route targets) live in
// ValidateProject, domain ownership is checked by clowdrd against platform.yml.
func (s *Service) Validate() error {
	if !ValidName(s.Name) {
		return fmt.Errorf("service %q: invalid name (lowercase letters, digits, '-')", s.Name)
	}
	errf := func(format string, a ...any) error {
		return fmt.Errorf("service %s: "+format, append([]any{s.Name}, a...)...)
	}
	if s.Port < 0 || s.Port > 65535 {
		return errf("invalid port %d", s.Port)
	}
	if (len(s.Domains) > 0 || len(s.Routes) > 0) && s.Port == 0 {
		return errf("port is required when domains or routes are set")
	}
	if p, q, _ := strings.Cut(s.Health.Path, "?"); s.Health.Path != "" && (!pathRe.MatchString(p) || strings.ContainsAny(q, " \t\r\n#")) {
		return errf("health path must be a URL path starting with /")
	}
	if s.Health.Path != "" && s.Port == 0 {
		return errf("port is required for an http health check")
	}
	if s.Replicas < 0 || s.Replicas > 32 {
		return errf("replicas must be between 0 and 32")
	}
	switch s.Switch {
	case SwitchStartFirst, SwitchStopFirst:
	default:
		return errf("switch must be stop-first or empty")
	}
	switch s.TLS {
	case "", TLSDirect:
	default:
		return errf("tls must be direct or empty")
	}
	seen := map[string]bool{}
	for _, d := range s.Domains {
		if !ValidDomain(d) {
			return errf("invalid domain %q", d)
		}
		if seen[d] {
			return errf("duplicate domain %q", d)
		}
		seen[d] = true
	}
	paths := map[string]bool{}
	for _, r := range s.Routes {
		if !pathRe.MatchString(r.Path) {
			return errf("invalid route path %q", r.Path)
		}
		if paths[r.Path] {
			return errf("duplicate route %q", r.Path)
		}
		paths[r.Path] = true
		if r.Service != "" && !ValidName(r.Service) {
			return errf("route %s: invalid service %q", r.Path, r.Service)
		}
	}
	for _, v := range s.Volumes {
		if !ValidName(v.Name) {
			return errf("invalid volume name %q", v.Name)
		}
		if !strings.HasPrefix(v.Target, "/") {
			return errf("volume %s: target must be an absolute path", v.Name)
		}
	}
	for _, e := range s.Env {
		if !ValidEnvName(e) {
			return errf("invalid env name %q", e)
		}
	}
	for _, u := range slices.Concat(s.Uses, s.ExposeTo, s.AcceptFrom) {
		if !ValidName(u) {
			return errf("invalid name %q", u)
		}
	}
	return nil
}

// ValidateProject checks a set of services that belong to one project.
func ValidateProject(services []Service) error {
	byName := map[string]bool{}
	for _, s := range services {
		byName[s.Name] = true
	}
	domains := map[string]string{}
	for _, s := range services {
		if err := s.Validate(); err != nil {
			return err
		}
		for _, d := range s.Domains {
			if other, ok := domains[d]; ok {
				return fmt.Errorf("domain %s is used by both %s and %s", d, other, s.Name)
			}
			domains[d] = s.Name
		}
		for _, r := range s.Routes {
			if r.Service != "" && !byName[r.Service] {
				return fmt.Errorf("service %s: route %s points to unknown service %s", s.Name, r.Path, r.Service)
			}
		}
	}
	return nil
}
