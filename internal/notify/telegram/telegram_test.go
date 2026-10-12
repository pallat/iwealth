package telegram

import (
	"context"
	"encoding/json"
	"errors"
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

func TestPollReportsErrorsRateLimited(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 4 {
			clock = clock.Add(61 * time.Second) // the fourth identical failure comes a minute later
		}
		if calls >= 6 {
			cancel()
		}
		if calls <= 4 {
			w.Write([]byte(`{"ok":false,"description":"rate limited"}`))
			return
		}
		w.Write([]byte(`{"ok":true,"result":[]}`))
	}))
	defer srv.Close()
	b := &Bot{Token: "SECRET", ChatID: 1, BaseURL: srv.URL, HTTP: srv.Client()}
	b.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	b.backoff = time.Millisecond
	b.idle = time.Millisecond
	var errs []error
	b.OnError = func(err error) { errs = append(errs, err) }
	if err := b.Poll(ctx, func(context.Context, string) string { return "" }); err != nil {
		t.Fatal(err)
	}
	// 4 identical failures: reported at the first, silent for 2 and 3, reported again after 60s.
	if len(errs) != 2 {
		t.Fatalf("want 2 reports, got %d: %v", len(errs), errs)
	}
	for _, e := range errs {
		if !strings.Contains(e.Error(), "rate limited") || strings.Contains(e.Error(), "SECRET") {
			t.Fatalf("bad reported error: %v", e)
		}
	}
}

func TestParseDeduct(t *testing.T) {
	k, a, err := ParseDeduct("/deduct@MyBot PVD 60000")
	if err != nil || k != "pvd" || a != 60000 {
		t.Fatalf("%v %v %v", k, a, err)
	}
	if _, a, err := ParseDeduct("/deduct pvd 0"); err != nil || a != 0 {
		t.Errorf("zero must be allowed: %v %v", a, err)
	}
	for _, in := range []string{"/deduct", "/deduct pvd", "/deduct pvd -1", "/deduct pvd x", "/deduct pvd NaN", "/paid pvd 5", "/deduct a b c"} {
		if _, _, err := ParseDeduct(in); err == nil {
			t.Errorf("%q should fail", in)
		}
	}
}

func TestTimeoutMarginsCoverLongPoll(t *testing.T) {
	if margin := pollTimeout - pollSeconds*time.Second; margin < 60*time.Second {
		t.Errorf("long-poll margin %s is too thin for a slow link", margin)
	}
	if clientTimeout <= pollTimeout {
		t.Errorf("client timeout %s must exceed the poll timeout %s", clientTimeout, pollTimeout)
	}
	if got := New("t", 1).HTTP.Timeout; got != clientTimeout {
		t.Errorf("New client timeout = %s", got)
	}
	if got := (&Bot{}).client().Timeout; got != clientTimeout {
		t.Errorf("fallback client timeout = %s", got)
	}
}

func TestWithTimeoutNamesTheTimeout(t *testing.T) {
	var err error
	ctx, done := withTimeout(context.Background(), 10*time.Millisecond, "getUpdates", &err)
	<-ctx.Done()
	err = ctx.Err()
	done()
	if err == nil || !strings.Contains(err.Error(), "getUpdates: timed out after 10ms") {
		t.Fatalf("err = %v", err)
	}

	// the caller's own cancellation is passed through untouched
	parent, cancel := context.WithCancel(context.Background())
	var err2 error
	ctx2, done2 := withTimeout(parent, time.Minute, "sendMessage", &err2)
	cancel()
	<-ctx2.Done()
	err2 = ctx2.Err()
	done2()
	if !errors.Is(err2, context.Canceled) {
		t.Fatalf("err2 = %v", err2)
	}
}
