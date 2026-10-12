package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParsePaid(t *testing.T) {
	f, a, err := ParsePaid("/paid ABC-RMF 15000.50")
	if err != nil || f != "ABC-RMF" || a != 15000.50 {
		t.Fatalf("%v %v %v", f, a, err)
	}
	if f, _, err := ParsePaid("/paid@MyBot X 5"); err != nil || f != "X" {
		t.Fatalf("bot suffix: %v %v", f, err)
	}
	for _, bad := range []string{"/paid", "/paid A", "/paid A x", "/paid A -5", "/paid A 0", "/paid A 1 2", "/other A 5", "", "/paid A NaN", "/paid A Inf", "/paid A +Inf", "/paidx A 5"} {
		if _, _, err := ParsePaid(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestSendSplitsLongMessages(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n++; w.Write([]byte(`{"ok":true}`)) }))
	defer srv.Close()
	b := &Bot{Token: "T", ChatID: 1, BaseURL: srv.URL, HTTP: srv.Client()}
	if err := b.Send(context.Background(), strings.Repeat("ก", 9000)); err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestSendNotOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":false,"description":"chat not found"}`))
	}))
	defer srv.Close()
	b := &Bot{Token: "SECRET", ChatID: 1, BaseURL: srv.URL, HTTP: srv.Client()}
	err := b.Send(context.Background(), "hi")
	if err == nil || !strings.Contains(err.Error(), "chat not found") || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("err=%v", err)
	}
}

func TestSendErrorHidesToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	b := &Bot{Token: "SECRET123", ChatID: 1, BaseURL: url, HTTP: &http.Client{Timeout: time.Second}}
	err := b.Send(context.Background(), "hi")
	if err == nil || strings.Contains(err.Error(), "SECRET123") {
		t.Fatalf("err=%v", err)
	}
}

func TestPollIgnoresOtherChats(t *testing.T) {
	var mu sync.Mutex
	var calls int
	var sent []map[string]any
	var offsets []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			calls++
			offsets = append(offsets, r.URL.Query().Get("offset"))
			if calls == 1 {
				w.Write([]byte(`{"ok":true,"result":[
{"update_id":10,"message":{"chat":{"id":999},"text":"evil"}},
{"update_id":11,"message":{"chat":{"id":1},"text":"hello"}}]}`))
				return
			}
			w.Write([]byte(`{"ok":true,"result":[]}`))
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			body, _ := io.ReadAll(r.Body)
			var m map[string]any
			json.Unmarshal(body, &m)
			sent = append(sent, m)
			w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()
	b := &Bot{Token: "T", ChatID: 1, BaseURL: srv.URL, HTTP: srv.Client()}
	b.idle = 5 * time.Millisecond
	b.backoff = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	var got []string
	done := make(chan error)
	go func() {
		done <- b.Poll(ctx, func(_ context.Context, text string) string {
			got = append(got, text)
			if text == "boom" {
				panic("x")
			}
			return "reply:" + text
		})
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n, c := len(sent), calls
		mu.Unlock()
		if n >= 1 && c >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "hello" {
		t.Fatalf("got=%v", got)
	}
	if len(sent) != 1 || sent[0]["text"] != "reply:hello" || sent[0]["chat_id"] != float64(1) {
		t.Fatalf("sent=%v", sent)
	}
	if len(offsets) < 2 || offsets[1] != "12" {
		t.Fatalf("offsets=%v", offsets)
	}
}

func TestPollSurvivesPanicAndErrors(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			calls++
			switch calls {
			case 1:
				w.Write([]byte(`{"ok":false,"description":"boom"}`))
			case 2:
				w.Write([]byte(`{"ok":true,"result":[{"update_id":1,"message":{"chat":{"id":1},"text":"panic"}},{"update_id":2,"message":{"chat":{"id":1},"text":"ok"}}]}`))
			default:
				w.Write([]byte(`{"ok":true,"result":[]}`))
			}
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	b := &Bot{Token: "T", ChatID: 1, BaseURL: srv.URL, HTTP: srv.Client()}
	b.idle = 5 * time.Millisecond
	b.backoff = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var got []string
	err := b.Poll(ctx, func(_ context.Context, text string) string {
		got = append(got, text)
		if text == "panic" {
			panic("x")
		}
		if text == "ok" {
			cancel()
		}
		return ""
	})
	if err != nil || len(got) != 2 {
		t.Fatalf("err=%v got=%v", err, got)
	}
}
