package ingress

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/veksa/clowdr/internal/spec"
)

// Every ACME location passes a normalised URI and sets the forwarding headers itself, and
// only the deploy host's own location marks requests as its own.
func TestACMELocationsReachOnlyChallenges(t *testing.T) {
	main := RenderMain(Options{DeployHost: "deploy.example.com", HasCert: func(string) bool { return true }})
	site, err := RenderProject("shop", []ServiceView{{Spec: svc(spec.Service{Name: "web", Port: 80, Domains: []string{"example.com"}, TLS: spec.TLSDirect}), Targets: []string{"t:80"}}},
		Options{HasCert: func(string) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`location \^~ /\.well-known/acme-challenge/ \{[^}]*\}`)
	n := 0
	for _, conf := range []string{main, site} {
		for _, b := range block.FindAllString(conf, -1) {
			n++
			for _, want := range []string{
				"proxy_pass http://unix:" + APISocket + ":" + ACMEChallenge + ";",
				"proxy_set_header X-Forwarded-For $remote_addr;",
				`proxy_set_header X-Forwarded-Host "";`,
				`proxy_set_header X-Forwarded-Proto "";`,
				`proxy_set_header X-Clowdr-Via "";`,
			} {
				if !strings.Contains(b, want) {
					t.Errorf("ACME location lacks %q:\n%s", want, b)
				}
			}
		}
	}
	if n != 3 { // the default server, the deploy host on :80, the site on :80
		t.Fatalf("found %d ACME locations", n)
	}
	if strings.Count(main, "proxy_set_header X-Clowdr-Via deploy-host;") != 1 {
		t.Error("the deploy host's location must mark its requests, once")
	}
	if !strings.Contains(ProxyDefaultsConf, `proxy_set_header X-Clowdr-Via "";`) {
		t.Error("sites must not pass a client's marker on")
	}
}

func TestFromDeployHost(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/projects", nil)
	r.RemoteAddr = "@"
	if !FromAPISocket(r) || FromDeployHost(r) {
		t.Fatal("unix socket without the marker")
	}
	r.Header.Set(ViaHeader, ViaDeployHost)
	if !FromDeployHost(r) {
		t.Fatal("marked request")
	}
	r.RemoteAddr = "203.0.113.1:1234"
	if FromAPISocket(r) || FromDeployHost(r) {
		t.Fatal("a TCP peer is not nginx")
	}
	for path, ok := range map[string]bool{
		"/.well-known/acme-challenge/abc_-1":                  true,
		"/.well-known/acme-challenge/":                        false,
		"/.well-known/acme-challenge/a/b":                     false,
		"/.well-known/acme-challenge/..%2F..%2Fv1%2Fprojects": false,
		"/npm/x/..%2F..%2F.well-known/acme-challenge/t":       false,
		"/plugins/x/..%2F..%2F.well-known/acme-challenge/t":   false,
		"/.well-known/acme-challenge/%2e%2e":                  false,
	} {
		r := httptest.NewRequest("GET", "http://h"+path, nil)
		if IsACMEChallenge(r) != ok {
			t.Errorf("%s: %v", path, !ok)
		}
	}
}

// The deploy host's TLS server passes every path to clowdrd through one marked location, so the
// npm registry and the plugin marketplace need nothing of their own; and nothing logs headers.
func TestDeployHostPassesEveryPath(t *testing.T) {
	main := RenderMain(Options{DeployHost: "deploy.example.com", HasCert: func(string) bool { return true }})
	i := strings.Index(main, "listen 443 ssl;")
	if i < 0 {
		t.Fatal("no TLS server for the deploy host")
	}
	tls := main[i:]
	if strings.Count(tls, "location ") != 1 || !strings.Contains(tls, "location / {") || !strings.Contains(tls, "proxy_pass http://unix:"+APISocket) {
		t.Fatalf("the deploy host must pass every path through one location:\n%s", tls)
	}
	if !strings.Contains(tls, "client_max_body_size 0;") {
		t.Error("the deploy host must not cap bodies (registry pushes and publishes have their own limits)")
	}
	for _, h := range []string{"$http_authorization", "$http_cookie"} {
		if strings.Contains(main, h) {
			t.Errorf("nginx.conf logs or uses %s", h)
		}
	}
}
