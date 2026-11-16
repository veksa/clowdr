// Package notify sends alerts. Telegram is the only channel for now.
package notify

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Telegram struct {
	Token  string
	ChatID string
	// API is the Bot API base URL (default https://api.telegram.org).
	API string
	// ParseMode is Telegram's parse_mode ("HTML"); empty sends plain text.
	ParseMode string
	Log       *slog.Logger
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
		if err := t.post(ctx, text); err != nil && t.Log != nil {
			t.Log.Warn("telegram", "err", err)
		}
	}()
}

// Deliver posts text and waits for Telegram's answer, so the caller can retry a failure. It
// skips the duplicate check: a caller that retries wants the message to go.
func (t *Telegram) Deliver(ctx context.Context, text string) error { return t.post(ctx, text) }

func (t *Telegram) post(ctx context.Context, text string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	api := t.API
	if api == "" {
		api = "https://api.telegram.org"
	}
	form := url.Values{"chat_id": {t.ChatID}, "text": {text}, "disable_web_page_preview": {"true"}}
	if t.ParseMode != "" {
		form.Set("parse_mode", t.ParseMode)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", api+"/bot"+t.Token+"/sendMessage", nil)
	if err != nil {
		return err
	}
	req.URL.RawQuery = form.Encode()
	resp, err := t.hc.Do(req)
	if err != nil {
		// The URL carries the bot token: keep it out of the error.
		if ue, ok := err.(*url.Error); ok {
			return fmt.Errorf("telegram: %v", ue.Err)
		}
		return fmt.Errorf("telegram: request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("telegram: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
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
