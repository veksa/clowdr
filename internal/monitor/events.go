// Package monitor watches what runs: container crashes, OOM, restart loops, failing health
// checks, 5xx rates, disk and memory. It records events and sends alerts.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/veksa/clowdr/internal/logs"
)

type Severity string

const (
	Info  Severity = "info"
	Warn  Severity = "warn"
	Error Severity = "error"
)

// Event is one entry of `clowctl events`.
type Event struct {
	Time     time.Time `json:"time"`
	Project  string    `json:"project"`
	Service  string    `json:"service,omitempty"`
	Kind     string    `json:"kind"` // deploy, crash, oom, restart-loop, health, recovered, 5xx, disk, memory, nginx
	Severity Severity  `json:"severity"`
	Message  string    `json:"message"`
}

// Events records events in the log store (stream <project>/events/_) and alerts on them.
type Events struct {
	Logs *logs.Store
	// Alert sends a message (Telegram). Called for warn/error events and deploys.
	Alert func(ctx context.Context, text string)
	// Dedup suppresses repeated alerts with the same key (default 10 minutes).
	Dedup time.Duration

	mu   sync.Mutex
	last map[string]time.Time
}

const KindEvents = "events"

func eventsKey(project string) logs.Key {
	return logs.Key{Project: project, Kind: KindEvents, Name: "_"}
}

// Emit records an event; extra (e.g. last log lines) only goes to the alert.
func (e *Events) Emit(ctx context.Context, ev Event, extra string) {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	if ev.Project == "" {
		ev.Project = logs.Platform
	}
	if e.Logs != nil {
		b, _ := json.Marshal(ev)
		e.Logs.Append(eventsKey(ev.Project), logs.Line{T: ev.Time.UnixNano(), L: string(b), E: ev.Severity != Info})
	}
	if e.Alert == nil || (ev.Severity == Info && ev.Kind != "deploy" && ev.Kind != "recovered") {
		return
	}
	if !e.allow(ev.Project + "|" + ev.Service + "|" + ev.Kind + "|" + ev.Message) {
		return
	}
	icon := map[Severity]string{Info: "✅", Warn: "⚠️", Error: "🔥"}[ev.Severity]
	where := ev.Project
	if ev.Service != "" {
		where += "/" + ev.Service
	}
	text := fmt.Sprintf("%s %s: %s", icon, where, ev.Message)
	if extra != "" {
		text += "\n\n" + truncate(extra, 3000)
	}
	e.Alert(ctx, text)
}

func (e *Events) allow(key string) bool {
	window := e.Dedup
	if window == 0 {
		window = 10 * time.Minute
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.last == nil {
		e.last = map[string]time.Time{}
	}
	now := time.Now()
	if t, ok := e.last[key]; ok && now.Sub(t) < window {
		return false
	}
	e.last[key] = now
	for k, t := range e.last {
		if now.Sub(t) > window {
			delete(e.last, k)
		}
	}
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// Read returns events of a project in time order.
func (e *Events) Read(project string, q logs.Query, fn func(Event)) error {
	return e.Logs.Read(eventsKey(project), q, func(l logs.Line) {
		var ev Event
		if json.Unmarshal([]byte(l.L), &ev) == nil {
			fn(ev)
		}
	})
}

// Subscribe streams new events of a project.
func (e *Events) Subscribe(project string) (<-chan Event, func(), error) {
	ch, cancel, err := e.Logs.Subscribe(eventsKey(project))
	if err != nil {
		return nil, nil, err
	}
	out := make(chan Event, 64)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case l := <-ch:
				var ev Event
				if json.Unmarshal([]byte(l.L), &ev) == nil {
					select {
					case out <- ev:
					default:
					}
				}
			}
		}
	}()
	return out, func() { cancel(); close(done) }, nil
}
