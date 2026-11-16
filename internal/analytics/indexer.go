package analytics

import (
	"sort"
	"time"
)

// Catalog is what has been seen in a project's records: events, their keys, the last time.
type Catalog struct {
	Events map[string]*CatalogEvent `json:"events"`
}

// CatalogEvent describes one event name.
type CatalogEvent struct {
	Last string            `json:"last"`
	Src  map[string]string `json:"src,omitempty"`  // client|server → last seen
	DM   map[string]string `json:"dm,omitempty"`   // key → last seen
	P    map[string]string `json:"p,omitempty"`    // key → last seen
	Set  map[string]string `json:"set,omitempty"`  // set and once keys → last seen
	Apps map[string]string `json:"apps,omitempty"` // app → last seen
}

const maxCatalogKeys = 200

func (c *Catalog) add(r *Record) {
	if c.Events == nil {
		c.Events = map[string]*CatalogEvent{}
	}
	if len(c.Events) >= 2000 && c.Events[r.E] == nil {
		return // a runaway producer must not grow the catalog without bound
	}
	e := c.Events[r.E]
	if e == nil {
		e = &CatalogEvent{}
		c.Events[r.E] = e
	}
	day := r.Day()
	if day > e.Last {
		e.Last = day
	}
	touch := func(m *map[string]string, k string) {
		if k == "" {
			return
		}
		if *m == nil {
			*m = map[string]string{}
		}
		if len(*m) >= maxCatalogKeys && (*m)[k] == "" {
			return
		}
		if day > (*m)[k] {
			(*m)[k] = day
		}
	}
	touch(&e.Src, r.Src)
	touch(&e.Apps, r.App)
	for k := range r.DM {
		touch(&e.DM, k)
	}
	for k := range r.P {
		touch(&e.P, k)
	}
	for k := range r.Set {
		touch(&e.Set, k)
	}
	for k := range r.Once {
		touch(&e.Set, k)
	}
}

// Names lists the event names, sorted.
func (c *Catalog) Names() []string {
	out := make([]string, 0, len(c.Events))
	for n := range c.Events {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// InstallDecision is what the indexer made of a $first_open.
type InstallDecision struct {
	Record Record
	Kind   string // install | reinstall | first_visit
	N      int    // the install's number on its key
}

// Index applies records to the state in one transaction: persons, links, actors, install keys,
// account deletion and the catalog. It returns the decisions on $first_open records.
func Index(st *State, recs []Record) ([]InstallDecision, error) {
	var out []InstallDecision
	err := st.Update(func(t *Tx) error {
		var cat Catalog
		t.Meta("catalog", &cat)
		exclude := map[string]any{"test": true}
		var cfg struct {
			Exclude map[string]any `json:"exclude"`
		}
		if t.Meta("config", &cfg) && cfg.Exclude != nil {
			exclude = cfg.Exclude
		}
		for i := range recs {
			r := &recs[i]
			cat.add(r)
			u := r.User()
			if r.E == EventAccountDeleted && u != "" {
				// The person goes, but a test account stays excluded: keep only the properties
				// the exclusion rule reads.
				old, _ := t.Person(u)
				if err := t.DeletePerson(u); err != nil {
					return err
				}
				kept := map[string]any{}
				for k := range exclude {
					if v, ok := old.Props[k]; ok {
						kept[k] = v
					}
				}
				if len(kept) > 0 {
					if err := t.PutPerson(u, Person{Props: kept}); err != nil {
						return err
					}
				}
				continue
			}
			if u != "" {
				dev := r.D
				if r.E == EventIdentify {
					dev = r.anon()
				}
				linked, err := t.SetLink(dev, u)
				if err != nil {
					return err
				}
				p, _ := t.Person(u)
				p.apply(r)
				if err := t.PutPerson(u, p); err != nil {
					return err
				}
				if linked {
					if _, err := t.EnsureActor("u:"+u, r); err != nil {
						return err
					}
					if err := t.mergeActor("d:"+dev, "u:"+u); err != nil {
						return err
					}
				}
			}
			if key := t.ActorKey(r); key != "" {
				if _, err := t.EnsureActor(key, r); err != nil {
					return err
				}
			}
			if r.E == EventFirstOpen {
				kind, n, err := t.recordInstall(r)
				if err != nil {
					return err
				}
				out = append(out, InstallDecision{Record: *r, Kind: kind, N: n})
			}
			for typ, key := range r.G {
				if _, err := t.GroupID(typ, str(key)); err != nil {
					return err
				}
			}
		}
		return t.PutMeta("catalog", cat)
	})
	return out, err
}

// indexer feeds records from the collector to Index in batches, off the collector's goroutine.
type indexer struct {
	in      chan Record
	sync    chan chan struct{}
	dropped int64
	done    chan struct{}
}

const (
	indexBatch = 500
	indexWait  = 200 * time.Millisecond
)

func (p *Project) runIndexer() {
	defer close(p.idx.done)
	batch := make([]Record, 0, indexBatch)
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	flush := func() {
		if len(batch) == 0 {
			return
		}
		p.indexMu.Lock() // a rebuild holds it while it rewrites the state
		decisions, err := Index(p.state, batch)
		p.indexMu.Unlock()
		if err != nil {
			p.m.log().Error("analytics: index", "project", p.name, "err", err)
		}
		batch = batch[:0]
		p.onInstalls(decisions)
	}
	for {
		select {
		case r, ok := <-p.idx.in:
			if !ok {
				flush()
				return
			}
			if len(batch) == 0 {
				timer.Reset(indexWait)
			}
			batch = append(batch, r)
			if len(batch) >= indexBatch {
				flush()
			}
		case <-timer.C:
			flush()
		case ack := <-p.idx.sync:
			for drained := false; !drained; {
				select {
				case r, ok := <-p.idx.in:
					if !ok {
						drained = true
						break
					}
					batch = append(batch, r)
				default:
					drained = true
				}
			}
			flush()
			close(ack)
		}
	}
}

// Sync waits until every record handed to the indexer so far is in the state.
func (p *Project) Sync() {
	ack := make(chan struct{})
	select {
	case p.idx.sync <- ack:
		<-ack
	case <-p.idx.done:
	}
}
