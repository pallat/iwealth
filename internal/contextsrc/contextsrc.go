// Package contextsrc gathers public market context (benchmarks, headlines,
// search snippets) to be passed to an external LLM. Only public data may be
// emitted; all remote text is untrusted and sanitized into single-line data.
package contextsrc

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/pallat/iwealth/internal/config"
	"github.com/pallat/iwealth/internal/dip"
)

const (
	maxBody       = 2 << 20 // 2 MiB per response
	maxText       = 200     // runes per headline/snippet/link
	maxSearch     = 5
	clientTimeout = 15 * time.Second
	callTimeout   = 30 * time.Second
	defaultQuery  = "Thai mutual fund market news"
)

type Provider interface {
	Name() string
	// Fetch returns plain-text facts, or "" if nothing to add.
	Fetch(ctx context.Context) (string, error)
}

type Histories interface {
	History(ctx context.Context, fund string) ([]dip.Point, error)
}

// clean collapses all whitespace/control characters to single spaces and
// truncates to maxText runes, so remote text stays on one line.
func clean(s string) string {
	var b strings.Builder
	space := false
	n := 0
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			if n++; n > maxText {
				break
			}
			b.WriteByte(' ')
		}
		space = false
		if n++; n > maxText {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

func get(ctx context.Context, hc *http.Client, u string, hdr map[string]string) ([]byte, error) {
	if hc == nil {
		hc = &http.Client{Timeout: clientTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, errors.New("bad request")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // drop URL (may carry secrets)
		}
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxBody))
}

// ---- benchmark ----

type benchmark struct {
	h     Histories
	funds []string
}

func NewBenchmark(h Histories, funds []string) Provider { return &benchmark{h, funds} }
func (b *benchmark) Name() string                       { return "benchmark" }

func (b *benchmark) Fetch(ctx context.Context) (string, error) {
	var lines []string
	for _, f := range b.funds {
		pts, err := b.h.History(ctx, f)
		if err != nil || len(pts) < 2 {
			continue
		}
		pts = append([]dip.Point(nil), pts...)
		sort.Slice(pts, func(i, j int) bool { return pts[i].Date.Before(pts[j].Date) })
		last := pts[len(pts)-1].NAV
		prev := pts[len(pts)-2].NAV
		if prev == 0 {
			continue
		}
		line := fmt.Sprintf("%s: NAV %.4f, 1d %+.2f%%", clean(f), last, (last-prev)/prev*100)
		if len(pts) > 20 {
			if base := pts[len(pts)-21].NAV; base != 0 {
				line += fmt.Sprintf(", 20d %+.2f%%", (last-base)/base*100)
			}
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"), nil
}

// ---- rss ----

type rss struct {
	urls    []string
	perFeed int
	hc      *http.Client
}

func NewRSS(urls []string, perFeed int, hc *http.Client) Provider {
	return &rss{urls, perFeed, hc}
}
func (r *rss) Name() string { return "rss" }

type feedDoc struct {
	Items []struct {
		Title string `xml:"title"`
		Link  string `xml:"link"`
	} `xml:"channel>item"`
	Entries []struct {
		Title string `xml:"title"`
		Link  struct {
			Href string `xml:"href,attr"`
		} `xml:"link"`
	} `xml:"entry"`
}

func (r *rss) Fetch(ctx context.Context) (string, error) {
	var lines []string
	for _, u := range r.urls {
		body, err := get(ctx, r.hc, u, nil)
		if err != nil {
			continue
		}
		var d feedDoc
		if xml.Unmarshal(body, &d) != nil {
			continue
		}
		n := 0
		add := func(title, link string) {
			if n >= r.perFeed {
				return
			}
			if title = clean(title); title != "" {
				lines = append(lines, title+" - "+clean(link))
				n++
			}
		}
		for _, it := range d.Items {
			add(it.Title, it.Link)
		}
		for _, e := range d.Entries {
			add(e.Title, e.Link.Href)
		}
	}
	return strings.Join(lines, "\n"), nil
}

// ---- search ----

type search struct {
	url, key, query string
	hc              *http.Client
}

func NewSearch(url, key, query string, hc *http.Client) Provider {
	return &search{url, key, query, hc}
}
func (s *search) Name() string { return "search" }

func (s *search) Fetch(ctx context.Context) (string, error) {
	u, err := url.Parse(s.url)
	if err != nil {
		return "", errors.New("bad search url")
	}
	q := u.Query()
	q.Set("q", s.query)
	u.RawQuery = q.Encode()
	body, err := get(ctx, s.hc, u.String(), map[string]string{"Authorization": "Bearer " + s.key})
	if err != nil {
		return "", err
	}
	var resp struct {
		Results []struct{ Title, URL, Snippet string } `json:"results"`
	}
	if json.Unmarshal(body, &resp) != nil {
		return "", errors.New("bad search response")
	}
	var lines []string
	for _, r := range resp.Results {
		if len(lines) >= maxSearch {
			break
		}
		title := clean(r.Title)
		if title == "" {
			continue
		}
		line := title + " - " + clean(r.URL)
		if sn := clean(r.Snippet); sn != "" {
			line += ": " + sn
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"), nil
}

// ---- assembly ----

func Build(cfg *config.Config, h Histories, hc *http.Client) []Provider {
	if hc == nil {
		hc = &http.Client{Timeout: clientTimeout}
	}
	var ps []Provider
	for _, name := range cfg.ContextProviders {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "benchmark":
			codes := cfg.Benchmarks
			if len(codes) == 0 {
				for _, f := range cfg.Funds {
					codes = append(codes, f.Code)
				}
			}
			if len(codes) > 0 && h != nil {
				ps = append(ps, NewBenchmark(h, codes))
			}
		case "rss":
			if len(cfg.RSSURLs) > 0 {
				ps = append(ps, NewRSS(cfg.RSSURLs, 5, hc))
			}
		case "search":
			if cfg.SearchAPIURL != "" {
				ps = append(ps, NewSearch(cfg.SearchAPIURL, cfg.SearchAPIKey, defaultQuery, hc))
			}
		}
	}
	return ps
}

// Gather joins non-empty provider sections; failing providers are skipped.
func Gather(ctx context.Context, ps []Provider) string {
	var secs []string
	for _, p := range ps {
		cctx, cancel := context.WithTimeout(ctx, callTimeout)
		out, err := p.Fetch(cctx)
		cancel()
		if err != nil || strings.TrimSpace(out) == "" {
			continue
		}
		secs = append(secs, "## "+p.Name()+"\n"+out)
	}
	return strings.Join(secs, "\n\n")
}
