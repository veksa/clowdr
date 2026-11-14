package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/veksa/clowdr/internal/spec"
)

const DefaultPlatformPath = "/etc/clowdr/platform.yml"

// Platform is /etc/clowdr/platform.yml, owned by the host admin.
type Platform struct {
	// Domain serves the API and registry on deploy.<Domain>. Empty = https://<ip>:7443 with a
	// self-signed certificate.
	Domain string `yaml:"domain"`
	// Listen is where clowdrd's API listens. nginx proxies deploy.<Domain> to it.
	Listen string `yaml:"listen"`
	// ACMEEmail is used for Let's Encrypt registration.
	ACMEEmail string `yaml:"acme_email"`
	// ACMEDirectory overrides the ACME server (Let's Encrypt staging, Pebble in tests).
	ACMEDirectory string `yaml:"acme_directory"`

	Nginx          NginxConf               `yaml:"nginx"`
	TrustedProxies []string                `yaml:"trusted_proxies"` // "cloudflare" or CIDRs
	Projects       map[string]ProjectConf  `yaml:"projects"`
	Addons         map[string]AddonConf    `yaml:"addons"`
	External       map[string]ExternalConf `yaml:"external"`
	Logs           LogsConf                `yaml:"logs"`
	Notify         NotifyConf              `yaml:"notify"`
	Limits         LimitsConf              `yaml:"limits"`
	NPM            NPMConf                 `yaml:"npm"`
	Plugins        PluginsConf             `yaml:"plugins"`
}

// PluginsConf configures the Claude Code plugin marketplace at /plugins/ on the deploy host
// (docs/plugins.md). Every key is optional.
type PluginsConf struct {
	// Name is the marketplace's name, typed after "@" when a plugin is installed. Default:
	// MarketplaceName. Changing it later makes every user set the marketplace up again.
	Name string `yaml:"name"`
	// Owner is the marketplace's owner shown by Claude Code. Default: the domain.
	Owner string `yaml:"owner"`
	// MaxZip caps a plugin's zip: a publish whose zip would be larger is refused.
	MaxZip Bytes `yaml:"max_zip"`
}

// DefaultPluginMaxZip is plugins.max_zip unless set; PluginMaxZipCap is the most it may be,
// Claude Code's own download limit.
const (
	DefaultPluginMaxZip = 64 << 20
	PluginMaxZipCap     = 256 << 20
)

var (
	marketplaceNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	nonNameRe         = regexp.MustCompile(`[^a-z0-9]+`)
)

// MarketplaceName is the marketplace's name: c.Name, else "clowdr-" and the domain with every
// run of other characters than a-z and 0-9 made one dash ("example.com" → "clowdr-example-com").
// Without a domain, the host the client used stands in, without its port.
func MarketplaceName(c PluginsConf, domain, host string) string {
	if c.Name != "" {
		return c.Name
	}
	if domain == "" {
		domain = host
		if h, _, err := net.SplitHostPort(host); err == nil {
			domain = h
		}
	}
	s := strings.Trim(nonNameRe.ReplaceAllString(strings.ToLower(domain), "-"), "-")
	if s == "" {
		s = "local"
	}
	return "clowdr-" + s
}

// ValidMarketplaceName refuses a name Claude Code would not take: other characters than
// letters, digits, '.', '_' and '-', "..", more than 128 characters, the "claudeai-" prefix, names
// it keeps for itself, and anything with "claude" or "anthropic" in it, which it may read as an
// imitation of an official marketplace. The error states the rule.
func ValidMarketplaceName(n string) error {
	l := strings.ToLower(n)
	switch {
	case !marketplaceNameRe.MatchString(n) || strings.Contains(n, ".."):
		return errors.New(`a marketplace name uses only letters, digits, '.', '_' and '-', starts with a letter or digit, has no "..", and is at most 128 characters`)
	case strings.Contains(l, "claude") || strings.Contains(l, "anthropic"):
		return errors.New(`Claude Code refuses marketplace names containing "claude" or "anthropic" (they read as its own)`)
	}
	switch l {
	case "inline", "builtin", "skills-dir", "synced", "org", "org-provisioned", "unknown", "healthcare", "life-sciences", "agent-skills",
		"constructor", "hasownproperty", "isprototypeof", "propertyisenumerable", "tolocalestring", "tostring", "valueof":
		return errors.New("Claude Code reserves this marketplace name")
	}
	return nil
}

// NPMConf configures the private npm registry at /npm/ on the deploy host.
type NPMConf struct {
	// Publish allows publishing on this server. Only one server should have it; the others
	// serve what they have read-only.
	Publish bool `yaml:"publish"`
	// Keep is how many versions of a package retention keeps (by semver), besides every version
	// a dist-tag points at.
	Keep int `yaml:"keep"`
	// Grace is how long an old version stays deprecated before the sweep deletes it.
	Grace Duration `yaml:"grace"`
	// MaxPublish caps one publish request body (the tarball travels base64-encoded in it).
	MaxPublish Bytes `yaml:"max_publish"`
}

type NginxConf struct {
	Image                 string   `yaml:"image"`
	WorkerShutdownTimeout Duration `yaml:"worker_shutdown_timeout"`
	ClientMaxBodySize     Bytes    `yaml:"client_max_body_size"`
}

// ProjectConf seeds a project declaratively; `clowctl project create` does the same at runtime.
type ProjectConf struct {
	Domains []string `yaml:"domains"`
	Addons  []string `yaml:"addons"`
	Memory  Bytes    `yaml:"memory"`
}

type AddonConf struct {
	Image  string   `yaml:"image"`
	Memory Bytes    `yaml:"memory"`
	Port   int      `yaml:"port"`
	Volume string   `yaml:"volume"` // mount target for the data volume
	Allow  []string `yaml:"allow"`
}

type ExternalConf struct {
	Share map[string]struct {
		Env map[string]string `yaml:"env"`
	} `yaml:"share"`
}

type NotifyConf struct {
	Telegram struct {
		Token  string `yaml:"token"`
		ChatID string `yaml:"chat_id"`
	} `yaml:"telegram"`
}

type LimitsConf struct {
	// ReserveMemory is kept free for the OS, docker, nginx and clowdrd when checking
	// whether a second instance fits.
	ReserveMemory Bytes `yaml:"reserve_memory"`
	DiskFreeAlert int   `yaml:"disk_free_alert"` // percent
	// Pids caps processes per container (fork bombs); CPUs caps cores per container, 0 = no cap.
	Pids int64   `yaml:"pids"`
	CPUs float64 `yaml:"cpus"`
	// MaxBlob caps one pushed image layer; pushes also stop while the disk is nearly full.
	MaxBlob Bytes `yaml:"max_blob"`
}

// DefaultPlatform is written to /etc/clowdr/platform.yml on first start.
const DefaultPlatform = `# clowdr platform config. Owned by the host admin; projects cannot change it.
# domain: example.com          # API and registry on deploy.example.com (Let's Encrypt)
# acme_email: ops@example.com
listen: 127.0.0.1:7070
trusted_proxies: [cloudflare]

nginx:
  image: nginx:alpine
  worker_shutdown_timeout: 30s

# projects:
#   myproject: { domains: [example.com, "*.example.com"], addons: [redis], memory: 600m }

addons:
  redis:
    image: redis:7-alpine
    memory: 64m
    port: 6379
    volume: /data
    allow: []

logs:
  app:    { retention: 14d, max: 200MB }
  trace:  { retention: 7d, max: 2GB }
  metric: { retention: 30d, max: 500MB }
  access: { retention: 30d, max: 500MB }

limits:
  reserve_memory: 150m
  disk_free_alert: 15
  pids: 1024           # processes per container
  max_blob: 10g        # largest image layer a push may upload
  # cpus: 1            # cores per container (default: no cap)

# notify:
#   telegram: { token: "...", chat_id: "..." }

# Private npm registry at https://deploy.<domain>/npm/ (tokens: clowctl npm token).
# npm:
#   publish: true       # only on the one server that accepts publishes
#   keep: 3             # versions kept per package, besides dist-tagged ones
#   grace: 14d          # how long an old version stays deprecated before it is deleted
#   max_publish: 64m    # largest publish request

# Claude Code plugin marketplace at https://deploy.<domain>/plugins/marketplace.json, built from
# the npm packages marked "claude-plugin": true (users: clowctl plugins setup).
# plugins:
#   name: clowdr-example-com   # marketplace name (default: clowdr-<domain, dots as dashes>)
#   owner: Example Inc.        # shown as the owner (default: the domain)
#   max_zip: 64m               # largest plugin zip a publish may make (at most 256m)
`

func LoadPlatform(path string) (*Platform, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p, err := ParsePlatform(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

func ParsePlatform(b []byte) (*Platform, error) {
	var p Platform
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if err := checkSetNPM(b); err != nil {
		return nil, err
	}
	p.defaults()
	if err := p.validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// checkSetNPM refuses npm values that are written but would be replaced by a default: zero
// is "not set" for keep, grace and max_publish, so "keep: 0" would quietly mean 3.
func checkSetNPM(b []byte) error {
	var probe struct {
		NPM struct {
			Keep       *int      `yaml:"keep"`
			Grace      *Duration `yaml:"grace"`
			MaxPublish *Bytes    `yaml:"max_publish"`
		} `yaml:"npm"`
		Plugins struct {
			MaxZip *Bytes `yaml:"max_zip"`
		} `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(b, &probe); err != nil {
		return err
	}
	if k := probe.NPM.Keep; k != nil && *k < 1 {
		return fmt.Errorf("npm.keep: must be at least 1, got %d", *k)
	}
	if g := probe.NPM.Grace; g != nil && *g <= 0 {
		return errors.New("npm.grace: must be longer than 0")
	}
	if m := probe.NPM.MaxPublish; m != nil && *m <= 0 {
		return errors.New("npm.max_publish: must be larger than 0")
	}
	if m := probe.Plugins.MaxZip; m != nil && *m <= 0 {
		return errors.New("plugins.max_zip: must be larger than 0")
	}
	return nil
}

func (p *Platform) defaults() {
	if p.Listen == "" {
		p.Listen = "127.0.0.1:7070"
	}
	if p.Nginx.Image == "" {
		p.Nginx.Image = "nginx:alpine"
	}
	if p.Nginx.WorkerShutdownTimeout == 0 {
		p.Nginx.WorkerShutdownTimeout = Duration(30e9)
	}
	if p.Limits.MaxBlob == 0 {
		p.Limits.MaxBlob = 10 << 30
	}
	if p.NPM.Keep == 0 {
		p.NPM.Keep = 3
	}
	if p.NPM.Grace == 0 {
		p.NPM.Grace = Duration(14 * 24 * time.Hour)
	}
	if p.NPM.MaxPublish == 0 {
		p.NPM.MaxPublish = 64 << 20
	}
	if p.Plugins.MaxZip == 0 {
		p.Plugins.MaxZip = DefaultPluginMaxZip
	}
	if p.Limits.Pids == 0 {
		p.Limits.Pids = 1024
	}
	if p.Limits.ReserveMemory == 0 {
		p.Limits.ReserveMemory = 150 << 20
	}
	if p.Limits.DiskFreeAlert == 0 {
		p.Limits.DiskFreeAlert = 15
	}
	for name, a := range p.Addons {
		if a.Port == 0 && name == "redis" {
			a.Port = 6379
		}
		if a.Volume == "" && name == "redis" {
			a.Volume = "/data"
		}
		p.Addons[name] = a
	}
	if p.Logs.App == nil {
		p.Logs.App = &Retention{Retention: Duration(14 * 24 * 3600e9), Max: 200 << 20}
	}
	if p.Logs.Metric == nil {
		p.Logs.Metric = &Retention{Retention: Duration(30 * 24 * 3600e9), Max: 500 << 20}
	}
	if p.Logs.Trace == nil {
		p.Logs.Trace = &Retention{Retention: Duration(7 * 24 * 3600e9), Max: 2 << 30}
	}
	if p.Logs.Access == nil {
		p.Logs.Access = &Retention{Retention: Duration(30 * 24 * 3600e9), Max: 500 << 20}
	}
}

func (p *Platform) validate() error {
	if p.Domain != "" && !spec.ValidDomain(p.Domain) {
		return fmt.Errorf("domain: invalid %q", p.Domain)
	}
	for name, pr := range p.Projects {
		if !spec.ValidName(name) {
			return fmt.Errorf("projects: invalid name %q", name)
		}
		for _, d := range pr.Domains {
			if !spec.ValidDomain(d) {
				return fmt.Errorf("projects.%s: invalid domain %q", name, d)
			}
		}
		for _, a := range pr.Addons {
			if _, ok := p.Addons[a]; !ok {
				return fmt.Errorf("projects.%s: unknown addon %q", name, a)
			}
		}
	}
	for name, a := range p.Addons {
		if !spec.ValidName(name) {
			return fmt.Errorf("addons: invalid name %q", name)
		}
		if a.Image == "" {
			return fmt.Errorf("addons.%s: image is required", name)
		}
	}
	if p.Plugins.Name != "" {
		if err := ValidMarketplaceName(p.Plugins.Name); err != nil {
			return fmt.Errorf("plugins.name %q: %v", p.Plugins.Name, err)
		}
	} else if p.Domain != "" {
		n := MarketplaceName(p.Plugins, p.Domain, "")
		if err := ValidMarketplaceName(n); err != nil {
			return fmt.Errorf("plugins.name: the plugin marketplace's default name %q, made from domain %q, will not do: %v; set plugins.name in platform.yml", n, p.Domain, err)
		}
	}
	if p.Plugins.MaxZip < 0 || p.Plugins.MaxZip > PluginMaxZipCap {
		return fmt.Errorf("plugins.max_zip: from 1 byte to %d (Claude Code downloads at most 256 MiB)", PluginMaxZipCap)
	}
	if p.NPM.MaxPublish < 0 {
		return fmt.Errorf("npm.max_publish: must be positive")
	}
	for _, tp := range p.TrustedProxies {
		if tp != "cloudflare" && !strings.Contains(tp, "/") {
			return fmt.Errorf("trusted_proxies: %q is neither cloudflare nor a CIDR", tp)
		}
	}
	return nil
}

// DeployHost is the host name the API and registry are served on, or "" without a domain.
func (p *Platform) DeployHost() string {
	if p.Domain == "" {
		return ""
	}
	return "deploy." + p.Domain
}

// AddonAllowed reports whether project may run addon.
func (p *Platform) AddonAllowed(addon, project string) bool {
	a, ok := p.Addons[addon]
	if !ok {
		return false
	}
	return contains(a.Allow, project) || contains(a.Allow, "*")
}
