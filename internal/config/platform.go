package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

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

# notify:
#   telegram: { token: "...", chat_id: "..." }
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
	p.defaults()
	if err := p.validate(); err != nil {
		return nil, err
	}
	return &p, nil
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
