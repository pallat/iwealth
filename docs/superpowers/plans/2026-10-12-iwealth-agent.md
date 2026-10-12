# iwealth Agent Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A single Go binary for a Raspberry Pi that ingests Thai fund NAV daily, detects dips from a rolling peak, and sends Telegram alerts with an LLM market summary and a tax-aware action plan.

**Architecture:** Deterministic pipeline (`sec -> store -> dip -> plan -> advisor -> telegram`). Money math lives in pure Go packages. The ADK agent (DeepSeek via a custom `model.LLM` adapter) only writes the narrative, only on alert days, and never sees income or holdings.

**Tech Stack:** Go (go.mod `go 1.25`), `google.golang.org/adk` v1.8.0, `google.golang.org/genai`, `modernc.org/sqlite`, `github.com/joho/godotenv`, `gopkg.in/yaml.v3`, stdlib `net/http` for SEC/DeepSeek/Telegram.

**Spec:** `docs/superpowers/specs/2026-10-12-iwealth-agent-design.md`

## Global Constraints

- Module path `github.com/pallat/iwealth`; binary `cmd/iwealth`.
- Single binary, SQLite single file, in-process scheduler, no external services besides SEC, DeepSeek, Telegram (and optional RSS/search).
- LLM: model `deepseek-v4-pro`, base URL `https://api.deepseek.com`, Chat Completions.
- Never send `ANNUAL_NET_INCOME`, holdings, purchase history, or plan amounts to DeepSeek/search.
- Default `PEAK_WINDOW_DAYS=252`, `RUN_AT=20:00`, `RETRY_UNTIL=23:30`, `RETRY_INTERVAL=30m`.
- Tax rules in `tax_rules.yaml`, never hard-coded.
- Secrets only in `.env` (gitignored). Telegram restricted to `TELEGRAM_CHAT_ID`.
- Cross-compile target `GOOS=linux GOARCH=arm64`, `CGO_ENABLED=0`.
- No test calls a live API.

## Review Focus

1. NAV missing/zero/duplicated for a day, or fewer points than the peak window (new fund) — expect no panic, peak over available points.
2. Holiday or weekend run — expect skip without a failure notice.
3. Tax-room edge: paid exceeds cap, combined cap binding, income below first taxable bracket (saving 0), income exactly on a bracket edge.
4. Alert dedupe: deeper level re-alerts, same level does not, recovery above trigger then new drop re-alerts.
5. Telegram messages from a chat other than `TELEGRAM_CHAT_ID` must be ignored; malformed `/paid` input must not crash or record.

---

## File Structure

```
go.mod
.env.example
tax_rules.yaml
Makefile
deploy/iwealth.service
cmd/iwealth/main.go
internal/config/config.go            + config_test.go
internal/tax/tax.go                  + tax_test.go
internal/dip/dip.go                  + dip_test.go
internal/plan/plan.go                + plan_test.go
internal/store/store.go              + store_test.go
internal/sec/client.go               + client_test.go + testdata/
internal/llm/deepseek.go             + deepseek_test.go
internal/advisor/advisor.go          + advisor_test.go
internal/contextsrc/contextsrc.go    + contextsrc_test.go
internal/notify/telegram/telegram.go + telegram_test.go
internal/job/job.go                  + job_test.go   (orchestration)
```

---

### Task 1: Scaffold and config

**Files:**
- Create: `go.mod`, `.env.example`, `tax_rules.yaml`, `internal/config/config.go`, `internal/config/config_test.go`

**Interfaces:**
- Produces:
```go
package config

type FundType string
const (RMF FundType = "rmf"; SSF FundType = "ssf"; ThaiESG FundType = "thaiesg"; Ordinary FundType = "ordinary")
type Fund struct { Code string; Type FundType }
type Config struct {
    SECAPIKey, DeepSeekKey, TelegramToken string
    TelegramChatID int64
    Funds []Fund
    PeakWindowDays int
    DropTriggers []float64 // percent, ascending
    AnnualNetIncome float64
    RunAt string // "20:00"
    RetryUntil string // "23:30"
    RetryInterval time.Duration
    ContextProviders []string
    Benchmarks, RSSURLs []string
    SearchAPIURL, SearchAPIKey string
    DBPath, TaxRulesPath string
}
func Load(getenv func(string) string) (*Config, error)
```

- [ ] **Step 1: Init module and deps**

```bash
cd /Users/pallat/workspace/github.com/pallat/iwealth
go mod init github.com/pallat/iwealth
go mod edit -go=1.25
go get google.golang.org/adk@v1.8.0 modernc.org/sqlite github.com/joho/godotenv gopkg.in/yaml.v3
```

- [ ] **Step 2: Write the failing test** `internal/config/config_test.go`

```go
package config

import "testing"

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func base() map[string]string {
	return map[string]string{
		"SEC_API_KEY": "s", "DEEPSEEK_API_KEY": "d", "TELEGRAM_BOT_TOKEN": "t",
		"TELEGRAM_CHAT_ID": "42", "FUNDS": "ABC-RMF:rmf, XYZ:ordinary",
		"ANNUAL_NET_INCOME": "800000",
	}
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(base()))
	if err != nil {
		t.Fatal(err)
	}
	if c.PeakWindowDays != 252 || c.RunAt != "20:00" || c.RetryUntil != "23:30" {
		t.Fatalf("bad defaults: %+v", c)
	}
	if len(c.Funds) != 2 || c.Funds[0] != (Fund{"ABC-RMF", RMF}) || c.Funds[1].Type != Ordinary {
		t.Fatalf("funds: %+v", c.Funds)
	}
	if len(c.DropTriggers) != 3 || c.DropTriggers[0] != 5 {
		t.Fatalf("triggers: %v", c.DropTriggers)
	}
	if c.TelegramChatID != 42 || c.RetryInterval.Minutes() != 30 {
		t.Fatalf("misc: %+v", c)
	}
}

func TestLoadErrors(t *testing.T) {
	cases := map[string]func(map[string]string){
		"missing sec key": func(m map[string]string) { delete(m, "SEC_API_KEY") },
		"bad fund type":   func(m map[string]string) { m["FUNDS"] = "A:bogus" },
		"bad fund format": func(m map[string]string) { m["FUNDS"] = "A" },
		"no funds":        func(m map[string]string) { m["FUNDS"] = "" },
		"bad chat id":     func(m map[string]string) { m["TELEGRAM_CHAT_ID"] = "x" },
		"bad trigger":     func(m map[string]string) { m["DROP_TRIGGERS"] = "5,abc" },
		"bad window":      func(m map[string]string) { m["PEAK_WINDOW_DAYS"] = "0" },
	}
	for name, mut := range cases {
		m := base()
		mut(m)
		if _, err := Load(env(m)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `go test ./internal/config/ -v`
Expected: FAIL (`undefined: Load`)

- [ ] **Step 4: Implement** `internal/config/config.go`

```go
package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

type FundType string

const (
	RMF      FundType = "rmf"
	SSF      FundType = "ssf"
	ThaiESG  FundType = "thaiesg"
	Ordinary FundType = "ordinary"
)

type Fund struct {
	Code string
	Type FundType
}

type Config struct {
	SECAPIKey, DeepSeekKey, TelegramToken string
	TelegramChatID                        int64
	Funds                                 []Fund
	PeakWindowDays                        int
	DropTriggers                          []float64
	AnnualNetIncome                       float64
	RunAt, RetryUntil                     string
	RetryInterval                         time.Duration
	ContextProviders                      []string
	Benchmarks, RSSURLs                   []string
	SearchAPIURL, SearchAPIKey            string
	DBPath, TaxRulesPath                  string
}

func Load(getenv func(string) string) (*Config, error) {
	c := &Config{
		SECAPIKey:     getenv("SEC_API_KEY"),
		DeepSeekKey:   getenv("DEEPSEEK_API_KEY"),
		TelegramToken: getenv("TELEGRAM_BOT_TOKEN"),
		SearchAPIURL:  getenv("SEARCH_API_URL"),
		SearchAPIKey:  getenv("SEARCH_API_KEY"),
		RunAt:         or(getenv("RUN_AT"), "20:00"),
		RetryUntil:    or(getenv("RETRY_UNTIL"), "23:30"),
		DBPath:        or(getenv("DB_PATH"), "iwealth.db"),
		TaxRulesPath:  or(getenv("TAX_RULES_PATH"), "tax_rules.yaml"),
	}
	for k, v := range map[string]string{"SEC_API_KEY": c.SECAPIKey, "DEEPSEEK_API_KEY": c.DeepSeekKey, "TELEGRAM_BOT_TOKEN": c.TelegramToken} {
		if v == "" {
			return nil, fmt.Errorf("%s is required", k)
		}
	}
	var err error
	if c.TelegramChatID, err = strconv.ParseInt(getenv("TELEGRAM_CHAT_ID"), 10, 64); err != nil {
		return nil, fmt.Errorf("TELEGRAM_CHAT_ID: %w", err)
	}
	if c.Funds, err = parseFunds(getenv("FUNDS")); err != nil {
		return nil, err
	}
	c.PeakWindowDays = 252
	if v := getenv("PEAK_WINDOW_DAYS"); v != "" {
		if c.PeakWindowDays, err = strconv.Atoi(v); err != nil || c.PeakWindowDays < 1 {
			return nil, fmt.Errorf("PEAK_WINDOW_DAYS must be a positive integer")
		}
	}
	if c.DropTriggers, err = parseFloats(or(getenv("DROP_TRIGGERS"), "5,10,15")); err != nil {
		return nil, fmt.Errorf("DROP_TRIGGERS: %w", err)
	}
	sort.Float64s(c.DropTriggers)
	if v := getenv("ANNUAL_NET_INCOME"); v != "" {
		if c.AnnualNetIncome, err = strconv.ParseFloat(v, 64); err != nil {
			return nil, fmt.Errorf("ANNUAL_NET_INCOME: %w", err)
		}
	}
	if c.RetryInterval, err = time.ParseDuration(or(getenv("RETRY_INTERVAL"), "30m")); err != nil {
		return nil, fmt.Errorf("RETRY_INTERVAL: %w", err)
	}
	c.ContextProviders = split(getenv("CONTEXT_PROVIDERS"))
	c.Benchmarks = split(getenv("BENCHMARKS"))
	c.RSSURLs = split(getenv("RSS_URLS"))
	return c, nil
}

func or(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseFloats(s string) ([]float64, error) {
	var out []float64
	for _, p := range split(s) {
		f, err := strconv.ParseFloat(p, 64)
		if err != nil || f <= 0 {
			return nil, fmt.Errorf("invalid number %q", p)
		}
		out = append(out, f)
	}
	return out, nil
}

func parseFunds(s string) ([]Fund, error) {
	var out []Fund
	for _, p := range split(s) {
		code, typ, ok := strings.Cut(p, ":")
		if !ok || code == "" {
			return nil, fmt.Errorf("FUNDS entry %q must be code:type", p)
		}
		switch ft := FundType(strings.ToLower(strings.TrimSpace(typ))); ft {
		case RMF, SSF, ThaiESG, Ordinary:
			out = append(out, Fund{strings.TrimSpace(code), ft})
		default:
			return nil, fmt.Errorf("FUNDS entry %q: unknown type %q", p, typ)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("FUNDS is required")
	}
	return out, nil
}
```

- [ ] **Step 5: Run to verify pass**

Run: `go test ./internal/config/ -v`
Expected: PASS

- [ ] **Step 6: Add `.env.example` and `tax_rules.yaml`**

`.env.example`:
```
SEC_API_KEY=
DEEPSEEK_API_KEY=
TELEGRAM_BOT_TOKEN=
TELEGRAM_CHAT_ID=
# SEC fund abbreviation:type  (rmf|ssf|thaiesg|ordinary)
FUNDS=
PEAK_WINDOW_DAYS=252
DROP_TRIGGERS=5,10,15
ANNUAL_NET_INCOME=
RUN_AT=20:00
RETRY_UNTIL=23:30
RETRY_INTERVAL=30m
CONTEXT_PROVIDERS=benchmark
BENCHMARKS=
RSS_URLS=
SEARCH_API_URL=
SEARCH_API_KEY=
```

`tax_rules.yaml` (VERIFY every number against current Revenue Department rules before relying on it; income-percentage limits such as 30% of income are not modelled — set the absolute caps to what applies to you):
```yaml
year: 2026
brackets:        # up_to: 0 means no upper limit
  - {up_to: 150000, rate: 0}
  - {up_to: 300000, rate: 0.05}
  - {up_to: 500000, rate: 0.10}
  - {up_to: 750000, rate: 0.15}
  - {up_to: 1000000, rate: 0.20}
  - {up_to: 2000000, rate: 0.25}
  - {up_to: 5000000, rate: 0.30}
  - {up_to: 0, rate: 0.35}
caps:
  rmf: 500000
  ssf: 200000
  thaiesg: 300000
combined:
  cap: 500000
  types: [rmf, ssf]
```

- [ ] **Step 7: Commit**

```bash
git add -A && git commit -m "feat: scaffold module and config loader"
```

---

### Task 2: Tax engine

**Files:**
- Create: `internal/tax/tax.go`, `internal/tax/tax_test.go`

**Interfaces:**
- Consumes: `config.FundType`
- Produces:
```go
package tax

type Bracket struct { UpTo float64 `yaml:"up_to"`; Rate float64 `yaml:"rate"` }
type Combined struct { Cap float64 `yaml:"cap"`; Types []config.FundType `yaml:"types"` }
type Rules struct {
    Year     int                          `yaml:"year"`
    Brackets []Bracket                    `yaml:"brackets"`
    Caps     map[config.FundType]float64  `yaml:"caps"`
    Combined Combined                     `yaml:"combined"`
}
func LoadRules(path string) (*Rules, error)
func (r *Rules) Tax(netIncome float64) float64
func (r *Rules) TaxSaved(netIncome, extraDeduction float64) float64
func (r *Rules) RemainingRoom(t config.FundType, paid map[config.FundType]float64) float64
```

- [ ] **Step 1: Write the failing test**

```go
package tax

import (
	"testing"

	"github.com/pallat/iwealth/internal/config"
)

func rules() *Rules {
	return &Rules{
		Brackets: []Bracket{{150000, 0}, {300000, 0.05}, {500000, 0.10}, {0, 0.20}},
		Caps:     map[config.FundType]float64{config.RMF: 500000, config.SSF: 200000, config.ThaiESG: 300000},
		Combined: Combined{Cap: 500000, Types: []config.FundType{config.RMF, config.SSF}},
	}
}

func TestTax(t *testing.T) {
	r := rules()
	cases := []struct{ in, want float64 }{
		{0, 0}, {150000, 0}, {300000, 7500}, {500000, 27500}, {600000, 47500}, {-5, 0},
	}
	for _, c := range cases {
		if got := r.Tax(c.in); got != c.want {
			t.Errorf("Tax(%v)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestTaxSaved(t *testing.T) {
	r := rules()
	if got := r.TaxSaved(600000, 100000); got != 20000 { // 47500 - 27500
		t.Errorf("got %v", got)
	}
	if got := r.TaxSaved(100000, 50000); got != 0 {
		t.Errorf("below threshold got %v", got)
	}
	if got := r.TaxSaved(100000, 999999); got != 0 { // deduction > income
		t.Errorf("over-deduction got %v", got)
	}
}

func TestRemainingRoom(t *testing.T) {
	r := rules()
	cases := []struct {
		name string
		typ  config.FundType
		paid map[config.FundType]float64
		want float64
	}{
		{"fresh rmf limited by combined", config.RMF, nil, 500000},
		{"ssf own cap", config.SSF, nil, 200000},
		{"rmf with ssf paid, combined binds", config.RMF, map[config.FundType]float64{config.SSF: 200000}, 300000},
		{"thaiesg outside combined", config.ThaiESG, map[config.FundType]float64{config.RMF: 500000}, 300000},
		{"overpaid never negative", config.SSF, map[config.FundType]float64{config.SSF: 250000}, 0},
		{"ordinary has none", config.Ordinary, nil, 0},
	}
	for _, c := range cases {
		if got := r.RemainingRoom(c.typ, c.paid); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/tax/ -v` → FAIL (undefined)

- [ ] **Step 3: Implement** `internal/tax/tax.go`

```go
package tax

import (
	"fmt"
	"os"
	"slices"

	"gopkg.in/yaml.v3"

	"github.com/pallat/iwealth/internal/config"
)

type Bracket struct {
	UpTo float64 `yaml:"up_to"`
	Rate float64 `yaml:"rate"`
}

type Combined struct {
	Cap   float64           `yaml:"cap"`
	Types []config.FundType `yaml:"types"`
}

type Rules struct {
	Year     int                         `yaml:"year"`
	Brackets []Bracket                   `yaml:"brackets"`
	Caps     map[config.FundType]float64 `yaml:"caps"`
	Combined Combined                    `yaml:"combined"`
}

func LoadRules(path string) (*Rules, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Rules
	if err := yaml.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(r.Brackets) == 0 {
		return nil, fmt.Errorf("%s: no brackets", path)
	}
	return &r, nil
}

// Tax computes progressive tax on net taxable income.
func (r *Rules) Tax(netIncome float64) float64 {
	if netIncome <= 0 {
		return 0
	}
	var total, lower float64
	for _, b := range r.Brackets {
		upper := b.UpTo
		if upper == 0 || netIncome < upper {
			upper = netIncome
		}
		if upper > lower {
			total += (upper - lower) * b.Rate
		}
		if b.UpTo == 0 || netIncome <= b.UpTo {
			break
		}
		lower = b.UpTo
	}
	return total
}

// TaxSaved is the tax reduction from an extra deduction.
func (r *Rules) TaxSaved(netIncome, extra float64) float64 {
	return r.Tax(netIncome) - r.Tax(max(netIncome-extra, 0))
}

// RemainingRoom is how much more can be deducted for fund type t this year.
func (r *Rules) RemainingRoom(t config.FundType, paid map[config.FundType]float64) float64 {
	cap, ok := r.Caps[t]
	if !ok {
		return 0
	}
	room := cap - paid[t]
	if slices.Contains(r.Combined.Types, t) {
		var used float64
		for _, ct := range r.Combined.Types {
			used += paid[ct]
		}
		room = min(room, r.Combined.Cap-used)
	}
	return max(room, 0)
}
```

- [ ] **Step 4: Run to verify pass** — `go test ./internal/tax/ -v` → PASS
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: progressive tax and deduction room"`

---

### Task 3: Dip detection

**Files:**
- Create: `internal/dip/dip.go`, `internal/dip/dip_test.go`

**Interfaces:**
- Produces:
```go
package dip

type Point struct { Date time.Time; NAV float64 }
// Peak returns the max-NAV point in the last `window` points (points ascending by date).
func Peak(points []Point, window int) (Point, bool)
func DropPct(peak, current float64) float64            // 0 if peak<=0 or current>=peak
func Level(drop float64, triggers []float64) float64   // highest trigger <= drop, else 0 (triggers ascending)
func ShouldAlert(level, lastLevel float64) bool        // level > lastLevel
```

- [ ] **Step 1: Write the failing test**

```go
package dip

import (
	"testing"
	"time"
)

func pts(v ...float64) []Point {
	out := make([]Point, len(v))
	for i, n := range v {
		out[i] = Point{time.Date(2026, 1, 1+i, 0, 0, 0, 0, time.UTC), n}
	}
	return out
}

func TestPeakWindow(t *testing.T) {
	p, ok := Peak(pts(10, 20, 15, 12), 3)
	if !ok || p.NAV != 20 {
		t.Fatalf("got %v %v", p, ok)
	}
	p, _ = Peak(pts(10, 20, 15, 12), 2) // 20 falls out of window
	if p.NAV != 15 {
		t.Fatalf("window not applied: %v", p)
	}
	p, ok = Peak(pts(10, 12), 252) // fewer points than window
	if !ok || p.NAV != 12 {
		t.Fatalf("short history: %v %v", p, ok)
	}
	if _, ok := Peak(nil, 5); ok {
		t.Fatal("empty must be !ok")
	}
}

func TestDropPct(t *testing.T) {
	if got := DropPct(100, 90); got != 10 {
		t.Errorf("got %v", got)
	}
	for _, c := range [][2]float64{{100, 100}, {100, 110}, {0, 5}} {
		if got := DropPct(c[0], c[1]); got != 0 {
			t.Errorf("DropPct(%v,%v)=%v want 0", c[0], c[1], got)
		}
	}
}

func TestLevelAndAlert(t *testing.T) {
	tr := []float64{5, 10, 15}
	cases := []struct{ drop, want float64 }{{0, 0}, {4.99, 0}, {5, 5}, {12, 10}, {40, 15}}
	for _, c := range cases {
		if got := Level(c.drop, tr); got != c.want {
			t.Errorf("Level(%v)=%v want %v", c.drop, got, c.want)
		}
	}
	if !ShouldAlert(10, 5) || ShouldAlert(10, 10) || ShouldAlert(5, 10) || ShouldAlert(0, 0) {
		t.Fatal("ShouldAlert wrong")
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/dip/ -v` → FAIL

- [ ] **Step 3: Implement** `internal/dip/dip.go`

```go
package dip

import "time"

type Point struct {
	Date time.Time
	NAV  float64
}

func Peak(points []Point, window int) (Point, bool) {
	if len(points) == 0 || window < 1 {
		return Point{}, false
	}
	if len(points) > window {
		points = points[len(points)-window:]
	}
	best := points[0]
	for _, p := range points[1:] {
		if p.NAV > best.NAV {
			best = p
		}
	}
	return best, true
}

func DropPct(peak, current float64) float64 {
	if peak <= 0 || current >= peak {
		return 0
	}
	return (peak - current) / peak * 100
}

func Level(drop float64, triggers []float64) float64 {
	var lvl float64
	for _, t := range triggers {
		if drop >= t {
			lvl = t
		}
	}
	return lvl
}

func ShouldAlert(level, lastLevel float64) bool { return level > lastLevel }
```

- [ ] **Step 4: Run to verify pass** — PASS
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: rolling peak dip detection"`

---

### Task 4: Store (SQLite)

**Files:**
- Create: `internal/store/store.go`, `internal/store/store_test.go`

**Interfaces:**
- Consumes: `dip.Point`, `config.FundType`
- Produces:
```go
package store

type Store struct{ db *sql.DB }
func Open(path string) (*Store, error)               // ":memory:" allowed in tests
func (s *Store) Close() error
func (s *Store) UpsertNAV(ctx context.Context, fund string, date time.Time, nav float64) error
func (s *Store) HasNAV(ctx context.Context, fund string, date time.Time) (bool, error)
func (s *Store) History(ctx context.Context, fund string) ([]dip.Point, error)          // ascending
func (s *Store) LastLevel(ctx context.Context, fund string) (float64, error)
func (s *Store) SetLevel(ctx context.Context, fund string, level float64) error
func (s *Store) AddPaid(ctx context.Context, fund string, t config.FundType, amount float64, on time.Time) error
func (s *Store) PaidByType(ctx context.Context, year int) (map[config.FundType]float64, error)
```

- [ ] **Step 1: Write the failing test**

```go
package store

import (
	"context"
	"testing"
	"time"

	"github.com/pallat/iwealth/internal/config"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func d(m time.Month, day int) time.Time { return time.Date(2026, m, day, 0, 0, 0, 0, time.UTC) }

func TestNAVUpsertAndHistory(t *testing.T) {
	s, ctx := open(t), context.Background()
	s.UpsertNAV(ctx, "A", d(1, 2), 11)
	s.UpsertNAV(ctx, "A", d(1, 1), 10)
	s.UpsertNAV(ctx, "A", d(1, 2), 12) // overwrite, no duplicate
	h, err := s.History(ctx, "A")
	if err != nil || len(h) != 2 || h[0].NAV != 10 || h[1].NAV != 12 {
		t.Fatalf("%v %v", h, err)
	}
	if ok, _ := s.HasNAV(ctx, "A", d(1, 2)); !ok {
		t.Fatal("HasNAV false")
	}
	if ok, _ := s.HasNAV(ctx, "A", d(1, 3)); ok {
		t.Fatal("HasNAV true for missing")
	}
}

func TestLevel(t *testing.T) {
	s, ctx := open(t), context.Background()
	if l, _ := s.LastLevel(ctx, "A"); l != 0 {
		t.Fatalf("default %v", l)
	}
	s.SetLevel(ctx, "A", 10)
	s.SetLevel(ctx, "A", 15)
	if l, _ := s.LastLevel(ctx, "A"); l != 15 {
		t.Fatalf("got %v", l)
	}
}

func TestPaidByTypeYearScoped(t *testing.T) {
	s, ctx := open(t), context.Background()
	s.AddPaid(ctx, "A", config.RMF, 1000, d(3, 1))
	s.AddPaid(ctx, "A", config.RMF, 500, d(4, 1))
	s.AddPaid(ctx, "B", config.SSF, 200, d(5, 1))
	s.AddPaid(ctx, "A", config.RMF, 9999, time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC))
	m, err := s.PaidByType(ctx, 2026)
	if err != nil || m[config.RMF] != 1500 || m[config.SSF] != 200 {
		t.Fatalf("%v %v", m, err)
	}
}
```

- [ ] **Step 2: Run to verify failure** — FAIL

- [ ] **Step 3: Implement** `internal/store/store.go`

```go
package store

import (
	"context"
	"database/sql"
	"time"

	_ "modernc.org/sqlite"

	"github.com/pallat/iwealth/internal/config"
	"github.com/pallat/iwealth/internal/dip"
)

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS nav (fund TEXT NOT NULL, date TEXT NOT NULL, nav REAL NOT NULL, PRIMARY KEY (fund, date));
CREATE TABLE IF NOT EXISTS alert_state (fund TEXT PRIMARY KEY, level REAL NOT NULL);
CREATE TABLE IF NOT EXISTS tax_paid (id INTEGER PRIMARY KEY AUTOINCREMENT, fund TEXT NOT NULL, type TEXT NOT NULL, amount REAL NOT NULL, paid_on TEXT NOT NULL);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // single writer; also keeps :memory: on one connection
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

const dayFmt = "2006-01-02"

func (s *Store) UpsertNAV(ctx context.Context, fund string, date time.Time, nav float64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO nav(fund,date,nav) VALUES(?,?,?)
		ON CONFLICT(fund,date) DO UPDATE SET nav=excluded.nav`, fund, date.Format(dayFmt), nav)
	return err
}

func (s *Store) HasNAV(ctx context.Context, fund string, date time.Time) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM nav WHERE fund=? AND date=?`, fund, date.Format(dayFmt)).Scan(&n)
	return n > 0, err
}

func (s *Store) History(ctx context.Context, fund string) ([]dip.Point, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT date, nav FROM nav WHERE fund=? ORDER BY date`, fund)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []dip.Point
	for rows.Next() {
		var ds string
		var p dip.Point
		if err := rows.Scan(&ds, &p.NAV); err != nil {
			return nil, err
		}
		if p.Date, err = time.Parse(dayFmt, ds); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) LastLevel(ctx context.Context, fund string) (float64, error) {
	var l float64
	err := s.db.QueryRowContext(ctx, `SELECT level FROM alert_state WHERE fund=?`, fund).Scan(&l)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return l, err
}

func (s *Store) SetLevel(ctx context.Context, fund string, level float64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO alert_state(fund,level) VALUES(?,?)
		ON CONFLICT(fund) DO UPDATE SET level=excluded.level`, fund, level)
	return err
}

func (s *Store) AddPaid(ctx context.Context, fund string, t config.FundType, amount float64, on time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO tax_paid(fund,type,amount,paid_on) VALUES(?,?,?,?)`,
		fund, string(t), amount, on.Format(dayFmt))
	return err
}

func (s *Store) PaidByType(ctx context.Context, year int) (map[config.FundType]float64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT type, SUM(amount) FROM tax_paid
		WHERE paid_on >= ? AND paid_on < ? GROUP BY type`,
		time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC).Format(dayFmt),
		time.Date(year+1, 1, 1, 0, 0, 0, 0, time.UTC).Format(dayFmt))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[config.FundType]float64{}
	for rows.Next() {
		var t string
		var sum float64
		if err := rows.Scan(&t, &sum); err != nil {
			return nil, err
		}
		out[config.FundType(t)] = sum
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run to verify pass** — PASS
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: sqlite store"`

---

### Task 5: Action plan

**Files:**
- Create: `internal/plan/plan.go`, `internal/plan/plan_test.go`

**Interfaces:**
- Consumes: `tax.Rules`, `config.FundType`
- Produces:
```go
package plan

type Plan struct { Room, Suggested, TaxSaved float64 }
// Build returns ok=false for funds with no deduction (ordinary) or no room left.
func Build(r *tax.Rules, netIncome float64, paid map[config.FundType]float64, t config.FundType) (Plan, bool)
```

- [ ] **Step 1: Write the failing test**

```go
package plan

import (
	"testing"

	"github.com/pallat/iwealth/internal/config"
	"github.com/pallat/iwealth/internal/tax"
)

func rules() *tax.Rules {
	return &tax.Rules{
		Brackets: []tax.Bracket{{150000, 0}, {300000, 0.05}, {500000, 0.10}, {0, 0.20}},
		Caps:     map[config.FundType]float64{config.SSF: 200000},
	}
}

func TestBuild(t *testing.T) {
	p, ok := Build(rules(), 600000, map[config.FundType]float64{config.SSF: 100000}, config.SSF)
	if !ok || p.Room != 100000 || p.Suggested != 100000 || p.TaxSaved != 20000 {
		t.Fatalf("%+v %v", p, ok)
	}
	if _, ok := Build(rules(), 600000, nil, config.Ordinary); ok {
		t.Fatal("ordinary must have no plan")
	}
	if _, ok := Build(rules(), 600000, map[config.FundType]float64{config.SSF: 200000}, config.SSF); ok {
		t.Fatal("no room must have no plan")
	}
}
```

- [ ] **Step 2: Run to verify failure** — FAIL
- [ ] **Step 3: Implement** `internal/plan/plan.go`

```go
package plan

import (
	"github.com/pallat/iwealth/internal/config"
	"github.com/pallat/iwealth/internal/tax"
)

type Plan struct{ Room, Suggested, TaxSaved float64 }

func Build(r *tax.Rules, netIncome float64, paid map[config.FundType]float64, t config.FundType) (Plan, bool) {
	room := r.RemainingRoom(t, paid)
	if room <= 0 {
		return Plan{}, false
	}
	return Plan{Room: room, Suggested: room, TaxSaved: r.TaxSaved(netIncome, room)}, true
}
```

- [ ] **Step 4: Run to verify pass** — PASS
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: tax-aware action plan"`

---

### Task 6: SEC client

The SEC V2 response shape and the fund identification scheme are **unverified**. This task starts by discovering them; do not skip Step 1.

**Files:**
- Create: `internal/sec/client.go`, `internal/sec/client_test.go`, `internal/sec/testdata/nav.json`, `internal/sec/testdata/funds.json`

**Interfaces:**
- Produces:
```go
package sec

type NAVSource interface {
    // DailyNAV returns ok=false (nil error) when SEC has not published a NAV for that date yet.
    DailyNAV(ctx context.Context, fundCode string, date time.Time) (nav float64, ok bool, err error)
}
type Client struct { BaseURL, Key string; HTTP *http.Client }
func New(key string) *Client   // BaseURL defaults to https://api.sec.or.th
```

- [ ] **Step 1: Discover the real API (needs the user's `SEC_API_KEY`)**

Ask the user for the key in `.env` (never commit). Then, with the SEC developer portal docs open (https://api.sec.or.th), run a request against `/v2/fund/daily-info/nav` and the fund-list endpoint, e.g.:

```bash
set -a; source .env; set +a
curl -s -H "Ocp-Apim-Subscription-Key: $SEC_API_KEY" "https://api.sec.or.th/v2/fund/daily-info/nav?<params from portal docs>" | tee internal/sec/testdata/nav.json | head -c 1500
```

Record in a short comment at the top of `client.go`: auth header name, query parameters, how a fund is identified (the SEC API typically keys on `proj_id`, not the abbreviation, so a lookup from `FUNDS` abbreviation to `proj_id` is likely needed — save that lookup response as `testdata/funds.json`), date format, the NAV field name, and when a not-yet-published date looks like (empty list vs 404). Redact nothing sensitive: fixtures contain only public data and never the key.

- [ ] **Step 2: Write the failing test** against the saved fixtures using `httptest.NewServer` serving the fixture files. Test cases: published NAV -> `(nav, true, nil)`; empty/unpublished -> `(0, false, nil)`; HTTP 401/500 -> error; unknown fund code -> error. The expected NAV value comes from `testdata/nav.json`. Example skeleton (adjust names to the discovered schema):

```go
func TestDailyNAVPublished(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Ocp-Apim-Subscription-Key") != "k" {
			http.Error(w, "no", 401)
			return
		}
		http.ServeFile(w, r, "testdata/nav.json")
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Key: "k", HTTP: srv.Client()}
	nav, ok, err := c.DailyNAV(context.Background(), "<code in fixture>", time.Date(/* fixture date */))
	if err != nil || !ok || nav != /* fixture value */ 0 {
		t.Fatalf("%v %v %v", nav, ok, err)
	}
}
```

- [ ] **Step 3: Run to verify failure** — `go test ./internal/sec/ -v` → FAIL
- [ ] **Step 4: Implement `Client`**: `New`, a `get(ctx, path, query, out any)` helper that sets the key header, returns an error including status code on non-2xx, decodes JSON; a cached abbreviation→`proj_id` resolver (map guarded by a mutex) if Step 1 shows it is needed; `DailyNAV` building the request from the discovered params and returning `ok=false` for the "not published" shape. Keep struct tags matching the fixture exactly.
- [ ] **Step 5: Run to verify pass** — PASS
- [ ] **Step 6: Commit** — `git add -A && git commit -m "feat: SEC daily NAV client"`

---

### Task 7: DeepSeek `model.LLM` adapter

**Files:**
- Create: `internal/llm/deepseek.go`, `internal/llm/deepseek_test.go`

**Interfaces:**
- Produces:
```go
package llm

type DeepSeek struct { BaseURL, Key, ModelName string; HTTP *http.Client }
func NewDeepSeek(key string) *DeepSeek                   // BaseURL https://api.deepseek.com, model deepseek-v4-pro
func (d *DeepSeek) Name() string
func (d *DeepSeek) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error]
```
Text-only, non-streaming (`stream` is ignored; one response yielded). Tools are not supported — the advisor has none.

- [ ] **Step 1: Write the failing test**

```go
package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

func TestGenerateContent(t *testing.T) {
	var got struct {
		Model    string `json:"model"`
		Messages []struct{ Role, Content string } `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "bad", 400)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"choices":[{"message":{"content":"สรุป"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	}))
	defer srv.Close()

	d := &DeepSeek{BaseURL: srv.URL, Key: "k", ModelName: "deepseek-v4-pro", HTTP: srv.Client()}
	req := &model.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText("facts", genai.RoleUser)},
		Config:   &genai.GenerateContentConfig{SystemInstruction: genai.NewContentFromText("be brief", genai.RoleUser)},
	}
	var text string
	for resp, err := range d.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatal(err)
		}
		text = resp.Content.Parts[0].Text
	}
	if text != "สรุป" {
		t.Fatalf("text=%q", text)
	}
	if got.Model != "deepseek-v4-pro" || len(got.Messages) != 2 ||
		got.Messages[0].Role != "system" || got.Messages[1].Role != "user" {
		t.Fatalf("request: %+v", got)
	}
}

func TestGenerateContentHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) }))
	defer srv.Close()
	d := &DeepSeek{BaseURL: srv.URL, Key: "k", ModelName: "m", HTTP: srv.Client()}
	var gotErr error
	for _, err := range d.GenerateContent(context.Background(), &model.LLMRequest{}, false) {
		gotErr = err
	}
	if gotErr == nil {
		t.Fatal("expected error")
	}
}
```

- [ ] **Step 2: Run to verify failure** — FAIL
- [ ] **Step 3: Implement** `internal/llm/deepseek.go`

```go
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
	"time"

	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

type DeepSeek struct {
	BaseURL, Key, ModelName string
	HTTP                    *http.Client
}

func NewDeepSeek(key string) *DeepSeek {
	return &DeepSeek{
		BaseURL: "https://api.deepseek.com", Key: key, ModelName: "deepseek-v4-pro",
		HTTP: &http.Client{Timeout: 120 * time.Second},
	}
}

func (d *DeepSeek) Name() string { return d.ModelName }

type chatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func text(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range c.Parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

func (d *DeepSeek) GenerateContent(ctx context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		var msgs []chatMsg
		if req.Config != nil && req.Config.SystemInstruction != nil {
			msgs = append(msgs, chatMsg{"system", text(req.Config.SystemInstruction)})
		}
		for _, c := range req.Contents {
			role := "user"
			if c.Role == genai.RoleModel {
				role = "assistant"
			}
			msgs = append(msgs, chatMsg{role, text(c)})
		}
		body, _ := json.Marshal(map[string]any{"model": d.ModelName, "messages": msgs})
		hr, err := http.NewRequestWithContext(ctx, http.MethodPost, d.BaseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			yield(nil, err)
			return
		}
		hr.Header.Set("Authorization", "Bearer "+d.Key)
		hr.Header.Set("Content-Type", "application/json")
		res, err := d.HTTP.Do(hr)
		if err != nil {
			yield(nil, err)
			return
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		if res.StatusCode/100 != 2 {
			yield(nil, fmt.Errorf("deepseek: status %d: %s", res.StatusCode, truncate(string(raw), 200)))
			return
		}
		var out struct {
			Choices []struct {
				Message struct{ Content string } `json:"message"`
			} `json:"choices"`
			Usage struct {
				Prompt     int32 `json:"prompt_tokens"`
				Completion int32 `json:"completion_tokens"`
				Total      int32 `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
			yield(nil, fmt.Errorf("deepseek: bad response: %v", err))
			return
		}
		yield(&model.LLMResponse{
			Content:       genai.NewContentFromText(out.Choices[0].Message.Content, genai.RoleModel),
			TurnComplete:  true,
			FinishReason:  genai.FinishReasonStop,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: out.Usage.Prompt, CandidatesTokenCount: out.Usage.Completion, TotalTokenCount: out.Usage.Total},
		}, nil)
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
```

- [ ] **Step 4: Run to verify pass** — `go test ./internal/llm/ -v` → PASS. If a genai field name differs from the above, fix to match `go doc google.golang.org/genai GenerateContentResponseUsageMetadata`.
- [ ] **Step 5: Live smoke (manual, optional, needs key):** `curl -s https://api.deepseek.com/chat/completions -H "Authorization: Bearer $DEEPSEEK_API_KEY" -H 'Content-Type: application/json' -d '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}'` — confirm the model name and path. If the model name is rejected, correct `NewDeepSeek` default and the Global Constraints line.
- [ ] **Step 6: Commit** — `git add -A && git commit -m "feat: DeepSeek model.LLM adapter"`

---

### Task 8: Advisor (ADK agent)

**Files:**
- Create: `internal/advisor/advisor.go`, `internal/advisor/advisor_test.go`

**Interfaces:**
- Consumes: `model.LLM`
- Produces:
```go
package advisor

type Advisor struct { r *runner.Runner }
func New(m model.LLM) (*Advisor, error)
// Summarize explains, in Thai, why a fund's NAV dropped using ONLY the facts text provided.
func (a *Advisor) Summarize(ctx context.Context, facts string) (string, error)
```

- [ ] **Step 1: Write the failing test** with a fake `model.LLM` that records the request and returns fixed text:

```go
package advisor

import (
	"context"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

type fakeLLM struct{ gotFacts, gotSystem string }

func (f *fakeLLM) Name() string { return "fake" }
func (f *fakeLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		for _, c := range req.Contents {
			for _, p := range c.Parts {
				f.gotFacts += p.Text
			}
		}
		if req.Config != nil && req.Config.SystemInstruction != nil {
			f.gotSystem = req.Config.SystemInstruction.Parts[0].Text
		}
		yield(&model.LLMResponse{Content: genai.NewContentFromText("ตลาดหุ้นลงทั้งแถบ", genai.RoleModel), TurnComplete: true}, nil)
	}
}

func TestSummarize(t *testing.T) {
	f := &fakeLLM{}
	a, err := New(f)
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Summarize(context.Background(), "FUND A drop 12%")
	if err != nil || out != "ตลาดหุ้นลงทั้งแถบ" {
		t.Fatalf("%q %v", out, err)
	}
	if !strings.Contains(f.gotFacts, "FUND A drop 12%") {
		t.Fatalf("facts not forwarded: %q", f.gotFacts)
	}
	if !strings.Contains(f.gotSystem, "unknown") {
		t.Fatalf("system prompt must forbid guessing: %q", f.gotSystem)
	}
}
```

- [ ] **Step 2: Run to verify failure** — FAIL
- [ ] **Step 3: Implement** `internal/advisor/advisor.go`

```go
package advisor

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

const instruction = `You are a market commentator for a personal Thai mutual fund tracker.
Write a short Thai summary (max 6 lines) explaining the NAV decline of the fund(s) using ONLY the facts given.
Say whether the move looks market-wide (compare with the benchmark figures) or fund-specific.
If the facts do not explain the cause, say it is unknown. Never invent events, numbers, or news.
Cite headlines only if provided. Do not give buy/sell instructions or mention amounts.`

type Advisor struct{ r *runner.Runner }

const appName, userID = "iwealth", "owner"

func New(m model.LLM) (*Advisor, error) {
	ag, err := llmagent.New(llmagent.Config{
		Name:        "advisor",
		Model:       m,
		Description: "Explains fund NAV declines from supplied facts.",
		Instruction: instruction,
	})
	if err != nil {
		return nil, err
	}
	r, err := runner.New(runner.Config{
		AppName: appName, Agent: ag, SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		return nil, err
	}
	return &Advisor{r}, nil
}

var seq int

func (a *Advisor) Summarize(ctx context.Context, facts string) (string, error) {
	seq++ // fresh session each call: no cross-day memory
	sid := fmt.Sprintf("s%d", seq)
	var sb strings.Builder
	msg := genai.NewContentFromText(facts, genai.RoleUser)
	for ev, err := range a.r.Run(ctx, userID, sid, msg, agent.RunConfig{}) {
		if err != nil {
			return "", err
		}
		if ev.IsFinalResponse() && ev.Content != nil {
			for _, p := range ev.Content.Parts {
				sb.WriteString(p.Text)
			}
		}
	}
	out := strings.TrimSpace(sb.String())
	if out == "" {
		return "", fmt.Errorf("advisor: empty response")
	}
	return out, nil
}
```

`seq` is only touched from the single job goroutine; if that changes, guard with a mutex.

- [ ] **Step 4: Run to verify pass** — `go test ./internal/advisor/ -v` → PASS. If `llmagent.Config` or `agent.RunConfig` fields differ, check `go doc google.golang.org/adk/agent/llmagent Config` (v1.8.0 quickstart uses Name/Model/Description/Instruction).
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: ADK advisor agent"`

---

### Task 9: Context providers

**Files:**
- Create: `internal/contextsrc/contextsrc.go`, `internal/contextsrc/contextsrc_test.go`

**Interfaces:**
- Consumes: `store.Store`-like history via a small interface (below), `config.Config`
- Produces:
```go
package contextsrc

type Provider interface {
    Name() string
    // Fetch returns plain-text facts, or "" if nothing to add.
    Fetch(ctx context.Context) (string, error)
}
type Histories interface{ History(ctx context.Context, fund string) ([]dip.Point, error) }

// Benchmark: for each tracked fund code, last NAV and 1-day/20-day % change from stored history (numbers only).
func NewBenchmark(h Histories, funds []string) Provider
// RSS: headlines (title + link) from feed URLs, max n items each.
func NewRSS(urls []string, perFeed int, hc *http.Client) Provider
// Search: GET {url}?q=<query> with Bearer key; expects JSON {"results":[{"title","url","snippet"}]}.
func NewSearch(url, key, query string, hc *http.Client) Provider
func Build(cfg *config.Config, h Histories, hc *http.Client) []Provider   // honours cfg.ContextProviders; unset URL/key => skipped
func Gather(ctx context.Context, ps []Provider) string                     // joins non-empty sections "## name\n..."; a failing provider is skipped, never fails the run
```

- [ ] **Step 1: Write the failing test** covering: Benchmark output contains each fund code and the correct 1-day % change from a fake `Histories`; RSS parses a canned RSS 2.0 `httptest` feed to `title - link` lines and respects `perFeed`; `Gather` skips a provider that returns an error and one that returns ""; `Build` returns no `rss` provider when `RSSURLs` is empty even if `rss` is listed.

```go
func TestGatherSkipsFailures(t *testing.T) {
	ps := []Provider{stub{"a", "x", nil}, stub{"b", "", nil}, stub{"c", "", errors.New("down")}}
	got := Gather(context.Background(), ps)
	if got != "## a\nx" {
		t.Fatalf("%q", got)
	}
}
```
(`stub` is a test-local struct implementing `Provider`.) Write the Benchmark/RSS tests in the same style with exact expected strings.

- [ ] **Step 2: Run to verify failure** — FAIL
- [ ] **Step 3: Implement** the four constructors, `Build`, `Gather`. RSS uses `encoding/xml` into `struct{ Channel struct{ Items []struct{ Title, Link string } `xml:"item"` } `xml:"channel"` }`. Search decodes the JSON shape above and emits `title - url: snippet` lines (max 5). All HTTP via `hc` with request context. Benchmark percent change = `(last-prev)/prev*100` formatted `%.2f`; skip funds with < 2 points.
- [ ] **Step 4: Run to verify pass** — PASS
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: pluggable context providers"`

---

### Task 10: Telegram notifier and commands

**Files:**
- Create: `internal/notify/telegram/telegram.go`, `internal/notify/telegram/telegram_test.go`

**Interfaces:**
- Produces:
```go
package telegram

type Bot struct { Token string; ChatID int64; BaseURL string; HTTP *http.Client }
func New(token string, chatID int64) *Bot                        // BaseURL https://api.telegram.org
func (b *Bot) Send(ctx context.Context, text string) error      // splits >4000 runes into several messages
// Poll long-polls getUpdates and calls handle(text) for messages from ChatID only; replies with the returned string. Returns when ctx is done.
func (b *Bot) Poll(ctx context.Context, handle func(ctx context.Context, text string) string) error
// ParsePaid parses "/paid <fund> <amount>"; error on any other shape, non-positive or non-numeric amount.
func ParsePaid(text string) (fund string, amount float64, err error)
```

- [ ] **Step 1: Write the failing test**

```go
func TestParsePaid(t *testing.T) {
	f, a, err := ParsePaid("/paid ABC-RMF 15000.50")
	if err != nil || f != "ABC-RMF" || a != 15000.50 {
		t.Fatalf("%v %v %v", f, a, err)
	}
	for _, bad := range []string{"/paid", "/paid A", "/paid A x", "/paid A -5", "/paid A 0", "/paid A 1 2", "/other A 5", ""} {
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
```

Add `TestPollIgnoresOtherChats`: fake server returns one update from chat 999 and one from chat 1 on the first `getUpdates` call, then blocks/cancels; assert `handle` is called exactly once with the chat-1 text and the reply is posted via `sendMessage` to chat 1.

- [ ] **Step 2: Run to verify failure** — FAIL
- [ ] **Step 3: Implement** with `POST {BaseURL}/bot{Token}/sendMessage` (JSON `{chat_id,text}`), `GET .../getUpdates?timeout=30&offset=N` tracking the next offset, ok-flag check on every response, error text never echoing the token (wrap errors, don't print the URL). Split on rune boundaries at 4000.
- [ ] **Step 4: Run to verify pass** — PASS
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: telegram notifier and command polling"`

---

### Task 11: Job orchestration and main

**Files:**
- Create: `internal/job/job.go`, `internal/job/job_test.go`, `cmd/iwealth/main.go`

**Interfaces:**
- Consumes: everything above. Declared locally in `job` to keep it testable:
```go
package job

type Notifier interface{ Send(ctx context.Context, text string) error }
type Summarizer interface{ Summarize(ctx context.Context, facts string) (string, error) }
type Store interface {
    UpsertNAV(ctx context.Context, fund string, date time.Time, nav float64) error
    HasNAV(ctx context.Context, fund string, date time.Time) (bool, error)
    History(ctx context.Context, fund string) ([]dip.Point, error)
    LastLevel(ctx context.Context, fund string) (float64, error)
    SetLevel(ctx context.Context, fund string, level float64) error
    PaidByType(ctx context.Context, year int) (map[config.FundType]float64, error)
}
type Job struct {
    Cfg *config.Config; Rules *tax.Rules; Store Store; SEC sec.NAVSource
    Notify Notifier; Advisor Summarizer; Context func(ctx context.Context) string
    Now func() time.Time
}
// RunOnce ingests `date`; returns missing = fund codes whose NAV is not yet published.
func (j *Job) RunOnce(ctx context.Context, date time.Time) (missing []string, err error)
// RunDaily retries RunOnce every RetryInterval until nothing is missing or RetryUntil passes; then sends a failure notice listing still-missing funds (unless date is a weekend).
func (j *Job) RunDaily(ctx context.Context, date time.Time) error
// HandleCommand serves /paid and /tax from Telegram.
func (j *Job) HandleCommand(ctx context.Context, text string) string
```

- [ ] **Step 1: Write the failing test** with fakes (`fakeSEC` returning scripted NAVs per fund/date, `fakeNotifier` collecting messages, `fakeSummarizer`, a real `store.Open(":memory:")`). Required tests:
  1. `TestAlertOnNewLevel`: history peak 100 then NAV 88 -> 12% drop, level 10 -> one message containing the summary text, "ลดหย่อน"-section amount and tax saved for an RMF fund; second `RunOnce` same level sends nothing; NAV 84 (level 15) sends again; NAV recovers to 99 (level 0) resets so a later 88 alerts again.
  2. `TestOrdinaryFundHasNoPlan`: ordinary fund alert contains no suggested-purchase line.
  3. `TestSummarizerFailureStillAlerts`: summarizer returns error -> message still sent with numbers and plan, no narrative.
  4. `TestUnpublishedNAVReportedMissing`: SEC returns `ok=false` -> fund in `missing`, no alert, no error.
  5. `TestWeekendNoFailureNotice`: `RunDaily` on a Saturday sends nothing and returns nil.
  6. `TestFactsNeverContainIncomeOrAmounts`: capture the `facts` string passed to the summarizer; assert it contains fund code and drop % but not the income value, not the suggested amount.
  7. `TestHandleCommand`: `/paid ABC-RMF 10000` records under the fund's configured type and replies with remaining room; `/paid UNKNOWN 5` replies with an error text and records nothing; `/tax` reports per-type paid/room.

- [ ] **Step 2: Run to verify failure** — FAIL
- [ ] **Step 3: Implement `job.go`**. Per fund in `Cfg.Funds`: if `HasNAV(date)` skip fetch, else `SEC.DailyNAV`; `!ok` -> append to missing; error -> collect and continue (return joined error at end); `UpsertNAV`. Then `History` -> `dip.Peak(history, window)` -> `DropPct(peak.NAV, latest.NAV)` -> `Level` -> compare with `LastLevel`: if `level==0` call `SetLevel(0)` and continue; if `ShouldAlert` -> build facts string (fund code, type name, latest NAV, peak NAV/date, drop %, window, `Context(ctx)`; no income, no amounts) -> `Advisor.Summarize` (on error, narrative omitted with a line "ไม่มีบทสรุป (LLM ใช้ไม่ได้)") -> if fund type != ordinary, `PaidByType(date.Year())` + `plan.Build` and append plan lines built in Go (suggested amount, tax saved) -> `Notify.Send` -> `SetLevel(level)` only after a successful send. Weekend check via `date.Weekday()`. `HandleCommand`: `telegram.ParsePaid`, look up the fund's type in `Cfg.Funds`, `AddPaid` is not on the `Job.Store` interface above — add `AddPaid(ctx, fund string, t config.FundType, amount float64, on time.Time) error` to the interface when implementing this step, and mirror it in the fake.
- [ ] **Step 4: Run to verify pass** — `go test ./internal/job/ -v` → PASS
- [ ] **Step 5: Implement `cmd/iwealth/main.go`**: `godotenv.Load()` (ignore missing file), `config.Load(os.Getenv)`, `tax.LoadRules`, `store.Open`, build `sec.New`, `llm.NewDeepSeek` -> `advisor.New`, `contextsrc.Build/Gather`, `telegram.New`; `signal.NotifyContext` for SIGINT/SIGTERM; goroutine `bot.Poll(ctx, job.HandleCommand)`; scheduler loop computing the next `RunAt` in `Asia/Bangkok` (`time.LoadLocation`; fatal with clear message if tzdata missing — import `_ "time/tzdata"`), sleeping with `select` on `ctx.Done()`, then `job.RunDaily(ctx, today)`. Flag `-once` runs `RunDaily` immediately for manual testing. Log with `log/slog`; never log keys.
- [ ] **Step 6: Build check** — `go build ./... && go vet ./... && go test ./...` → all PASS
- [ ] **Step 7: Commit** — `git add -A && git commit -m "feat: job orchestration and main"`

---

### Task 12: Deploy to Raspberry Pi

**Files:**
- Create: `Makefile`, `deploy/iwealth.service`, `README.md`

- [ ] **Step 1: Makefile**

```make
.PHONY: test build-pi
test:
	go test ./...
build-pi:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/iwealth-arm64 ./cmd/iwealth
```

- [ ] **Step 2: `deploy/iwealth.service`**

```ini
[Unit]
Description=iwealth personal fund agent
After=network-online.target
Wants=network-online.target

[Service]
User=iwealth
WorkingDirectory=/opt/iwealth
ExecStart=/opt/iwealth/iwealth-arm64
Restart=on-failure
RestartSec=30
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/opt/iwealth

[Install]
WantedBy=multi-user.target
```

- [ ] **Step 3: README** — document: create SEC key, DeepSeek key, Telegram bot + chat id; copy `.env.example` to `.env` (chmod 600) and `tax_rules.yaml` next to the binary; `make build-pi`; `scp bin/iwealth-arm64 deploy files to the Pi`; `systemctl enable --now iwealth`; run `./iwealth-arm64 -once` once to verify end to end; back up `iwealth.db`.
- [ ] **Step 4: Verify** — `make test build-pi && file bin/iwealth-arm64` -> expect "ELF 64-bit ... ARM aarch64".
- [ ] **Step 5: Commit** — `git add -A && git commit -m "chore: pi deployment"`

---

## Self-Review

- **Spec coverage:** ingest + retry + holidays (T6, T11), rolling peak + triggers + dedupe (T3, T4, T11), progressive tax + caps + tax saved (T2, T5), actual deduction tracking via `/paid` `/tax` (T4, T10, T11), LLM summary via ADK + DeepSeek adapter (T7, T8), pluggable context with config slots (T9), Telegram alerts (T10), privacy facts boundary (T11 test 6), `.env` funds config (T1), Pi deploy (T12), LLM-failure fallback (T11 test 3), SEC failure notice (T11).
- **Known unverified items, deliberately scheduled first:** SEC schema and fund identification (T6 Step 1), DeepSeek model name/path (T7 Step 5), `tax_rules.yaml` values (T1 Step 6). Tasks 9 and 10 describe tests and implementation in prose plus key snippets rather than full listings; the implementer writes them to the stated interfaces.
- **Type consistency:** `FundType`, `dip.Point`, `tax.Rules`, `plan.Plan` signatures match across tasks; `Job.Store` gains `AddPaid` in T11 (noted inline).
