package ingress

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veksa/clowdr/internal/spec"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/ingress -update)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from golden file; got:\n%s", name, got)
	}
}

func ptr[T any](v T) *T { return &v }

func svc(s spec.Service) spec.Service { s.Defaults(); return s }

func TestRenderShop(t *testing.T) {
	views := []ServiceView{
		{Spec: svc(spec.Service{
			Name: "server", Port: 3000, Domains: []string{"api.example.com"},
			Routes: []spec.Route{{Path: "/ws", Timeout: 24 * time.Hour, Buffering: ptr(false)}},
		}), Targets: []string{"shop-server-r12-1:3000"}},
		{Spec: svc(spec.Service{
			Name: "landing", Port: 8080, Domains: []string{"example.com", "www.example.com"},
			MaxBody: ptr(int64(8 << 20)),
		}), Targets: []string{"shop-landing-r3-1:8080", "shop-landing-r3-2:8080"}},
		{Spec: svc(spec.Service{Name: "worker"})},
	}
	got, err := RenderProject("shop", views, Options{Syslog: true})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "shop.conf", got)
}

func TestRenderSpecialCases(t *testing.T) {
	views := []ServiceView{
		// blog api routes /.well-known/ to web, video-style tls: direct + tus uploads.
		{Spec: svc(spec.Service{
			Name: "api", Port: 3000, Domains: []string{"api.example.org"},
			Routes: []spec.Route{
				{Path: "/.well-known/", Service: "web"},
				{Path: "/files/", MaxBody: ptr(int64(0)), Timeout: time.Hour, Buffering: ptr(false)},
				{Path: "/gone/", Service: "missing"},
			},
		}), Targets: []string{"blog-api-r1-1:3000"}},
		{Spec: svc(spec.Service{Name: "web", Port: 80})},
		{Spec: svc(spec.Service{
			Name: "stream", Port: 8000, TLS: spec.TLSDirect,
			Domains:    []string{"stream.example.org", "cdn.example.org"},
			NginxExtra: "add_header X-Frame-Options DENY;\nlocation /hls/ { add_header Cache-Control no-cache; proxy_pass http://blog__stream; }",
		}), Targets: []string{"blog-stream-r4-1:8000"}},
	}
	o := Options{HasCert: func(d string) bool { return d == "stream.example.org" }}
	got, err := RenderProject("blog", views, o)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "blog.conf", got)
}

func TestProxyOnly(t *testing.T) {
	views := []ServiceView{
		{Spec: svc(spec.Service{Name: "web", Port: 80, Domains: []string{"example.com"}})},
		{Spec: svc(spec.Service{Name: "cdn", Port: 80, Domains: []string{"cdn.example.com"}, TLS: spec.TLSDirect})},
	}
	guard := "if ($clowdr_via_proxy = 0)"
	got, err := RenderProject("shop", views, Options{TrustedProxies: []string{"cloudflare"}})
	if err != nil {
		t.Fatal(err)
	}
	server := func(name string) string {
		s, _, _ := strings.Cut(got[strings.Index(got, "server_name "+name+";"):], "\n}\n")
		return s
	}
	web, cdn := server("example.com"), server("cdn.example.com")
	if !strings.Contains(web, guard) {
		t.Error("site behind the proxy is reachable directly")
	}
	if strings.Contains(cdn, guard) {
		t.Error("tls: direct site is limited to the proxy")
	}
	got, _ = RenderProject("shop", views, Options{})
	if strings.Contains(got, guard) {
		t.Error("guard without trusted proxies")
	}
	if strings.Contains(RenderMain(Options{}), "clowdr_via_proxy") {
		t.Error("geo without trusted proxies")
	}
}

func TestRenderMain(t *testing.T) {
	o := Options{
		Syslog: true, TrustedProxies: []string{"cloudflare"}, WorkerShutdownTimeout: 30 * time.Second,
		DeployHost: "deploy.example.com", HasCert: func(string) bool { return true },
	}
	golden(t, "nginx.conf", RenderMain(o))
	plain := RenderMain(Options{TrustedProxies: []string{"10.0.0.0/8"}, DeployHost: "deploy.x.com"})
	for _, want := range []string{"real_ip_header X-Forwarded-For", "access_log /dev/stdout", "return 503"} {
		if !strings.Contains(plain, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(plain, "listen 443") {
		t.Error("443 without a certificate")
	}
}

func TestCheckExtra(t *testing.T) {
	ok := []string{
		"add_header X 1;",
		"location /a { return 200 '}'; }",
		"# } comment\nadd_header a b;",
		"location /hls/ { proxy_pass http://p__stream; }",
		"location /api/ { proxy_pass http://p__api/v2/; }",
		"if ($http_x_a){ return 403; }",
		`add_header "X-A" "b c";`,
	}
	bad := []string{
		"} server { listen 80; server_name victim.com; }",
		"location /a {",
		"include /etc/passwd;",
		"location /x { alias /var/lib/clowdr/certs/; }",
		"location /k {alias /var/lib/clowdr/certs/;}",
		`location /k { "alias" /var/lib/clowdr/certs/; }`,
		"location /k { root /; }",
		"access_log /tmp/x;",
		"return 200 'x",
		"add_header X 1",
		"proxy_pass http://other__web;",
		"proxy_pass http://p-admin__web;",
		"proxy_pass http://169.254.169.254/;",
		"proxy_pass http://p__web/$uri;",
		"add_header X ${a};",
		"add_header {;",
		"mirror /m;",
		"add_header X a#; } location ~ ^/k { alias /var/lib/clowdr/certs/; } location /z { add_header Y b\n;",
		"add_header X a}; root /;",
		`add_header X a\; root /;`,
		`add_header X "a"b;`,
		`add_header X "a\"; root /; #";`,
	}
	for _, s := range ok {
		if err := checkExtra("p", s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	for _, s := range bad {
		if err := checkExtra("p", s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	_, err := RenderProject("p", []ServiceView{{Spec: spec.Service{Name: "a", Port: 1, Domains: []string{"a.com"}, NginxExtra: "}"}}}, Options{})
	if err == nil {
		t.Error("render accepted bad extra")
	}
}
