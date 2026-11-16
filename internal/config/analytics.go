package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"

	"gopkg.in/yaml.v3"
)

// AnalyticsConf is the analytics block of deploy.yml (docs/analytics.md). Secrets are not here:
// they are set with `clowctl analytics secret set`. The same struct travels to the server as JSON.
type AnalyticsConf struct {
	// Archive is where clowdr uploads each raw day and the rollups; nil or Off = no upload.
	Archive *ArchiveConf `yaml:"archive" json:"archive,omitempty"`
	// Digest is the daily Telegram digest; nil = defaults (07:00 UTC, Russian).
	Digest *DigestConf `yaml:"digest" json:"digest,omitempty"`
	// Installs: digest (only in the daily digest, the default), hourly or each.
	Installs string `yaml:"installs" json:"installs,omitempty"`
	// Exclude drops persons whose properties match every pair from every query, unless the query
	// asks to include them. Default: {test: true}.
	Exclude map[string]any `yaml:"exclude" json:"exclude,omitempty"`
	// Apps sorts the values of the records' `app` into the product, the site and internal tools,
	// so that actives, retention and the digest count the people of the product only.
	Apps *AppsConf `yaml:"apps" json:"apps,omitempty"`
}

// Categories of apps.
const (
	CategoryProduct  = "product"
	CategorySite     = "site"
	CategoryInternal = "internal"
)

// Categories lists the categories in the order they are shown.
var Categories = []string{CategoryProduct, CategorySite, CategoryInternal}

// AppsConf lists the apps of each category. An app in no list (the server's own records, app
// `server`) counts toward the product when its record has a person, and toward nothing otherwise.
type AppsConf struct {
	Product  []string `yaml:"product" json:"product,omitempty"`
	Site     []string `yaml:"site" json:"site,omitempty"`
	Internal []string `yaml:"internal" json:"internal,omitempty"`
}

// List returns the apps of a category.
func (a *AppsConf) List(category string) []string {
	if a == nil {
		return nil
	}
	switch category {
	case CategoryProduct:
		return a.Product
	case CategorySite:
		return a.Site
	case CategoryInternal:
		return a.Internal
	}
	return nil
}

var appNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,31}$`)

// ArchiveConf names the S3-compatible bucket of the daily archive. `archive: off` disables it.
type ArchiveConf struct {
	Off      bool   `yaml:"-" json:"off,omitempty"`
	Endpoint string `yaml:"endpoint" json:"endpoint,omitempty"`
	Region   string `yaml:"region" json:"region,omitempty"`
	Bucket   string `yaml:"bucket" json:"bucket,omitempty"`
	// Style is path (https://endpoint/bucket/key, the default) or virtual (https://bucket.endpoint/key).
	Style string `yaml:"style" json:"style,omitempty"`
}

func (a *ArchiveConf) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		switch n.Value {
		case "off", "false":
			a.Off = true
			return nil
		}
		return fmt.Errorf("analytics.archive: %q is neither off nor a mapping", n.Value)
	}
	type plain ArchiveConf
	var p plain
	dec := n
	if err := decodeKnown(dec, &p); err != nil {
		return err
	}
	*a = ArchiveConf(p)
	return nil
}

// decodeKnown decodes a node refusing unknown keys, as the top-level decoder does.
func decodeKnown(n *yaml.Node, v any) error {
	b, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	return newStrictDecoder(b).Decode(v)
}

// DigestConf configures the daily digest.
type DigestConf struct {
	// Hour is the UTC hour the digest of the previous day is sent at; default 7.
	Hour *int `yaml:"hour" json:"hour,omitempty"`
	// Lang is ru (default) or en.
	Lang string `yaml:"lang" json:"lang,omitempty"`
	// Name is the product name in the heading; default: the project name.
	Name string `yaml:"name" json:"name,omitempty"`
	// KeyEvents get a line with their counts and people.
	KeyEvents []string `yaml:"key_events" json:"key_events,omitempty"`
	// Access adds page views and visitors from the nginx access log (sites without events).
	Access bool `yaml:"access" json:"access,omitempty"`
	// Off sends no digest.
	Off bool `yaml:"off" json:"off,omitempty"`
}

// Install notification modes.
const (
	InstallsDigest = "digest"
	InstallsHourly = "hourly"
	InstallsEach   = "each"
)

var analyticsEventRe = regexp.MustCompile(`^[$a-z][a-z0-9_]{1,47}$`)

// DigestHour is the hour the digest goes out.
func (c *AnalyticsConf) DigestHour() int {
	if c != nil && c.Digest != nil && c.Digest.Hour != nil {
		return *c.Digest.Hour
	}
	return 7
}

// InstallsMode is the install notification mode with its default.
func (c *AnalyticsConf) InstallsMode() string {
	if c == nil || c.Installs == "" {
		return InstallsDigest
	}
	return c.Installs
}

// ExcludeRule is the exclusion with its default.
func (c *AnalyticsConf) ExcludeRule() map[string]any {
	if c == nil || c.Exclude == nil {
		return map[string]any{"test": true}
	}
	return c.Exclude
}

// ArchiveOn reports whether an archive bucket is configured (the accessors read a nil block
// as the defaults).
func (c *AnalyticsConf) ArchiveOn() bool {
	return c != nil && c.Archive != nil && !c.Archive.Off && c.Archive.Bucket != ""
}

// Validate checks the block; deploy.yml parsing and the server both call it.
func (c *AnalyticsConf) Validate() error {
	if a := c.Archive; a != nil && !a.Off {
		u, err := url.Parse(a.Endpoint)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("analytics.archive.endpoint: %q is not an http(s) URL", a.Endpoint)
		}
		if a.Bucket == "" {
			return errors.New("analytics.archive.bucket is required (or archive: off)")
		}
		if a.Style != "" && a.Style != "path" && a.Style != "virtual" {
			return fmt.Errorf("analytics.archive.style: %q is neither path nor virtual", a.Style)
		}
	}
	if d := c.Digest; d != nil {
		if d.Hour != nil && (*d.Hour < 0 || *d.Hour > 23) {
			return fmt.Errorf("analytics.digest.hour: %d is not 0..23", *d.Hour)
		}
		if d.Lang != "" && d.Lang != "ru" && d.Lang != "en" {
			return fmt.Errorf("analytics.digest.lang: %q is neither ru nor en", d.Lang)
		}
		for _, e := range d.KeyEvents {
			if !analyticsEventRe.MatchString(e) {
				return fmt.Errorf("analytics.digest.key_events: %q is not an event name", e)
			}
		}
	}
	switch c.Installs {
	case "", InstallsDigest, InstallsHourly, InstallsEach:
	default:
		return fmt.Errorf("analytics.installs: %q is not digest, hourly or each", c.Installs)
	}
	if c.Apps != nil {
		seen := map[string]string{}
		for _, cat := range Categories {
			for _, app := range c.Apps.List(cat) {
				if !appNameRe.MatchString(app) {
					return fmt.Errorf("analytics.apps.%s: %q is not an app name", cat, app)
				}
				if prev, ok := seen[app]; ok {
					return fmt.Errorf("analytics.apps: %q is in both %s and %s", app, prev, cat)
				}
				seen[app] = cat
			}
		}
	}
	for k, v := range c.Exclude {
		switch v.(type) {
		case string, bool, int, float64:
		default:
			return fmt.Errorf("analytics.exclude.%s: a string, number or boolean", k)
		}
	}
	return nil
}
