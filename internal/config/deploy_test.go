package config

import (
	"strings"
	"testing"
	"time"
)

const planExample = `
project: shop

envs:
  dev:  { server: https://deploy.example.net }
  prod: { server: https://deploy.example.com }

addons: [redis]

services:
  server:
    path: server
    build: yarn install && yarn build --type ${ENV}
    dockerfile: .ci/docker/Dockerfile
    port: 3000
    health: /health
    memory: 450m
    drain: 30s
    uses: [redis]
    domains:
      prod: [api.example.com]
      dev:  [api.example.net]
    routes:
      - { path: /ws, timeout: 24h, buffering: false }
    volumes:
      - data:/var/www/data

  admin:
    path: admin
    build: yarn install && yarn build:${ENV}
    dockerfile: .ci/docker/Dockerfile
    build_args: { TYPE: "${ENV}" }
    port: 3000
    health: { path: /health, timeout: 2m, successes: 3 }
    memory: 200m
    domains: { prod: [admin.example.com], dev: [admin.example.net] }
    env: [ADMIN_ALLOWED_IPS]

  landing:
    path: landing
    port: 8080
    memory: 32m
    max_body: 8m
    domains: { prod: [example.com], dev: [example.net] }
`

func TestParsePlanExample(t *testing.T) {
	d, err := ParseDeploy([]byte(planExample))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(d.ServiceNames(), ","); got != "server,admin,landing" {
		t.Fatalf("order = %s", got)
	}
	s, err := d.Spec("server", "prod")
	if err != nil {
		t.Fatal(err)
	}
	if s.Memory != 450<<20 || s.Port != 3000 || s.Drain != 30*time.Second || s.Replicas != 1 {
		t.Fatalf("bad spec %+v", s)
	}
	if len(s.Domains) != 1 || s.Domains[0] != "api.example.com" {
		t.Fatalf("domains %v", s.Domains)
	}
	if r := s.Routes[0]; r.Path != "/ws" || r.Timeout != 24*time.Hour || r.Buffering == nil || *r.Buffering {
		t.Fatalf("route %+v", r)
	}
	if v := s.Volumes[0]; v.Name != "data" || v.Target != "/var/www/data" {
		t.Fatalf("volume %+v", v)
	}
	if s.Health.Path != "/health" || s.Health.Successes != 2 {
		t.Fatalf("health %+v", s.Health)
	}
	a, _ := d.Spec("admin", "dev")
	if a.Health.Timeout != 2*time.Minute || a.Health.Successes != 3 || a.Domains[0] != "admin.example.net" {
		t.Fatalf("admin %+v", a)
	}
	l, _ := d.Spec("landing", "prod")
	if l.MaxBody == nil || *l.MaxBody != 8<<20 || l.Health.Path != "" {
		t.Fatalf("landing %+v", l)
	}
	if got := Expand(d.Services["admin"].BuildArgs["TYPE"], "prod"); got != "prod" {
		t.Fatalf("expand = %q", got)
	}
}

func TestParseErrors(t *testing.T) {
	base := "project: p\nenvs: {prod: {server: https://x}}\n"
	cases := map[string]string{
		"unknown field":       "services: {a: {port: 1, prot: 2}}",
		"bad project":         "services: {a: {port: 1}}\nproject: P!",
		"no services":         "services: {}",
		"host volume":         "services: {a: {volumes: [/etc:/x]}}",
		"domain without port": "services: {a: {domains: {prod: [a.com]}}}",
		"unknown env":         "services: {a: {port: 1, domains: {stage: [a.com]}}}",
		"bad domain":          "services: {a: {port: 1, domains: {prod: [a_b.com]}}}",
		"dup domain":          "services: {a: {port: 1, domains: {prod: [a.com]}}, b: {port: 1, domains: {prod: [a.com]}}}",
		"unknown route svc":   "services: {a: {port: 1, routes: [{path: /x, service: nope}]}}",
		"bad route path":      "services: {a: {port: 1, routes: [{path: 'x;'}]}}",
		"uses not in addons":  "services: {a: {uses: [redis]}}",
		"bad switch":          "services: {a: {switch: blue-green}}",
		"bad memory":          "services: {a: {memory: lots}}",
		"bad env name":        "services: {a: {env: [A-B]}}",
	}
	for name, tail := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseDeploy([]byte(base + tail)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestUnits(t *testing.T) {
	for in, want := range map[string]int64{"450m": 450 << 20, "1g": 1 << 30, "200MB": 200 << 20, "8m": 8 << 20, "0": 0, "512": 512, "64Mi": 64 << 20} {
		got, err := ParseBytes(in)
		if err != nil || got != want {
			t.Errorf("ParseBytes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for in, want := range map[string]time.Duration{"30s": 30 * time.Second, "14d": 14 * 24 * time.Hour, "1h": time.Hour} {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseDuration("-1s"); err == nil {
		t.Error("negative duration accepted")
	}
	if FormatBytes(450<<20) != "450m" {
		t.Error(FormatBytes(450 << 20))
	}
}

func FuzzParseDeploy(f *testing.F) {
	f.Add([]byte(planExample))
	f.Fuzz(func(t *testing.T, b []byte) {
		d, err := ParseDeploy(b)
		if err != nil {
			return
		}
		for _, env := range append(d.EnvNames(), "") {
			if _, err := d.Specs(env); err != nil {
				t.Fatalf("valid file, Specs(%q) failed: %v", env, err)
			}
		}
	})
}
