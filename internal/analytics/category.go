package analytics

import (
	"sort"
	"strings"

	"github.com/veksa/clowdr/internal/config"
)

// Categories sorts records into the product, the site and internal tools by their app
// (deploy.yml analytics.apps). A record of an app no list names (the server's own records)
// counts toward the product when its actor is a person: a signed-in account acting through the
// server is a user of the product. Without a person it counts toward no category.
type Categories struct {
	byApp map[string]string
	key   string
}

// newCategories builds the categorisation of a config; nil when it has none.
func newCategories(c *config.AnalyticsConf) *Categories {
	if c == nil || c.Apps == nil {
		return nil
	}
	cs := &Categories{byApp: map[string]string{}}
	var parts []string
	for _, cat := range config.Categories {
		apps := append([]string(nil), c.Apps.List(cat)...)
		sort.Strings(apps)
		for _, a := range apps {
			cs.byApp[a] = cat
		}
		parts = append(parts, cat+"="+strings.Join(apps, ","))
	}
	cs.key = strings.Join(parts, ";")
	return cs
}

// Key identifies the categorisation; a rollup keeps the key it was computed with.
func (c *Categories) Key() string {
	if c == nil {
		return ""
	}
	return c.key
}

// Of is the category of a record of app whose actor is (or is not) a person; "" for none.
func (c *Categories) Of(app string, person bool) string {
	if c == nil {
		return ""
	}
	if cat, ok := c.byApp[app]; ok {
		return cat
	}
	if person {
		return config.CategoryProduct
	}
	return ""
}

// IsCategory reports whether s names a category.
func IsCategory(s string) bool {
	for _, c := range config.Categories {
		if c == s {
			return true
		}
	}
	return false
}

// Describe says in a few words what a category covers, for notes and the skill.
func (c *Categories) Describe(cat string) string {
	if c == nil {
		return cat
	}
	var apps []string
	for a, ac := range c.byApp {
		if ac == cat {
			apps = append(apps, a)
		}
	}
	sort.Strings(apps)
	s := cat + " (" + strings.Join(apps, ", ")
	if cat == config.CategoryProduct {
		if len(apps) > 0 {
			s += ", "
		}
		s += "records of other apps with a person"
	}
	return s + ")"
}

// PFAV is the dimension of a platform and its app version together ("ios 2.3.1"), so versions
// can be read per platform: iOS and Android ship different ones.
const PFAV = "pf_av"
