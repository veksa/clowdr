// Package notify sends alerts. Telegram is the only channel for now.
package notify

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"
)

type Telegram struct {
	Token  string
	ChatID string
	Log    *slog.Logger
	// Dedup drops identical messages sent within this window (default 10 minutes).
	Dedup time.Duration

	mu   sync.Mutex
	last map[string]time.Time
	hc   http.Client
}

// Send posts text asynchronously; failures are logged, never returned to the caller.
func (t *Telegram) Send(ctx context.Context, text string) {
	if !t.allow(text) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		form := url.Values{"chat_id": {t.ChatID}, "text": {text}, "disable_web_page_preview": {"true"}}
		req, err := http.NewRequestWithContext(ctx, "POST", "https://api.telegram.org/bot"+t.Token+"/sendMessage", nil)
		if err != nil {
			return
		}
		req.URL.RawQuery = form.Encode()
		resp, err := t.hc.Do(req)
		if err != nil {
			t.Log.Warn("telegram", "err", err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Log.Warn("telegram", "status", resp.StatusCode)
		}
	}()
}

func (t *Telegram) allow(text string) bool {
	window := t.Dedup
	if window == 0 {
		window = 10 * time.Minute
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last == nil {
		t.last = map[string]time.Time{}
	}
	now := time.Now()
	if at, ok := t.last[text]; ok && now.Sub(at) < window {
		return false
	}
	for k, at := range t.last {
		if now.Sub(at) > window {
			delete(t.last, k)
		}
	}
	t.last[text] = now
	return true
}
