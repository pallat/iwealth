package sec

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fake struct {
	srv                   *httptest.Server
	searches, navs        atomic.Int32
	navStatus, navBody    int
	navOverride           string
	lastKey, lastNavQuery string
}

func newFake(t *testing.T) (*fake, *Client) {
	t.Helper()
	f := &fake{}
	funds, _ := os.ReadFile("testdata/funds.json")
	nav, _ := os.ReadFile("testdata/nav.json")
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastKey = r.Header.Get("Ocp-Apim-Subscription-Key")
		if f.lastKey != "k" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v2/fund/general-info/profiles":
			f.searches.Add(1)
			w.Write(funds)
		case "/v2/fund/daily-info/nav":
			f.navs.Add(1)
			f.lastNavQuery = r.URL.RawQuery
			if f.navStatus != 0 {
				w.WriteHeader(f.navStatus)
				return
			}
			if f.navOverride != "" {
				w.Write([]byte(f.navOverride))
				return
			}
			w.Write(nav)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f, &Client{BaseURL: f.srv.URL, Key: "k", HTTP: f.srv.Client()}
}

var d = time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)

func TestDailyNAVPublished(t *testing.T) {
	f, c := newFake(t)
	nav, ok, err := c.DailyNAV(context.Background(), "example-rmf", d)
	if err != nil || !ok || nav != 12.3456 {
		t.Fatalf("%v %v %v", nav, ok, err)
	}
	if f.lastKey != "k" {
		t.Fatalf("key header not sent")
	}
	for _, want := range []string{"proj_id=M0001_2550", "start_nav_date=2026-10-09", "end_nav_date=2026-10-09"} {
		if !strings.Contains(f.lastNavQuery, want) {
			t.Fatalf("query %q missing %q", f.lastNavQuery, want)
		}
	}
}

func TestDailyNAVNoData(t *testing.T) {
	for name, tc := range map[string]struct {
		status   int
		override string
	}{
		"204":        {status: 204},
		"empty":      {override: `{"items":[],"next_cursor":""}`},
		"other date": {override: `{"items":[{"proj_id":"M0001_2550","nav_date":"2026-10-08","last_val":12.1}]}`},
	} {
		t.Run(name, func(t *testing.T) {
			f, c := newFake(t)
			f.navStatus, f.navOverride = tc.status, tc.override
			nav, ok, err := c.DailyNAV(context.Background(), "EXAMPLE-RMF", d)
			if err != nil || ok || nav != 0 {
				t.Fatalf("%v %v %v", nav, ok, err)
			}
		})
	}
}

func TestDailyNAVDateFormats(t *testing.T) {
	for _, s := range []string{"2026-10-09T00:00:00+07:00", "2026-10-09T00:00:00.123", "2026-10-09"} {
		f, c := newFake(t)
		f.navOverride = `{"items":[{"nav_date":"` + s + `","last_val":9.5}]}`
		nav, ok, err := c.DailyNAV(context.Background(), "EXAMPLE-RMF", d)
		if err != nil || !ok || nav != 9.5 {
			t.Fatalf("%s: %v %v %v", s, nav, ok, err)
		}
	}
}

func TestDailyNAVErrors(t *testing.T) {
	t.Run("401", func(t *testing.T) {
		_, c := newFake(t)
		c.Key = "bad"
		if _, ok, err := c.DailyNAV(context.Background(), "EXAMPLE-RMF", d); err == nil || ok {
			t.Fatalf("want error, got ok=%v err=%v", ok, err)
		} else if strings.Contains(err.Error(), "bad") {
			t.Fatalf("error leaks key: %v", err)
		}
	})
	t.Run("500", func(t *testing.T) {
		f, c := newFake(t)
		f.navStatus = 500
		if _, ok, err := c.DailyNAV(context.Background(), "EXAMPLE-RMF", d); err == nil || ok {
			t.Fatalf("want error, got ok=%v err=%v", ok, err)
		}
	})
	t.Run("unknown fund", func(t *testing.T) {
		f, c := newFake(t)
		if _, ok, err := c.DailyNAV(context.Background(), "NOPE", d); err == nil || ok {
			t.Fatalf("want error, got ok=%v err=%v", ok, err)
		}
		if f.navs.Load() != 0 {
			t.Fatal("nav should not be queried for unknown fund")
		}
	})
	for _, v := range []string{"0", "-1.5"} {
		t.Run("nav "+v, func(t *testing.T) {
			f, c := newFake(t)
			f.navOverride = `{"items":[{"nav_date":"2026-10-09","last_val":` + v + `}]}`
			if _, ok, err := c.DailyNAV(context.Background(), "EXAMPLE-RMF", d); err == nil || ok {
				t.Fatalf("want error, got ok=%v err=%v", ok, err)
			}
		})
	}
}

func TestResolverCache(t *testing.T) {
	f, c := newFake(t)
	for i := 0; i < 2; i++ {
		if _, ok, err := c.DailyNAV(context.Background(), "EXAMPLE-RMF", d); err != nil || !ok {
			t.Fatalf("%v %v", ok, err)
		}
	}
	if f.searches.Load() != 1 || f.navs.Load() != 2 {
		t.Fatalf("searches=%d navs=%d", f.searches.Load(), f.navs.Load())
	}
}

func TestNewDefaults(t *testing.T) {
	c := New("k")
	if c.BaseURL != "https://api.sec.or.th" || c.Key != "k" || c.HTTP == nil {
		t.Fatalf("%+v", c)
	}
}
