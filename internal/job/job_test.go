package job

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pallat/iwealth/internal/config"
	"github.com/pallat/iwealth/internal/store"
	"github.com/pallat/iwealth/internal/tax"
)

const income = 812345.0

type fakeSEC struct {
	mu   sync.Mutex
	navs map[string]float64 // fund|date
	errs map[string]error   // fund
}

func newSEC() *fakeSEC { return &fakeSEC{navs: map[string]float64{}, errs: map[string]error{}} }
func (f *fakeSEC) set(fund string, date time.Time, nav float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.navs[fund+"|"+date.Format("2006-01-02")] = nav
}
func (f *fakeSEC) DailyNAV(_ context.Context, fund string, date time.Time) (float64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs[fund]; err != nil {
		return 0, false, err
	}
	v, ok := f.navs[fund+"|"+date.Format("2006-01-02")]
	return v, ok, nil
}

type fakeNotifier struct {
	msgs []string
	fail bool
}

func (n *fakeNotifier) Send(_ context.Context, t string) error {
	if n.fail {
		return errors.New("send failed")
	}
	n.msgs = append(n.msgs, t)
	return nil
}

type fakeSum struct {
	out   string
	err   error
	facts []string
}

func (s *fakeSum) Summarize(_ context.Context, facts string) (string, error) {
	s.facts = append(s.facts, facts)
	return s.out, s.err
}

func day(m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, bangkok) }

type env struct {
	j   *Job
	sec *fakeSEC
	n   *fakeNotifier
	sum *fakeSum
	st  *store.Store
	now time.Time
}

func setup(t *testing.T, funds ...config.Fund) *env {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	rules := &tax.Rules{
		Year:     2026,
		Brackets: []tax.Bracket{{UpTo: 150000, Rate: 0}, {UpTo: 1000000, Rate: 0.2}, {UpTo: 0, Rate: 0.35}},
		Caps:     map[config.FundType]float64{config.RMF: 500000, config.SSF: 200000, config.ThaiESG: 300000},
		Combined: tax.Combined{Cap: 500000, Types: []config.FundType{config.RMF, config.SSF}},
	}
	if len(funds) == 0 {
		funds = []config.Fund{{Code: "ABC-RMF", Type: config.RMF}}
	}
	cfg := &config.Config{
		Funds: funds, PeakWindowDays: 252, DropTriggers: []float64{5, 10, 15},
		AnnualNetIncome: income, RunAt: "20:00", RetryUntil: "23:30", RetryInterval: 30 * time.Minute,
	}
	e := &env{sec: newSEC(), n: &fakeNotifier{}, sum: &fakeSum{out: "ตลาดปรับตัวลง"}, st: st}
	e.now = time.Date(2026, 10, 12, 20, 0, 0, 0, bangkok)
	e.j = &Job{
		Cfg: cfg, Rules: rules, Store: st, SEC: e.sec, Notify: e.n, Advisor: e.sum,
		Context: func(context.Context) string { return "SET -1%" },
		Now:     func() time.Time { return e.now },
		Sleep:   func(_ context.Context, d time.Duration) error { e.now = e.now.Add(d); return nil },
	}
	return e
}

func (e *env) seed(t *testing.T, fund string, date time.Time, nav float64) {
	t.Helper()
	if err := e.st.UpsertNAV(context.Background(), fund, date, nav); err != nil {
		t.Fatal(err)
	}
}

func (e *env) run(t *testing.T, date time.Time, fund string, nav float64) []string {
	t.Helper()
	e.sec.set(fund, date, nav)
	missing, err := e.j.RunOnce(context.Background(), date)
	if err != nil || len(missing) != 0 {
		t.Fatalf("RunOnce: missing=%v err=%v", missing, err)
	}
	return e.n.msgs
}

func TestAlertOnNewLevel(t *testing.T) {
	e := setup(t)
	e.seed(t, "ABC-RMF", day(10, 1), 100)
	wantSaved := money(e.j.Rules.TaxSaved(income, 500000))

	msgs := e.run(t, day(10, 2), "ABC-RMF", 88)
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	for _, w := range []string{"ตลาดปรับตัวลง", "ABC-RMF", "ลดหย่อน", money(500000), wantSaved} {
		if !strings.Contains(msgs[0], w) {
			t.Errorf("message missing %q:\n%s", w, msgs[0])
		}
	}
	// same level: nothing new
	if got := e.run(t, day(10, 3), "ABC-RMF", 88); len(got) != 1 {
		t.Fatalf("same level re-alerted: %d", len(got))
	}
	// deeper level alerts again
	if got := e.run(t, day(10, 4), "ABC-RMF", 84); len(got) != 2 {
		t.Fatalf("deeper level should alert: %d", len(got))
	}
	// recovery resets silently
	if got := e.run(t, day(10, 5), "ABC-RMF", 99); len(got) != 2 {
		t.Fatalf("recovery must not alert: %d", len(got))
	}
	lvl, _ := e.st.LastLevel(context.Background(), "ABC-RMF")
	if lvl != 0 {
		t.Fatalf("level after recovery = %v", lvl)
	}
	// dropping again after recovery alerts again
	if got := e.run(t, day(10, 6), "ABC-RMF", 88); len(got) != 3 {
		t.Fatalf("re-drop after recovery should alert: %d", len(got))
	}
}

func TestFailedSendRetriesNextRun(t *testing.T) {
	e := setup(t)
	e.seed(t, "ABC-RMF", day(10, 1), 100)
	e.sec.set("ABC-RMF", day(10, 2), 88)
	e.n.fail = true
	if _, err := e.j.RunOnce(context.Background(), day(10, 2)); err == nil {
		t.Fatal("want error from failed send")
	}
	if lvl, _ := e.st.LastLevel(context.Background(), "ABC-RMF"); lvl != 0 {
		t.Fatalf("level set despite failed send: %v", lvl)
	}
	e.n.fail = false
	if _, err := e.j.RunOnce(context.Background(), day(10, 2)); err != nil || len(e.n.msgs) != 1 {
		t.Fatalf("retry: err=%v msgs=%d", err, len(e.n.msgs))
	}
}

func TestOrdinaryFundHasNoPlan(t *testing.T) {
	e := setup(t, config.Fund{Code: "ORD", Type: config.Ordinary})
	e.seed(t, "ORD", day(10, 1), 100)
	msgs := e.run(t, day(10, 2), "ORD", 88)
	if len(msgs) != 1 {
		t.Fatalf("want alert, got %d", len(msgs))
	}
	for _, bad := range []string{"ลดหย่อน", "ซื้อ", "ภาษี"} {
		if strings.Contains(msgs[0], bad) {
			t.Errorf("ordinary alert contains %q:\n%s", bad, msgs[0])
		}
	}
}

func TestSummarizerFailureStillAlerts(t *testing.T) {
	e := setup(t)
	e.sum.err = errors.New("llm down")
	e.seed(t, "ABC-RMF", day(10, 1), 100)
	msgs := e.run(t, day(10, 2), "ABC-RMF", 88)
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	for _, w := range []string{"12.0", "ไม่มีบทสรุป", "ลดหย่อน", money(500000)} {
		if !strings.Contains(msgs[0], w) {
			t.Errorf("missing %q:\n%s", w, msgs[0])
		}
	}
}

func TestContextEmptyStillAlerts(t *testing.T) {
	e := setup(t)
	e.j.Context = nil
	e.seed(t, "ABC-RMF", day(10, 1), 100)
	if msgs := e.run(t, day(10, 2), "ABC-RMF", 88); len(msgs) != 1 {
		t.Fatalf("want alert, got %d", len(msgs))
	}
}

func TestUnpublishedNAVReportedMissing(t *testing.T) {
	e := setup(t)
	missing, err := e.j.RunOnce(context.Background(), day(10, 2))
	if err != nil || len(missing) != 1 || missing[0] != "ABC-RMF" || len(e.n.msgs) != 0 {
		t.Fatalf("missing=%v err=%v msgs=%v", missing, err, e.n.msgs)
	}
}

func TestOneFundErrorDoesNotStopOthers(t *testing.T) {
	e := setup(t, config.Fund{Code: "BAD", Type: config.Ordinary}, config.Fund{Code: "ORD", Type: config.Ordinary})
	e.sec.errs["BAD"] = errors.New("boom")
	e.seed(t, "ORD", day(10, 1), 100)
	e.sec.set("ORD", day(10, 2), 88)
	_, err := e.j.RunOnce(context.Background(), day(10, 2))
	if err == nil || !strings.Contains(err.Error(), "BAD") {
		t.Fatalf("want joined error naming BAD, got %v", err)
	}
	if len(e.n.msgs) != 1 {
		t.Fatalf("ORD alert not sent: %d", len(e.n.msgs))
	}
}

func TestSingleNAVPointStillWorks(t *testing.T) {
	e := setup(t)
	if msgs := e.run(t, day(10, 2), "ABC-RMF", 50); len(msgs) != 0 {
		t.Fatalf("no drop expected: %v", msgs)
	}
}

func TestWeekendNoFailureNotice(t *testing.T) {
	e := setup(t)
	if err := e.j.RunDaily(context.Background(), day(10, 10)); err != nil { // Saturday
		t.Fatal(err)
	}
	if len(e.n.msgs) != 0 {
		t.Fatalf("weekend sent %v", e.n.msgs)
	}
}

func TestRunDailyRetriesThenSucceeds(t *testing.T) {
	e := setup(t)
	e.seed(t, "ABC-RMF", day(10, 9), 100)
	d := day(10, 12)
	sleeps := 0
	e.j.Sleep = func(_ context.Context, dur time.Duration) error {
		e.now = e.now.Add(dur)
		sleeps++
		if sleeps == 2 {
			e.sec.set("ABC-RMF", d, 80)
		}
		return nil
	}
	if err := e.j.RunDaily(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if sleeps != 2 || len(e.n.msgs) != 1 {
		t.Fatalf("sleeps=%d msgs=%d", sleeps, len(e.n.msgs))
	}
}

func TestRunDailyAllMissingSoftNotice(t *testing.T) {
	e := setup(t, config.Fund{Code: "A", Type: config.Ordinary}, config.Fund{Code: "B", Type: config.Ordinary})
	if err := e.j.RunDaily(context.Background(), day(10, 12)); err != nil {
		t.Fatal(err)
	}
	if len(e.n.msgs) != 1 || !strings.Contains(e.n.msgs[0], "ทุกกอง") || !strings.Contains(e.n.msgs[0], "วันหยุด") {
		t.Fatalf("want one soft notice, got %v", e.n.msgs)
	}
	if e.now.After(time.Date(2026, 10, 12, 23, 30, 0, 0, bangkok).Add(e.j.Cfg.RetryInterval)) {
		t.Fatalf("slept past RetryUntil: %v", e.now)
	}
}

func TestRunDailyPartialMissingFailureNotice(t *testing.T) {
	e := setup(t, config.Fund{Code: "A", Type: config.Ordinary}, config.Fund{Code: "B", Type: config.Ordinary})
	d := day(10, 12)
	e.sec.set("A", d, 10)
	if err := e.j.RunDaily(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if len(e.n.msgs) != 1 || !strings.Contains(e.n.msgs[0], "B") || strings.Contains(e.n.msgs[0], "A,") || strings.Contains(e.n.msgs[0], "ทุกกอง") {
		t.Fatalf("want failure notice listing B, got %v", e.n.msgs)
	}
}

func TestRunDailyHonorsContext(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.j.Sleep = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
	if err := e.j.RunDaily(ctx, day(10, 12)); !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
	if len(e.n.msgs) != 0 {
		t.Fatalf("no notice on cancel: %v", e.n.msgs)
	}
}

func TestDateNormalizedToBangkok(t *testing.T) {
	e := setup(t)
	// 2026-10-12 18:00 UTC is already 2026-10-13 in Bangkok.
	utc := time.Date(2026, 10, 12, 18, 0, 0, 0, time.UTC)
	e.sec.set("ABC-RMF", day(10, 13), 100)
	missing, err := e.j.RunOnce(context.Background(), utc)
	if err != nil || len(missing) != 0 {
		t.Fatalf("missing=%v err=%v", missing, err)
	}
	if ok, _ := e.st.HasNAV(context.Background(), "ABC-RMF", day(10, 13)); !ok {
		t.Fatal("NAV not stored under Bangkok date")
	}
}

func TestFactsNeverContainIncomeOrAmounts(t *testing.T) {
	e := setup(t)
	e.seed(t, "ABC-RMF", day(10, 1), 100)
	e.run(t, day(10, 2), "ABC-RMF", 88)
	if len(e.sum.facts) != 1 {
		t.Fatalf("facts calls = %d", len(e.sum.facts))
	}
	f := e.sum.facts[0]
	for _, w := range []string{"ABC-RMF", "rmf", "12.0", "88", "100", "252", "SET -1%", "2026-10-01", "2026-10-02"} {
		if !strings.Contains(f, w) {
			t.Errorf("facts missing %q:\n%s", w, f)
		}
	}
	room := e.j.Rules.RemainingRoom(config.RMF, nil)
	saved := e.j.Rules.TaxSaved(income, room)
	for _, bad := range []string{
		"812345", "812,345", money(income), money(room), "500000", "500,000", money(saved), fmt.Sprintf("%.0f", saved),
	} {
		if strings.Contains(f, bad) {
			t.Errorf("facts leak %q:\n%s", bad, f)
		}
	}
}

func TestHandleCommand(t *testing.T) {
	e := setup(t, config.Fund{Code: "ABC-RMF", Type: config.RMF}, config.Fund{Code: "ORD", Type: config.Ordinary})
	ctx := context.Background()
	d := day(10, 12)
	e.now = d.Add(10 * time.Hour)

	r := e.j.HandleCommand(ctx, "/paid abc-rmf 10000")
	room := e.j.Rules.RemainingRoom(config.RMF, map[config.FundType]float64{config.RMF: 10000})
	saved := e.j.Rules.TaxSaved(income, 10000)
	for _, w := range []string{"บันทึก", "ABC-RMF", money(10000), money(room), money(saved)} {
		if !strings.Contains(r, w) {
			t.Errorf("paid reply missing %q: %s", w, r)
		}
	}
	paid, _ := e.st.PaidByType(ctx, 2026)
	if paid[config.RMF] != 10000 {
		t.Fatalf("paid = %v", paid)
	}
	// repeat is not deduplicated
	e.j.HandleCommand(ctx, "/paid ABC-RMF 10000")
	if paid, _ = e.st.PaidByType(ctx, 2026); paid[config.RMF] != 20000 {
		t.Fatalf("repeat should add: %v", paid)
	}
	for _, in := range []string{"/paid UNKNOWN 5", "/paid ORD 5", "/paid ABC-RMF abc", "/paid"} {
		before, _ := e.st.PaidByType(ctx, 2026)
		if r := e.j.HandleCommand(ctx, in); r == "" || strings.HasPrefix(r, "บันทึก") {
			t.Errorf("%q: bad reply %q", in, r)
		}
		after, _ := e.st.PaidByType(ctx, 2026)
		if fmt.Sprint(before) != fmt.Sprint(after) {
			t.Errorf("%q recorded something", in)
		}
	}
	tr := e.j.HandleCommand(ctx, "/tax")
	for _, w := range []string{"RMF", money(20000), money(500000), money(e.j.Rules.RemainingRoom(config.RMF, map[config.FundType]float64{config.RMF: 20000}))} {
		if !strings.Contains(tr, w) {
			t.Errorf("/tax missing %q: %s", w, tr)
		}
	}
	if h := e.j.HandleCommand(ctx, "hello"); !strings.Contains(h, "/paid") || !strings.Contains(h, "/tax") {
		t.Errorf("help: %s", h)
	}
}

func TestSchedule(t *testing.T) {
	cfg := &config.Config{RunAt: "20:00", RetryUntil: "23:30", RetryInterval: time.Minute}
	if err := ValidateSchedule(cfg); err != nil {
		t.Fatal(err)
	}
	for _, c := range []config.Config{
		{RunAt: "25:00", RetryUntil: "23:30", RetryInterval: time.Minute},
		{RunAt: "20:00", RetryUntil: "7pm", RetryInterval: time.Minute},
		{RunAt: "20:00", RetryUntil: "19:00", RetryInterval: time.Minute},
		{RunAt: "20:00", RetryUntil: "23:30"},
	} {
		if ValidateSchedule(&c) == nil {
			t.Errorf("want error for %+v", c)
		}
	}
	now := time.Date(2026, 10, 12, 19, 0, 0, 0, bangkok)
	if got := NextRun(now, "20:00"); !got.Equal(time.Date(2026, 10, 12, 20, 0, 0, 0, bangkok)) {
		t.Errorf("same day: %v", got)
	}
	now = time.Date(2026, 10, 12, 20, 0, 0, 0, bangkok)
	if got := NextRun(now, "20:00"); !got.Equal(time.Date(2026, 10, 13, 20, 0, 0, 0, bangkok)) {
		t.Errorf("next day: %v", got)
	}
}
