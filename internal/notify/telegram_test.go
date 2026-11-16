package notify

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTelegram(t *testing.T) {
	got := make(chan string, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botT0KEN/sendMessage" || r.URL.Query().Get("chat_id") != "42" {
			w.WriteHeader(400)
		}
		got <- r.URL.Query().Get("text")
	}))
	defer srv.Close()
	tg := &Telegram{Token: "T0KEN", ChatID: "42", API: srv.URL, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()

	tg.Send(ctx, "shop/web crashed")
	tg.Send(ctx, "shop/web crashed") // a duplicate within the window is dropped
	tg.Send(ctx, "shop/web OOM")
	want := map[string]bool{"shop/web crashed": true, "shop/web OOM": true}
	for range 2 {
		select {
		case text := <-got:
			if !want[text] {
				t.Fatalf("unexpected %q", text)
			}
			delete(want, text)
		case <-time.After(5 * time.Second):
			t.Fatal("no message")
		}
	}
	select {
	case text := <-got:
		t.Fatalf("duplicate sent: %q", text)
	case <-time.After(100 * time.Millisecond):
	}

	// After the window the same text goes out again.
	tg.Dedup = time.Millisecond
	time.Sleep(5 * time.Millisecond)
	tg.Send(ctx, "shop/web crashed")
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("not resent after the window")
	}

	// Failures are logged, not returned or retried.
	tg.API = "http://127.0.0.1:1"
	tg.Send(ctx, "unreachable")
}

func TestTelegramDeliverHTML(t *testing.T) {
	var mode, text string
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mode, text = r.URL.Query().Get("parse_mode"), r.URL.Query().Get("text")
		w.WriteHeader(status)
		w.Write([]byte(`{"ok":false,"description":"chat not found"}`))
	}))
	defer srv.Close()
	tg := &Telegram{Token: "T0KEN", ChatID: "42", API: srv.URL, ParseMode: "HTML"}
	if err := tg.Deliver(context.Background(), "<b>digest</b>"); err != nil {
		t.Fatal(err)
	}
	if mode != "HTML" || text != "<b>digest</b>" {
		t.Fatalf("parse_mode %q text %q", mode, text)
	}
	// Deliver reports a refusal, so the caller can try again later.
	status = 400
	err := tg.Deliver(context.Background(), "again")
	if err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("refusal not reported: %v", err)
	}
	// The bot token is in the URL: a network error must not carry it.
	tg.API = "http://127.0.0.1:1"
	if err := tg.Deliver(context.Background(), "x"); err == nil || strings.Contains(err.Error(), "T0KEN") {
		t.Fatalf("network error leaks the token or is missing: %v", err)
	}
}
