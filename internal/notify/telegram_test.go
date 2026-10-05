package notify

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
