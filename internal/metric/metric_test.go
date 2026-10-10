package metric

import (
	"math"
	"testing"
	"time"
)

func pt(line string, service string, replica int) Point {
	p, ok := Parse(line)
	if !ok {
		panic(line)
	}
	p.Service, p.Replica = service, replica
	return p
}

func TestCounterAndGauge(t *testing.T) {
	a := &Aggregator{Q: Query{Name: "runs", Step: time.Hour, By: []string{"outcome"}}}
	for _, p := range []Point{
		pt(`{"$":"metric","ts":"2026-01-02T10:01:00Z","name":"runs","kind":"counter","value":3,"labels":{"outcome":"done"}}`, "server", 1),
		pt(`{"$":"metric","ts":"2026-01-02T10:01:00Z","name":"runs","kind":"counter","value":2,"labels":{"outcome":"done"}}`, "server", 2),
		pt(`{"$":"metric","ts":"2026-01-02T10:59:00Z","name":"runs","kind":"counter","value":1,"labels":{"outcome":"failed"}}`, "server", 1),
		pt(`{"$":"metric","ts":"2026-01-02T11:00:00Z","name":"runs","kind":"counter","value":4,"labels":{"outcome":"done"}}`, "server", 1),
		pt(`{"$":"metric","ts":"2026-01-02T10:00:00Z","name":"other","kind":"counter","value":9}`, "server", 1),
	} {
		a.Add(p)
	}
	rs := a.Result()
	if len(rs) != 2 || rs[0].Labels["outcome"] != "done" || len(rs[0].Points) != 2 || rs[0].Points[0].V != 5 || rs[0].Points[1].V != 4 || rs[1].Points[0].V != 1 {
		t.Fatalf("counter: %+v", rs)
	}
	a.Q.Agg = "rate"
	if v := a.Result()[0].Points[0].V; math.Abs(v-5.0/3600) > 1e-9 {
		t.Fatalf("rate: %v", v)
	}

	// Two workers report the same queue depth: max by default, the last point of each series.
	g := &Aggregator{Q: Query{Name: "queue", Step: time.Hour}}
	for _, p := range []Point{
		pt(`{"$":"metric","ts":"2026-01-02T10:01:00Z","name":"queue","kind":"gauge","value":7}`, "server", 1),
		pt(`{"$":"metric","ts":"2026-01-02T10:30:00Z","name":"queue","kind":"gauge","value":3}`, "server", 1),
		pt(`{"$":"metric","ts":"2026-01-02T10:30:00Z","name":"queue","kind":"gauge","value":4}`, "server", 2),
	} {
		g.Add(p)
	}
	if v := g.Result()[0].Points[0].V; v != 4 {
		t.Fatalf("gauge max of last values: %v", v)
	}
	g.Q.Agg = "sum"
	if v := g.Result()[0].Points[0].V; v != 7 {
		t.Fatalf("gauge sum: %v", v)
	}
}

func TestHistogram(t *testing.T) {
	a := &Aggregator{Q: Query{Name: "dur", Step: time.Hour}}
	// 100 observations: 50 in (0,1], 40 in (1,2], 10 in (2,5].
	a.Add(pt(`{"$":"metric","ts":"2026-01-02T10:01:00Z","name":"dur","kind":"histogram","count":60,"sum":60,"min":0.1,"max":4,"bounds":[1,2,5],"counts":[30,25,5,0]}`, "server", 1))
	a.Add(pt(`{"$":"metric","ts":"2026-01-02T10:02:00Z","name":"dur","kind":"histogram","count":40,"sum":50,"min":0.2,"max":4.5,"bounds":[1,2,5],"counts":[20,15,5,0]}`, "server", 2))
	get := func(agg string) float64 {
		a.Q.Agg = agg
		return a.Result()[0].Points[0].V
	}
	if v := get("count"); v != 100 {
		t.Fatalf("count %v", v)
	}
	if v := get("avg"); v != 1.1 {
		t.Fatalf("avg %v", v)
	}
	if v := get("p50"); v != 1 {
		t.Fatalf("p50 %v", v)
	}
	if v := get("p95"); v <= 2 || v > 4.5 {
		t.Fatalf("p95 %v", v)
	}
	if v := get("max"); v != 4.5 {
		t.Fatalf("max %v", v)
	}
}

func TestLister(t *testing.T) {
	var l Lister
	l.Add(pt(`{"$":"metric","ts":"2026-01-02T10:01:00Z","name":"runs","kind":"counter","value":3,"labels":{"outcome":"done"}}`, "server", 1))
	l.Add(pt(`{"$":"metric","ts":"2026-01-02T10:02:00Z","name":"runs","kind":"counter","value":1,"labels":{"outcome":"failed"}}`, "server", 1))
	l.Add(pt(`{"$":"metric","ts":"2026-01-02T10:02:00Z","name":"queue","kind":"gauge","value":5}`, "server", 1))
	r := l.Result()
	if len(r) != 2 || r[1].Name != "runs" || r[1].Series != 2 || len(r[1].Labels["outcome"]) != 2 || r[0].Last != 5 {
		t.Fatalf("list: %+v", r)
	}
	if _, ok := Parse(`{"$":"metric","name":"x","kind":"weird"}`); ok {
		t.Fatal("unknown kind parsed")
	}
}
