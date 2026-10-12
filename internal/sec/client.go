// UNVERIFIED against the live API: schema taken from github.com/jwitmann/sec-go; re-verify with a real SEC_API_KEY.
//
// Schema notes (SEC Open API V2):
//   - base URL https://api.sec.or.th, auth header Ocp-Apim-Subscription-Key
//   - fund search: GET /v2/fund/general-info/profiles?project_info=<abbr>&page_size=100
//     -> {"items":[{"proj_id","proj_abbr_name",...}],"next_cursor"}
//   - daily NAV: GET /v2/fund/daily-info/nav?proj_id=..&start_nav_date=YYYY-MM-DD&end_nav_date=YYYY-MM-DD
//     -> {"items":[{"proj_id","nav_date","last_val",...}],"next_cursor"}; NAV value is last_val
//   - no data: HTTP 204; dates may be RFC3339, fractional seconds without zone, or YYYY-MM-DD
package sec

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const maxBody = 4 << 20

// NAVSource returns the published NAV of a fund.
type NAVSource interface {
	// DailyNAV returns ok=false (nil error) when SEC has not published a NAV for that date yet.
	DailyNAV(ctx context.Context, fundCode string, date time.Time) (nav float64, ok bool, err error)
}

type Client struct {
	BaseURL, Key string
	HTTP         *http.Client

	mu    sync.Mutex
	projs map[string]string
}

// New returns a Client for the SEC API using the given subscription key.
func New(key string) *Client {
	return &Client{BaseURL: "https://api.sec.or.th", Key: key, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

type page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor"`
}

type profile struct {
	ProjID       string `json:"proj_id"`
	ProjAbbrName string `json:"proj_abbr_name"`
}

type navItem struct {
	ProjID  string  `json:"proj_id"`
	NavDate string  `json:"nav_date"`
	LastVal float64 `json:"last_val"`
}

// get performs a GET; found=false means HTTP 204 (no data).
func (c *Client) get(ctx context.Context, path string, q url.Values, out any) (found bool, err error) {
	u := strings.TrimRight(c.BaseURL, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, fmt.Errorf("sec: build request: %w", err)
	}
	req.Header.Set("Ocp-Apim-Subscription-Key", c.Key)
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		// url.Error carries the URL (no key); header is never included.
		return false, fmt.Errorf("sec: GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return false, fmt.Errorf("sec: GET %s: status %d", path, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return false, fmt.Errorf("sec: read %s: %w", path, err)
	}
	if len(body) > maxBody {
		return false, fmt.Errorf("sec: GET %s: response exceeds %d bytes", path, maxBody)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return false, fmt.Errorf("sec: decode %s: %w", path, err)
	}
	return true, nil
}

func (c *Client) projID(ctx context.Context, fundCode string) (string, error) {
	code := strings.ToUpper(strings.TrimSpace(fundCode))
	c.mu.Lock()
	id, ok := c.projs[code]
	c.mu.Unlock()
	if ok {
		return id, nil
	}
	cursor := ""
	for {
		q := url.Values{"project_info": {code}, "page_size": {"100"}}
		if cursor != "" {
			q.Set("next_cursor", cursor)
		}
		var p page[profile]
		found, err := c.get(ctx, "/v2/fund/general-info/profiles", q, &p)
		if err != nil {
			return "", err
		}
		if !found {
			break
		}
		for _, it := range p.Items {
			if strings.EqualFold(strings.TrimSpace(it.ProjAbbrName), code) && it.ProjID != "" {
				c.mu.Lock()
				if c.projs == nil {
					c.projs = map[string]string{}
				}
				c.projs[code] = it.ProjID
				c.mu.Unlock()
				return it.ProjID, nil
			}
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	return "", fmt.Errorf("sec: unknown fund %q: no matching proj_abbr_name", fundCode)
}

var dateLayouts = []string{
	time.RFC3339Nano, time.RFC3339,
	"2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02",
}

// sameDay reports whether the API date string is the calendar day of d.
// The date part is compared as written (no timezone shifting). A non-empty
// string that matches no known layout is an error.
func sameDay(s string, d time.Time) (bool, error) {
	s = strings.TrimSpace(s)
	for _, l := range dateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.Format("2006-01-02") == d.Format("2006-01-02"), nil
		}
	}
	return false, fmt.Errorf("sec: unrecognised nav_date format %q", s)
}

func (c *Client) DailyNAV(ctx context.Context, fundCode string, date time.Time) (float64, bool, error) {
	id, err := c.projID(ctx, fundCode)
	if err != nil {
		return 0, false, err
	}
	day := date.Format("2006-01-02")
	var vals []float64
	cursor := ""
	for {
		q := url.Values{"proj_id": {id}, "start_nav_date": {day}, "end_nav_date": {day}, "page_size": {"100"}}
		if cursor != "" {
			q.Set("next_cursor", cursor)
		}
		var p page[navItem]
		found, err := c.get(ctx, "/v2/fund/daily-info/nav", q, &p)
		if err != nil {
			return 0, false, err
		}
		if !found {
			break
		}
		for _, it := range p.Items {
			if it.ProjID != id || strings.TrimSpace(it.NavDate) == "" {
				continue
			}
			same, err := sameDay(it.NavDate, date)
			if err != nil {
				return 0, false, err
			}
			if !same {
				continue
			}
			if it.LastVal <= 0 {
				return 0, false, fmt.Errorf("sec: invalid NAV %v for %s on %s", it.LastVal, fundCode, day)
			}
			vals = append(vals, it.LastVal)
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if len(vals) == 0 {
		return 0, false, nil
	}
	for _, v := range vals[1:] {
		if v != vals[0] {
			return 0, false, fmt.Errorf("sec: ambiguous multi-class NAV for %s on %s", fundCode, day)
		}
	}
	return vals[0], true, nil
}

var _ NAVSource = (*Client)(nil)
