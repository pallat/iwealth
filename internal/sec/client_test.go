package sec

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fake struct {
	srv                   *httptest.Server
	searches, navs        atomic.Int32
	navStatus, navBody    int
	navOverride           string
	mu                    sync.Mutex
	lastKey, lastNavQuery string
	navPages              map[string]string // next_cursor -> body
}

func (f *fake) key() string   { f.mu.Lock(); defer f.mu.Unlock(); return f.lastKey }
func (f *fake) query() string { f.mu.Lock(); defer f.mu.Unlock(); return f.lastNavQuery }

func newFake(t *testing.T) (*fake, *Client) {
	t.Helper()
	f := &fake{}
	funds, _ := os.ReadFile("testdata/funds.json")
	nav, _ := os.ReadFile("testdata/nav.json")
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Ocp-Apim-Subscription-Key")
		f.mu.Lock()
		f.lastKey = key
		f.mu.Unlock()
		if key != "k" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v2/fund/general-info/profiles":
			f.searches.Add(1)
			w.Write(funds)
		case "/v2/fund/daily-info/nav":
			f.navs.Add(1)
			f.mu.Lock()
			f.lastNavQuery = r.URL.RawQuery
			pg, paged := f.navPages[r.URL.Query().Get("next_cursor")]
			f.mu.Unlock()
			if paged {
				w.Write([]byte(pg))
				return
			}
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
	if f.key() != "k" {
		t.Fatalf("key header not sent")
	}
	for _, want := range []string{"proj_id=M0001_2550", "start_nav_date=2026-10-09", "end_nav_date=2026-10-09"} {
		if !strings.Contains(f.query(), want) {
			t.Fatalf("query %q missing %q", f.query(), want)
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
		f.navOverride = `{"items":[{"proj_id":"M0001_2550","nav_date":"` + s + `","last_val":9.5}]}`
		nav, ok, err := c.DailyNAV(context.Background(), "EXAMPLE-RMF", d)
		if err != nil || !ok || nav != 9.5 {
			t.Fatalf("%s: %v %v %v", s, nav, ok, err)
		}
	}
}

func TestDailyNAVErrors(t *testing.T) {
	t.Run("401", func(t *testing.T) {
		_, c := newFake(t)
		c.Key = "bad-secret-key"
		if _, ok, err := c.DailyNAV(context.Background(), "EXAMPLE-RMF", d); err == nil || ok {
			t.Fatalf("want error, got ok=%v err=%v", ok, err)
		} else if strings.Contains(err.Error(), c.Key) {
			t.Fatalf("error leaks key: %v", err)
		}
	})
	t.Run("403", func(t *testing.T) {
		f, c := newFake(t)
		f.navStatus = 403
		if _, ok, err := c.DailyNAV(context.Background(), "EXAMPLE-RMF", d); err == nil || ok || strings.Contains(err.Error(), c.Key) {
			t.Fatalf("want error, got ok=%v err=%v", ok, err)
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
			f.navOverride = `{"items":[{"proj_id":"M0001_2550","nav_date":"2026-10-09","last_val":` + v + `}]}`
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

func row(proj, date string, v string) string {
	return `{"proj_id":"` + proj + `","nav_date":"` + date + `","last_val":` + v + `}`
}

func TestDailyNAVRows(t *testing.T) {
	cases := []struct {
		name, items string
		nav         float64
		ok, err     bool
	}{
		{"same-value duplicates", row("M0001_2550", "2026-10-09", "5.5") + "," + row("M0001_2550", "2026-10-09", "5.5"), 5.5, true, false},
		{"conflicting classes", row("M0001_2550", "2026-10-09", "5.5") + "," + row("M0001_2550", "2026-10-09", "6.1"), 0, false, true},
		{"zero row", row("M0001_2550", "2026-10-09", "5.5") + "," + row("M0001_2550", "2026-10-09", "0"), 0, false, true},
		{"other proj ignored", row("M9999_2000", "2026-10-09", "7.7") + "," + row("M0001_2550", "2026-10-09", "5.5"), 5.5, true, false},
		{"only other proj", row("M9999_2000", "2026-10-09", "7.7"), 0, false, false},
		{"empty date skipped", row("M0001_2550", "", "7.7") + "," + row("M0001_2550", "2026-10-09", "5.5"), 5.5, true, false},
		{"unparseable date", row("M0001_2550", "09/10/2026", "5.5"), 0, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFake(t)
			f.navOverride = `{"items":[` + tc.items + `]}`
			nav, ok, err := c.DailyNAV(context.Background(), "EXAMPLE-RMF", d)
			if (err != nil) != tc.err || ok != tc.ok || nav != tc.nav {
				t.Fatalf("nav=%v ok=%v err=%v", nav, ok, err)
			}
			if tc.name == "conflicting classes" && !strings.Contains(err.Error(), "ambiguous multi-class NAV for EXAMPLE-RMF on 2026-10-09") {
				t.Fatalf("err: %v", err)
			}
		})
	}
}

func TestDailyNAVPagination(t *testing.T) {
	f, c := newFake(t)
	f.navOverride = `{"items":[` + row("M0001_2550", "2026-10-08", "1") + `],"next_cursor":"p2"}`
	f.navPages = map[string]string{"p2": `{"items":[` + row("M0001_2550", "2026-10-09", "5.5") + `]}`}
	nav, ok, err := c.DailyNAV(context.Background(), "EXAMPLE-RMF", d)
	if err != nil || !ok || nav != 5.5 || f.navs.Load() != 2 {
		t.Fatalf("nav=%v ok=%v err=%v navs=%d", nav, ok, err, f.navs.Load())
	}
}

func TestDailyNAVContextCancelled(t *testing.T) {
	_, c := newFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok, err := c.DailyNAV(ctx, "EXAMPLE-RMF", d); err == nil || ok {
		t.Fatalf("want error, got ok=%v err=%v", ok, err)
	}
}

func TestResponseTooLarge(t *testing.T) {
	f, c := newFake(t)
	f.navOverride = `{"items":[],"pad":"` + strings.Repeat("x", maxBody) + `"}`
	if _, ok, err := c.DailyNAV(context.Background(), "EXAMPLE-RMF", d); err == nil || ok {
		t.Fatalf("want error, got ok=%v err=%v", ok, err)
	}
}
