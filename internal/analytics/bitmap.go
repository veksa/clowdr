package analytics

import (
	"bytes"
	"encoding/base64"
	"encoding/json"

	"github.com/RoaringBitmap/roaring/v2"
)

// Bitmap is a set of actor (or group) ids, stored in rollups as a base64 roaring bitmap.
// A nil *Bitmap is an empty set for reading.
type Bitmap struct{ b *roaring.Bitmap }

func newBitmap() *Bitmap { return &Bitmap{b: roaring.New()} }

func (m *Bitmap) Add(x uint32) {
	if m.b == nil {
		m.b = roaring.New()
	}
	m.b.Add(x)
}

func (m *Bitmap) Contains(x uint32) bool { return m != nil && m.b != nil && m.b.Contains(x) }

func (m *Bitmap) Card() int {
	if m == nil || m.b == nil {
		return 0
	}
	return int(m.b.GetCardinality())
}

// Clone returns a copy that can be changed.
func (m *Bitmap) Clone() *Bitmap {
	if m == nil || m.b == nil {
		return newBitmap()
	}
	return &Bitmap{b: m.b.Clone()}
}

// Or adds every member of o.
func (m *Bitmap) Or(o *Bitmap) {
	if o == nil || o.b == nil {
		return
	}
	if m.b == nil {
		m.b = roaring.New()
	}
	m.b.Or(o.b)
}

// And keeps only members of o.
func (m *Bitmap) And(o *Bitmap) {
	if m.b == nil {
		return
	}
	if o == nil || o.b == nil {
		m.b.Clear()
		return
	}
	m.b.And(o.b)
}

// AndNot removes the members of o.
func (m *Bitmap) AndNot(o *Bitmap) {
	if m.b == nil || o == nil || o.b == nil {
		return
	}
	m.b.AndNot(o.b)
}

// Each calls fn for every member in order.
func (m *Bitmap) Each(fn func(uint32)) {
	if m == nil || m.b == nil {
		return
	}
	it := m.b.Iterator()
	for it.HasNext() {
		fn(it.Next())
	}
}

func or(ms ...*Bitmap) *Bitmap {
	out := newBitmap()
	for _, m := range ms {
		out.Or(m)
	}
	return out
}

func and(a, b *Bitmap) *Bitmap {
	out := a.Clone()
	out.And(b)
	return out
}

func (m *Bitmap) MarshalJSON() ([]byte, error) {
	if m == nil || m.b == nil {
		return []byte(`""`), nil
	}
	m.b.RunOptimize()
	raw, err := m.b.ToBytes()
	if err != nil {
		return nil, err
	}
	return json.Marshal(base64.StdEncoding.EncodeToString(raw))
}

func (m *Bitmap) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	m.b = roaring.New()
	if s == "" {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	// ReadFrom copies: FromBuffer would alias raw, unaligned.
	_, err = m.b.ReadFrom(bytes.NewReader(raw))
	return err
}
