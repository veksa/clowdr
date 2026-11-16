package config

import (
	"strings"
	"testing"
)

const analyticsBase = `
project: shop
envs:
  prod: { server: https://deploy.example.com }
services:
  server: { image: x, port: 3000 }
`

func TestAnalyticsConf(t *testing.T) {
	d, err := ParseDeploy([]byte(analyticsBase + `
analytics:
  archive: { endpoint: https://acc.r2.cloudflarestorage.com, region: auto, bucket: shop-analytics }
  digest: { hour: 6, lang: en, name: Shop, key_events: [episode_viewed], access: true }
  installs: each
  exclude: { test: true, role: staff }
`))
	if err != nil {
		t.Fatal(err)
	}
	a := d.Analytics
	if !a.ArchiveOn() || a.DigestHour() != 6 || a.InstallsMode() != InstallsEach || a.ExcludeRule()["role"] != "staff" || !a.Digest.Access {
		t.Fatalf("%+v", a)
	}
	d, err = ParseDeploy([]byte(analyticsBase + "analytics:\n  archive: off\n"))
	if err != nil || d.Analytics.ArchiveOn() || d.Analytics.DigestHour() != 7 || d.Analytics.InstallsMode() != InstallsDigest || d.Analytics.ExcludeRule()["test"] != true {
		t.Fatalf("defaults: %+v %v", d.Analytics, err)
	}
	var nilConf *AnalyticsConf
	if nilConf.ArchiveOn() || nilConf.DigestHour() != 7 {
		t.Fatal("nil conf")
	}
	for body, want := range map[string]string{
		"archive: sometimes":                                      "neither off nor a mapping",
		"archive: { endpoint: ftp://x, bucket: b }":               "not an http(s) URL",
		"archive: { endpoint: https://x }":                        "bucket is required",
		"archive: { endpoint: https://x, bucket: b, style: odd }": "neither path nor virtual",
		"archive: { endpoint: https://x, bucket: b, secret: s }":  "secret",
		"digest: { hour: 24 }":                                    "not 0..23",
		"digest: { lang: de }":                                    "neither ru nor en",
		"digest: { key_events: [Bad] }":                           "not an event name",
		"installs: often":                                         "not digest, hourly or each",
		"exclude: { test: [1] }":                                  "a string, number or boolean",
		"unknown: 1":                                              "unknown",
	} {
		if _, err := ParseDeploy([]byte(analyticsBase + "analytics:\n  " + body + "\n")); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", body, err, want)
		}
	}
}
