package analytics

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

// State is a project's index in state.db (bbolt): actors and their numbers in bitmaps, persons,
// device links, install keys, groups, and meta (config, secrets, catalog, schedules).
type State struct {
	db *bolt.DB
}

var (
	bActors   = []byte("actors")       // actor key (u:<id> | d:<id>) → actorRec
	bActorIDs = []byte("actor_ids")    // uint32 big-endian → actor key
	bPersons  = []byte("persons")      // user id → Person
	bLinks    = []byte("links")        // device id → user id (the first link wins)
	bInstall  = []byte("install_keys") // install key → InstallKey
	bGroups   = []byte("groups")       // type \x00 key → uint32
	bMeta     = []byte("meta")         // name → JSON
	stateBkts = [][]byte{bActors, bActorIDs, bPersons, bLinks, bInstall, bGroups, bMeta}
)

// OpenState opens (or creates) a state file.
func OpenState(path string) (*State, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open analytics state %s: %w", path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range stateBkts {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &State{db: db}, nil
}

func (s *State) Close() error { return s.db.Close() }

// Tx is a state transaction.
type Tx struct{ tx *bolt.Tx }

// Update runs fn in a write transaction.
func (s *State) Update(fn func(*Tx) error) error {
	return s.db.Update(func(tx *bolt.Tx) error { return fn(&Tx{tx}) })
}

// View runs fn in a read transaction.
func (s *State) View(fn func(*Tx) error) error {
	return s.db.View(func(tx *bolt.Tx) error { return fn(&Tx{tx}) })
}

func getJSON[T any](tx *bolt.Tx, bucket []byte, key string) (T, bool) {
	var v T
	b := tx.Bucket(bucket).Get([]byte(key))
	if b == nil {
		return v, false
	}
	return v, json.Unmarshal(b, &v) == nil
}

func putJSON(tx *bolt.Tx, bucket []byte, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return tx.Bucket(bucket).Put([]byte(key), b)
}

// ─── Actors ──────────────────────────────────────────────────────────────────

// Actor is an actor's number in bitmaps and what was first seen of it.
type Actor struct {
	ID            uint32 `json:"id"`
	Key           string `json:"key,omitempty"`
	FirstDay      string `json:"first_day,omitempty"`
	FirstPF       string `json:"first_pf,omitempty"`
	FirstApp      string `json:"first_app,omitempty"`
	FirstCC       string `json:"first_cc,omitempty"`
	FirstSource   string `json:"first_source,omitempty"`
	FirstCampaign string `json:"first_campaign,omitempty"`
}

// Attr returns a first_* attribute by name.
func (a Actor) Attr(k string) string {
	switch k {
	case "first_day":
		return a.FirstDay
	case "first_pf":
		return a.FirstPF
	case "first_app":
		return a.FirstApp
	case "first_cc":
		return a.FirstCC
	case "first_source":
		return a.FirstSource
	case "first_campaign":
		return a.FirstCampaign
	}
	return ""
}

// ActorAttrs are the actor attributes queries break down by.
var ActorAttrs = []string{"first_source", "first_campaign", "first_pf", "first_app", "first_cc", "first_day"}

func idKey(id uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], id)
	return b[:]
}

// Actor returns an actor by key.
func (t *Tx) Actor(key string) (Actor, bool) { return getJSON[Actor](t.tx, bActors, key) }

// ActorByID returns an actor by number.
func (t *Tx) ActorByID(id uint32) (Actor, bool) {
	k := t.tx.Bucket(bActorIDs).Get(idKey(id))
	if k == nil {
		return Actor{}, false
	}
	a, ok := t.Actor(string(k))
	a.Key = string(k)
	return a, ok
}

// PutActor stores an actor that already has its number.
func (t *Tx) PutActor(key string, a Actor) error {
	a.Key = ""
	return putJSON(t.tx, bActors, key, a)
}

// EnsureActor returns the actor of key, numbering it and recording what the event shows of it
// when it is new. An event older than the actor's first day moves the first day back.
func (t *Tx) EnsureActor(key string, r *Record) (Actor, error) {
	a, ok := t.Actor(key)
	changed := false
	if !ok {
		seq, err := t.tx.Bucket(bActorIDs).NextSequence()
		if err != nil {
			return a, err
		}
		if seq > 1<<32-1 {
			return a, errors.New("analytics: actor numbers exhausted")
		}
		a = Actor{ID: uint32(seq)}
		if err := t.tx.Bucket(bActorIDs).Put(idKey(a.ID), []byte(key)); err != nil {
			return a, err
		}
		changed = true
	}
	if r != nil {
		day := r.Day()
		// The server's own events say nothing of the person's device.
		pf, app := r.PF, r.App
		if pf == "server" {
			pf = ""
		}
		if app == "server" {
			app = ""
		}
		if a.FirstDay == "" || day < a.FirstDay {
			a.FirstDay = day
			if pf != "" {
				a.FirstPF = pf
			}
			if app != "" {
				a.FirstApp = app
			}
			if r.CC != "" {
				a.FirstCC = r.CC
			}
			changed = true
		}
		if a.FirstPF == "" && pf != "" {
			a.FirstPF, changed = pf, true
		}
		if a.FirstApp == "" && app != "" {
			a.FirstApp, changed = app, true
		}
		if a.FirstCC == "" && r.CC != "" {
			a.FirstCC, changed = r.CC, true
		}
		if r.E == EventFirstOpen && a.FirstSource == "" {
			if src := sourceOf(r); src != "" {
				a.FirstSource, a.FirstCampaign, changed = src, str(r.DM["campaign"]), true
			}
		}
	}
	if changed {
		return a, t.PutActor(key, a)
	}
	return a, nil
}

// sourceOf is the install source of a $first_open: dm.source, else the store.
func sourceOf(r *Record) string {
	if s := str(r.DM["source"]); s != "" {
		return s
	}
	return str(r.DM["store"])
}

// mergeActor gives a person what its device saw first: the earlier first day with its platform,
// and the install source when the person has none.
func (t *Tx) mergeActor(device, person string) error {
	d, ok := t.Actor(device)
	if !ok {
		return nil
	}
	u, ok := t.Actor(person)
	if !ok {
		return nil
	}
	changed := false
	if d.FirstDay != "" && (u.FirstDay == "" || d.FirstDay <= u.FirstDay) {
		u.FirstDay = d.FirstDay
		for _, f := range [][2]*string{{&u.FirstPF, &d.FirstPF}, {&u.FirstApp, &d.FirstApp}, {&u.FirstCC, &d.FirstCC}} {
			if *f[1] != "" {
				*f[0] = *f[1]
			}
		}
		changed = true
	}
	if u.FirstSource == "" && d.FirstSource != "" {
		u.FirstSource, u.FirstCampaign = d.FirstSource, d.FirstCampaign
		changed = true
	}
	if changed {
		return t.PutActor(person, u)
	}
	return nil
}

// ─── Links ───────────────────────────────────────────────────────────────────

// Link returns the user a device is linked to.
func (t *Tx) Link(device string) string {
	if device == "" {
		return ""
	}
	return string(t.tx.Bucket(bLinks).Get([]byte(device)))
}

// SetLink links a device to a user unless it is linked already (the first link wins). It
// reports whether a new link was made.
func (t *Tx) SetLink(device, user string) (bool, error) {
	if device == "" || user == "" || t.Link(device) != "" {
		return false, nil
	}
	return true, t.tx.Bucket(bLinks).Put([]byte(device), []byte(user))
}

// ActorKey is the actor an event counts for: its user, else the user its device is linked to,
// else the device.
func (t *Tx) ActorKey(r *Record) string {
	if u := r.User(); u != "" {
		return "u:" + u
	}
	if u := t.Link(r.D); u != "" {
		return "u:" + u
	}
	if r.D != "" {
		return "d:" + r.D
	}
	return ""
}

// ─── Persons ─────────────────────────────────────────────────────────────────

// Person is what is known of a user: properties from set/once, first and last event, devices.
type Person struct {
	Props   map[string]any `json:"props,omitempty"`
	FirstTS string         `json:"first_ts,omitempty"`
	LastTS  string         `json:"last_ts,omitempty"`
	Devices []string       `json:"devices,omitempty"`
}

const (
	maxPersonProps   = 2 << 10
	maxPersonDevices = 20
)

func (t *Tx) Person(user string) (Person, bool) { return getJSON[Person](t.tx, bPersons, user) }

func (t *Tx) PutPerson(user string, p Person) error { return putJSON(t.tx, bPersons, user, p) }

// EachPerson calls fn for every person.
func (t *Tx) EachPerson(fn func(user string, p Person)) {
	t.tx.Bucket(bPersons).ForEach(func(k, v []byte) error {
		var p Person
		if json.Unmarshal(v, &p) == nil {
			fn(string(k), p)
		}
		return nil
	})
}

// DeletePerson removes a person and its device links (account_deleted). The actor numbers stay:
// rollups refer to them, and a number tells nothing about the person.
func (t *Tx) DeletePerson(user string) error {
	if err := t.tx.Bucket(bPersons).Delete([]byte(user)); err != nil {
		return err
	}
	var devices [][]byte
	t.tx.Bucket(bLinks).ForEach(func(k, v []byte) error {
		if string(v) == user {
			devices = append(devices, append([]byte(nil), k...))
		}
		return nil
	})
	for _, d := range devices {
		if err := t.tx.Bucket(bLinks).Delete(d); err != nil {
			return err
		}
	}
	return nil
}

// apply records an event of the person.
func (p *Person) apply(r *Record) {
	ts := r.Time().Format(time.RFC3339Nano)
	if p.FirstTS == "" || ts < p.FirstTS {
		p.FirstTS = ts
	}
	if ts > p.LastTS {
		p.LastTS = ts
	}
	if p.Props == nil && (len(r.Once) > 0 || len(r.Set) > 0) {
		p.Props = map[string]any{}
	}
	for k, v := range r.Once {
		if _, ok := p.Props[k]; !ok {
			p.Props[k] = v
		}
	}
	for k, v := range r.Set {
		p.Props[k] = v
	}
	for len(p.Props) > 0 {
		b, _ := json.Marshal(p.Props)
		if len(b) <= maxPersonProps {
			break
		}
		for k := range p.Props { // over the limit: drop keys until it fits
			delete(p.Props, k)
			break
		}
	}
	if r.D != "" {
		for i, d := range p.Devices {
			if d == r.D {
				p.Devices = append(p.Devices[:i], p.Devices[i+1:]...)
				break
			}
		}
		p.Devices = append(p.Devices, r.D)
		if len(p.Devices) > maxPersonDevices {
			p.Devices = p.Devices[len(p.Devices)-maxPersonDevices:]
		}
	}
}

// matches reports whether the person has every property of rule.
func (p Person) matches(rule map[string]any) bool {
	if len(rule) == 0 {
		return false
	}
	for k, want := range rule {
		got, ok := p.Props[k]
		if !ok || str(got) != str(want) {
			return false
		}
	}
	return true
}

// ─── Install keys ────────────────────────────────────────────────────────────

// InstallKey counts the installs seen with one install key.
type InstallKey struct {
	N        int      `json:"n"`
	FirstDay string   `json:"first_day"`
	LastDay  string   `json:"last_day"`
	FirstID  string   `json:"first_id"`
	IDs      []string `json:"ids,omitempty"` // event ids of the installs, the last maxInstallIDs
}

const maxInstallIDs = 50

func (t *Tx) InstallKey(ik string) (InstallKey, bool) { return getJSON[InstallKey](t.tx, bInstall, ik) }

// Install kinds of a $first_open.
const (
	KindInstall    = "install"
	KindReinstall  = "reinstall"
	KindFirstVisit = "first_visit"
)

// recordInstall adds a $first_open to its install key and returns its kind and its number among
// the installs of the key (1 for the first).
func (t *Tx) recordInstall(r *Record) (string, int, error) {
	if r.IK == "" {
		return KindFirstVisit, 1, nil
	}
	k, ok := t.InstallKey(r.IK)
	day := r.Day()
	if !ok {
		k = InstallKey{N: 1, FirstDay: day, LastDay: day, FirstID: r.ID, IDs: []string{r.ID}}
		if err := putJSON(t.tx, bInstall, r.IK, k); err != nil {
			return "", 0, err
		}
	} else if idx := indexOf(k.IDs, r.ID); idx < 0 && r.ID != k.FirstID {
		k.N++
		if day > k.LastDay {
			k.LastDay = day
		}
		if day < k.FirstDay {
			k.FirstDay = day
		}
		k.IDs = append(k.IDs, r.ID)
		if len(k.IDs) > maxInstallIDs {
			k.IDs = k.IDs[len(k.IDs)-maxInstallIDs:]
		}
		if err := putJSON(t.tx, bInstall, r.IK, k); err != nil {
			return "", 0, err
		}
	}
	kind, n := t.installKind(r)
	return kind, n, nil
}

// installKind decides what a $first_open is, from the index: the first install seen with its key
// is an install, a later one (or one with the client's reinstall hint) a reinstall. It does not
// change the index, so a recount gives the same answer.
func (t *Tx) installKind(r *Record) (string, int) {
	if r.IK == "" {
		return KindFirstVisit, 1
	}
	k, ok := t.InstallKey(r.IK)
	if !ok {
		if r.reinstallHint() {
			return KindReinstall, 2
		}
		return KindInstall, 1
	}
	n := k.N
	if idx := indexOf(k.IDs, r.ID); idx >= 0 {
		n = k.N - (len(k.IDs) - 1 - idx)
	}
	if r.ID == k.FirstID {
		if r.reinstallHint() {
			return KindReinstall, max(n, 2)
		}
		return KindInstall, 1
	}
	return KindReinstall, max(n, 2)
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

// ─── Groups ──────────────────────────────────────────────────────────────────

// GroupID numbers a group for the groups bitmaps.
func (t *Tx) GroupID(typ, key string) (uint32, error) {
	k := []byte(typ + "\x00" + key)
	b := t.tx.Bucket(bGroups)
	if v := b.Get(k); v != nil {
		return binary.BigEndian.Uint32(v), nil
	}
	seq, err := b.NextSequence()
	if err != nil {
		return 0, err
	}
	return uint32(seq), b.Put(k, idKey(uint32(seq)))
}

// ─── Meta ────────────────────────────────────────────────────────────────────

// Meta reads a meta value into v; false when it is not set.
func (t *Tx) Meta(name string, v any) bool {
	b := t.tx.Bucket(bMeta).Get([]byte(name))
	if b == nil {
		return false
	}
	return json.Unmarshal(b, v) == nil
}

// PutMeta stores a meta value.
func (t *Tx) PutMeta(name string, v any) error { return putJSON(t.tx, bMeta, name, v) }

// GetMeta reads a meta value outside a transaction.
func (s *State) GetMeta(name string, v any) bool {
	var ok bool
	s.View(func(t *Tx) error { ok = t.Meta(name, v); return nil })
	return ok
}

// SetMeta writes a meta value outside a transaction.
func (s *State) SetMeta(name string, v any) error {
	return s.Update(func(t *Tx) error { return t.PutMeta(name, v) })
}

// ─── Snapshot ────────────────────────────────────────────────────────────────

// snapshotLine is one line of a state snapshot: a bucket, a key and a value.
type snapshotLine struct {
	B string          `json:"b"`
	K string          `json:"k"`
	V json.RawMessage `json:"v,omitempty"`
	N uint32          `json:"n,omitempty"` // actor numbers: actor_ids values are keys, not JSON
}

// snapshotBuckets go into the weekly snapshot; meta (config, secrets) does not.
var snapshotBuckets = [][]byte{bActors, bPersons, bLinks, bInstall}

// WriteSnapshot writes actors, persons, links and install keys as NDJSON.
func (s *State) WriteSnapshot(w io.Writer) error {
	enc := json.NewEncoder(w)
	return s.db.View(func(tx *bolt.Tx) error {
		for _, name := range snapshotBuckets {
			err := tx.Bucket(name).ForEach(func(k, v []byte) error {
				l := snapshotLine{B: string(name), K: string(k)}
				if string(name) == string(bLinks) {
					l.V, _ = json.Marshal(string(v))
				} else {
					l.V = json.RawMessage(v)
				}
				return enc.Encode(l)
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// ReadSnapshot loads a snapshot into an empty state.
func (s *State) ReadSnapshot(r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	return s.db.Update(func(tx *bolt.Tx) error {
		var maxID uint32
		for sc.Scan() {
			var l snapshotLine
			if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
				return err
			}
			switch l.B {
			case string(bLinks):
				var u string
				json.Unmarshal(l.V, &u)
				if err := tx.Bucket(bLinks).Put([]byte(l.K), []byte(u)); err != nil {
					return err
				}
			case string(bActors):
				var a Actor
				if err := json.Unmarshal(l.V, &a); err != nil {
					return err
				}
				if err := tx.Bucket(bActors).Put([]byte(l.K), l.V); err != nil {
					return err
				}
				if err := tx.Bucket(bActorIDs).Put(idKey(a.ID), []byte(l.K)); err != nil {
					return err
				}
				maxID = max(maxID, a.ID)
			case string(bPersons), string(bInstall):
				if err := tx.Bucket([]byte(l.B)).Put([]byte(l.K), l.V); err != nil {
					return err
				}
			}
		}
		if err := sc.Err(); err != nil {
			return err
		}
		return tx.Bucket(bActorIDs).SetSequence(uint64(maxID))
	})
}
