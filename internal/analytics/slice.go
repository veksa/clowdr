package analytics

import (
	"fmt"
	"sort"
	"strings"

	"github.com/veksa/clowdr/internal/config"
)

// AppAll asks a query for every app, whatever analytics.apps says.
const AppAll = "all"

// rawKinds read raw records: an app or a platform is a filter on them.
var rawKinds = map[string]bool{"trends": true, "funnel": true, "paths": true, "events": true}

// categoryKinds count people and default to the product when analytics.apps is set.
var categoryKinds = map[string]bool{"trends": true, "funnel": true, "paths": true, "retention": true,
	"stickiness": true, "lifecycle": true, "installs": true}

// setSlice reads the spec's app and platform: a category selects the rollups' parts of that
// category (and filters raw records by it); one app or one platform is a filter for the raw
// kinds and a projection of the rollups by that dimension for the others.
func (q *qctx) setSlice(s *Spec) error {
	q.cats = newCategories(q.p.Config())
	app := s.App
	if app == "" && q.cats != nil && categoryKinds[s.Kind] {
		app = config.CategoryProduct
	}
	if (s.App != "" || s.Platform != "") && (s.Kind == "person" || s.Kind == "catalog") {
		return fmt.Errorf("%s does not take an app or a platform", s.Kind)
	}
	var proj [][2]string
	switch {
	case app == "" || app == AppAll:
	case IsCategory(app):
		if q.cats == nil {
			return fmt.Errorf("app %q is a category, but deploy.yml sets no analytics.apps: name an app (mobile, landing, …) or set analytics.apps", app)
		}
		q.cat = app
	default:
		proj = append(proj, [2]string{"app", app})
	}
	if s.Platform != "" {
		proj = append(proj, [2]string{"pf", s.Platform})
	}
	if rawKinds[s.Kind] {
		for _, p := range proj {
			s.Filters = append(s.Filters, p[0]+"="+p[1])
		}
	} else if len(proj) > 1 {
		return fmt.Errorf("%s reads rollups, which split by one app or one platform at a time: use a category (product, site, internal) with --platform", s.Kind)
	} else if len(proj) == 1 {
		q.projDim, q.projVal = proj[0][0], proj[0][1]
	}
	var cover []string
	switch {
	case q.cat != "":
		cover = append(cover, q.cats.Describe(q.cat))
	case app != "" && app != AppAll:
		cover = append(cover, "app "+app)
	case q.cats != nil && categoryKinds[s.Kind]:
		cover = append(cover, "all apps (site and internal ones too)")
	default:
		cover = append(cover, "all apps")
	}
	if s.Platform != "" {
		cover = append(cover, "platform "+s.Platform)
	}
	q.covers = strings.Join(cover, " · ")
	return nil
}

// inCategory reports whether a raw record belongs to the query's category, as ComputeDay
// decides it; true when the query has none.
func (q *qctx) inCategory(t *Tx, r *Record) bool {
	if q.cat == "" {
		return true
	}
	return q.cats.Of(r.App, strings.HasPrefix(t.ActorKey(r), "u:")) == q.cat
}

// parts are the parts of a day the query counts: the category's (or the whole day's),
// projected on one app or platform when the query asks for it.
func (q *qctx) parts(d *DayRollup) []*Part {
	if d == nil {
		return nil
	}
	var ps []*Part
	if q.cat != "" {
		ps = d.Cat(q.cat).Parts(q.spec.IncludeTest)
	} else {
		ps = d.Parts(q.spec.IncludeTest)
	}
	if q.projDim == "" {
		return ps
	}
	if q.projected == nil {
		q.projected = map[*DayRollup][]*Part{}
	}
	if out, ok := q.projected[d]; ok {
		return out
	}
	out := make([]*Part, 0, len(ps))
	for _, p := range ps {
		out = append(out, projectPart(p, q.projDim, q.projVal))
	}
	q.projected[d] = out
	return out
}

// testPart is the excluded persons' part of a day in the query's category.
func (q *qctx) testPart(d *DayRollup) *Part {
	if d == nil {
		return nil
	}
	if q.cat != "" {
		if c := d.Cat(q.cat); c != nil {
			return c.Test
		}
		return nil
	}
	return d.Test
}

// projectPart is the part of the actors and events of one value of a dimension (app or pf):
// actives, new actors and persons are cut to the actors active with that value, events to their
// cells of it. Other dimensions are not crossed with it in a rollup, so the projection keeps
// only that one (and, for a platform, its versions).
func projectPart(p *Part, dim, val string) *Part {
	var act *Bitmap
	if c := p.ActiveBy[dim][val]; c != nil {
		act = c.U.Clone()
	} else {
		act = newBitmap()
	}
	out := &Part{Active: act, First: and(p.First, act), Persons: and(p.Persons, act), ActiveBy: Dims{},
		Events: map[string]*EventAgg{}}
	crossed := func(by Dims) Dims {
		o := Dims{}
		if c := by[dim][val]; c != nil {
			o[dim] = map[string]*Cell{val: c}
		}
		if dim == "pf" {
			for v, c := range by[PFAV] {
				if av, ok := strings.CutPrefix(v, val+" "); ok {
					if o[PFAV] == nil {
						o[PFAV], o["av"] = map[string]*Cell{}, map[string]*Cell{}
					}
					o[PFAV][v], o["av"][av] = c, c
				}
			}
		}
		return o
	}
	out.ActiveBy = crossed(p.ActiveBy)
	for name, e := range p.Events {
		if c := e.By[dim][val]; c != nil {
			out.Events[name] = &EventAgg{Cell: *c, By: crossed(e.By)}
		}
	}
	if p.Installs != nil {
		in := &Installs{}
		for _, k := range []string{KindInstall, KindReinstall, KindFirstVisit} {
			if c := p.Installs.Get(k).By[dim][val]; c != nil {
				*in.kind(k) = &InstallAgg{Cell: *c, By: Dims{dim: {val: c}}}
			}
		}
		out.Installs = in
	}
	return out
}

// staleCats lists the days whose rollup was computed with another categorisation than the
// config's. Days in the hot window are recomputed on the spot; the rest are returned, for a
// note that asks for a rebuild.
func (p *Project) staleCats(ds []string) []string {
	cats := newCategories(p.Config())
	if cats == nil {
		return nil
	}
	key := cats.Key()
	var stale []string
	for _, d := range ds {
		if r := p.day(d); r != nil && r.CatsKey != key {
			stale = append(stale, d)
		}
	}
	if len(stale) == 0 {
		return nil
	}
	first := p.firstComputable()
	today := p.m.now().Format(DayLayout)
	var inWindow []string
	for _, d := range stale {
		if d >= first && d <= today {
			inWindow = append(inWindow, d)
		}
	}
	p.Recompute(inWindow)
	var left []string
	for _, d := range stale {
		if r := p.day(d); r != nil && r.CatsKey != key {
			left = append(left, d)
		}
	}
	sort.Strings(left)
	return left
}

// staleNote is the note for days rolled up before analytics.apps said what they are now.
func staleNote(stale []string) string {
	if len(stale) == 0 {
		return ""
	}
	return fmt.Sprintf("%d days (%s … %s) were rolled up before analytics.apps took its current form and count no categories: clowctl analytics rebuild --since %s",
		len(stale), stale[0], stale[len(stale)-1], stale[0])
}
