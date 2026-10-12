// Package job orchestrates the daily NAV ingest, dip alerts and Telegram commands.
package job

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pallat/iwealth/internal/config"
	"github.com/pallat/iwealth/internal/dip"
	"github.com/pallat/iwealth/internal/notify/telegram"
	"github.com/pallat/iwealth/internal/plan"
	"github.com/pallat/iwealth/internal/sec"
	"github.com/pallat/iwealth/internal/tax"
)

type Notifier interface {
	Send(ctx context.Context, text string) error
}

type Summarizer interface {
	Summarize(ctx context.Context, facts string) (string, error)
}

type Store interface {
	UpsertNAV(ctx context.Context, fund string, date time.Time, nav float64) error
	HasNAV(ctx context.Context, fund string, date time.Time) (bool, error)
	History(ctx context.Context, fund string) ([]dip.Point, error)
	LastLevel(ctx context.Context, fund string) (float64, error)
	SetLevel(ctx context.Context, fund string, level float64) error
	AddPaid(ctx context.Context, fund string, t config.FundType, amount float64, on time.Time) error
	PaidByType(ctx context.Context, year int) (map[config.FundType]float64, error)
	SetDeduction(ctx context.Context, year int, key config.FundType, amount float64) error
	Deductions(ctx context.Context, year int) (map[config.FundType]float64, error)
	ReminderSent(ctx context.Context, day time.Time) (bool, error)
	MarkReminderSent(ctx context.Context, day time.Time) error
}

type Job struct {
	Cfg     *config.Config
	Rules   *tax.Rules
	Store   Store
	SEC     sec.NAVSource
	Notify  Notifier
	Advisor Summarizer
	Context func(ctx context.Context) string
	Now     func() time.Time
	// Sleep waits d or until ctx is done (returning ctx.Err()). Defaults to a timer.
	Sleep func(ctx context.Context, d time.Duration) error
	// Log receives operational events (per-fund ingest results, advisor errors).
	// Nil is silent. Messages never include keys, tokens, income or amounts.
	Log *slog.Logger
	// Tick is how often Loop re-reads the wall clock (default 1 minute).
	Tick time.Duration
}

func (j *Job) log(level slog.Level, msg string, args ...any) {
	if j.Log != nil {
		j.Log.Log(context.Background(), level, msg, args...)
	}
}

// bangkok is the market calendar. Thailand has no DST, so the fixed-zone
// fallback is exact if tzdata is unavailable.
var bangkok = func() *time.Location {
	if l, err := time.LoadLocation("Asia/Bangkok"); err == nil {
		return l
	}
	return time.FixedZone("Asia/Bangkok", 7*3600)
}()

// Location returns the Asia/Bangkok location used for all calendar logic.
func Location() *time.Location { return bangkok }

// Today returns the Bangkok calendar date of t at midnight.
func Today(t time.Time) time.Time { return bkkDate(t) }

func bkkDate(t time.Time) time.Time {
	y, m, d := t.In(bangkok).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, bangkok)
}

func (j *Job) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now()
}

func (j *Job) sleep(ctx context.Context, d time.Duration) error {
	if j.Sleep != nil {
		return j.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ---- schedule helpers ----

func parseClock(s string) (h, m int, err error) {
	hs, ms, ok := strings.Cut(s, ":")
	if !ok || len(hs) != 2 || len(ms) != 2 {
		return 0, 0, fmt.Errorf("%q must be HH:MM", s)
	}
	h, e1 := strconv.Atoi(hs)
	m, e2 := strconv.Atoi(ms)
	if e1 != nil || e2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("%q must be HH:MM between 00:00 and 23:59", s)
	}
	return h, m, nil
}

// ValidateSchedule checks RunAt, RetryUntil and RetryInterval.
func ValidateSchedule(c *config.Config) error {
	rh, rm, err := parseClock(c.RunAt)
	if err != nil {
		return fmt.Errorf("RUN_AT: %w", err)
	}
	uh, um, err := parseClock(c.RetryUntil)
	if err != nil {
		return fmt.Errorf("RETRY_UNTIL: %w", err)
	}
	if uh*60+um < rh*60+rm {
		return fmt.Errorf("RETRY_UNTIL %s must not be before RUN_AT %s", c.RetryUntil, c.RunAt)
	}
	if c.RetryInterval <= 0 {
		return errors.New("RETRY_INTERVAL must be greater than 0")
	}
	return nil
}

func at(d time.Time, clock string) time.Time {
	h, m, _ := parseClock(clock)
	y, mo, dd := d.In(bangkok).Date()
	return time.Date(y, mo, dd, h, m, 0, 0, bangkok)
}

// NextRun returns the first RunAt strictly after now (Bangkok).
func NextRun(now time.Time, runAt string) time.Time {
	t := at(now, runAt)
	if !t.After(now) {
		y, m, d := t.Date()
		t = at(time.Date(y, m, d+1, 12, 0, 0, 0, bangkok), runAt)
	}
	return t
}

// morning reports whether RUN_AT is early in the day (before 12:00). Such a run
// ingests the previous weekday's NAV, which is published the evening before.
func morning(runAt string) bool {
	h, _, _ := parseClock(runAt)
	return h < 12
}

// NAVDate is the NAV date a run on runDay ingests: the previous weekday for a
// morning RUN_AT, otherwise runDay itself (the previous weekday if runDay is a weekend).
func NAVDate(runDay time.Time, runAt string) time.Time {
	d := bkkDate(runDay)
	if wd := d.Weekday(); morning(runAt) || wd == time.Saturday || wd == time.Sunday {
		return prevWeekdays(d, 1)[0]
	}
	return d
}

// runsOn reports whether the daily ingest runs on runDay: Monday to Friday for an
// evening RUN_AT, Tuesday to Saturday for a morning one (Sunday and Monday would
// only repeat Saturday's Friday NAV).
func runsOn(runDay time.Time, runAt string) bool {
	wd := bkkDate(runDay).Weekday()
	if morning(runAt) {
		return wd != time.Sunday && wd != time.Monday
	}
	return wd != time.Saturday && wd != time.Sunday
}

// OnceDate is the NAV date a manual `-once` run targets: the one the scheduler would
// ingest for the latest run time (today's RUN_AT if it has passed, otherwise yesterday's).
func OnceDate(now time.Time, runAt string) time.Time {
	d := bkkDate(now)
	if now.Before(at(d, runAt)) {
		d = time.Date(d.Year(), d.Month(), d.Day()-1, 0, 0, 0, 0, bangkok)
	}
	return NAVDate(d, runAt)
}

func (j *Job) tick() time.Duration {
	if j.Tick > 0 {
		return j.Tick
	}
	return time.Minute
}

// due reports whether the daily run should start at now: a run day, inside
// [RunAt, RetryUntil], with at least one tracked fund lacking the target NAV.
func (j *Job) due(ctx context.Context, now time.Time) bool {
	d := bkkDate(now)
	if !runsOn(d, j.Cfg.RunAt) {
		return false
	}
	if now.Before(at(d, j.Cfg.RunAt)) || now.After(at(d, j.Cfg.RetryUntil)) {
		return false
	}
	target := NAVDate(d, j.Cfg.RunAt)
	for _, f := range j.Cfg.Funds {
		has, err := j.Store.HasNAV(ctx, f.Code, target)
		if err != nil || !has {
			return true
		}
	}
	return false
}

// Loop is the daily scheduler. It wakes at least every Tick (default 1 minute) and
// re-reads the wall clock each time, so a clock step (a Pi without an RTC syncing
// after boot) or a restart inside the run window cannot skip or delay the run.
// It returns nil once ctx is done.
func (j *Job) Loop(ctx context.Context) error {
	var lastRun time.Time
	logged := false
	for {
		now := j.now()
		today := bkkDate(now)
		if !today.Equal(lastRun) && j.due(ctx, now) {
			lastRun, logged = today, false
			if err := j.RunDaily(ctx, today); err != nil && ctx.Err() == nil {
				j.log(slog.LevelError, "daily run failed", "date", today.Format("2006-01-02"), "err", err)
			}
		} else if !logged {
			logged = true
			j.log(slog.LevelInfo, "next run", "at", NextRun(now, j.Cfg.RunAt).Format(time.RFC3339))
		}
		j.yearEndReminder(ctx, now)
		if err := j.sleep(ctx, j.tick()); err != nil {
			return nil
		}
	}
}

// ---- formatting ----

func money(v float64) string {
	s := strconv.FormatFloat(v, 'f', 0, 64)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// amountStr keeps up to 2 decimals only when needed.
func amountStr(v float64) string {
	if v == float64(int64(v)) {
		return money(v)
	}
	return money(float64(int64(v))) + strconv.FormatFloat(v-float64(int64(v)), 'f', 2, 64)[1:]
}

func isTax(t config.FundType) bool {
	return t == config.RMF || t == config.SSF || t == config.ThaiESG
}

func label(t config.FundType) string { return strings.ToUpper(string(t)) }

// allPaid merges fund purchases (/paid) with manually reported totals (/deduct)
// into one amount per deduction key.
func (j *Job) allPaid(ctx context.Context, year int) (map[config.FundType]float64, error) {
	paid, err := j.Store.PaidByType(ctx, year)
	if err != nil {
		return nil, err
	}
	manual, err := j.Store.Deductions(ctx, year)
	if err != nil {
		return nil, err
	}
	for k, v := range manual {
		paid[k] += v
	}
	return paid, nil
}

// ---- ingest ----

// backfillWeekdays is how many weekdays before the run date are re-checked for gaps.
const backfillWeekdays = 5

// prevWeekdays returns up to n weekdays before date, newest first.
func prevWeekdays(date time.Time, n int) []time.Time {
	var out []time.Time
	y, m, d := date.Date()
	for i := 1; len(out) < n; i++ {
		t := time.Date(y, m, d-i, 0, 0, 0, 0, bangkok)
		if wd := t.Weekday(); wd != time.Saturday && wd != time.Sunday {
			out = append(out, t)
		}
	}
	return out
}

// lazyText fetches the context text at most once, and only when first needed.
type lazyText struct {
	fn   func(ctx context.Context) string
	done bool
	text string
}

func (l *lazyText) get(ctx context.Context) string {
	if !l.done {
		l.done = true
		if l.fn != nil {
			l.text = l.fn(ctx)
		}
	}
	return l.text
}

// RunOnce ingests `date` (a Bangkok calendar date); missing lists funds whose NAV is not yet
// published for that date. It also backfills the last few weekdays still absent from the
// store, and ingests BENCHMARKS that are not tracked funds; those are best-effort only
// (never reported in missing, never an error, never alerted).
func (j *Job) RunOnce(ctx context.Context, date time.Time) (missing []string, err error) {
	date = bkkDate(date)
	var errs []error
	text := &lazyText{fn: j.Context}
	older := prevWeekdays(date, backfillWeekdays)
	for _, f := range j.Cfg.Funds {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		j.backfill(ctx, f.Code, older)
		ok, e := j.ingest(ctx, f.Code, date)
		if e != nil {
			errs = append(errs, fmt.Errorf("fund %s: %w", f.Code, e))
			continue
		}
		if !ok {
			missing = append(missing, f.Code)
			continue
		}
		if e := j.evaluate(ctx, f, date, text); e != nil {
			errs = append(errs, fmt.Errorf("fund %s: %w", f.Code, e))
		}
	}
	for _, code := range j.extraBenchmarks() {
		if ctx.Err() != nil {
			break
		}
		j.backfill(ctx, code, older)
		_, _ = j.ingest(ctx, code, date) // best-effort; ingest already logs the outcome
	}
	return missing, errors.Join(errs...)
}

// extraBenchmarks are BENCHMARKS codes that are not tracked funds.
func (j *Job) extraBenchmarks() []string {
	var out []string
	for _, b := range j.Cfg.Benchmarks {
		tracked := false
		for _, f := range j.Cfg.Funds {
			if strings.EqualFold(f.Code, b) {
				tracked = true
				break
			}
		}
		if !tracked && !slices.Contains(out, b) {
			out = append(out, b)
		}
	}
	return out
}

// backfill fetches earlier days that are absent from the store; failures are non-fatal.
func (j *Job) backfill(ctx context.Context, code string, days []time.Time) {
	for _, d := range days {
		if ctx.Err() != nil {
			return
		}
		_, _ = j.ingest(ctx, code, d)
	}
}

func (j *Job) ingest(ctx context.Context, code string, date time.Time) (bool, error) {
	day := date.Format("2006-01-02")
	has, err := j.Store.HasNAV(ctx, code, date)
	if err != nil {
		j.log(slog.LevelWarn, "nav check failed", "fund", code, "date", day, "err", err)
		return false, err
	}
	if has {
		return true, nil
	}
	nav, ok, err := j.SEC.DailyNAV(ctx, code, date)
	if err != nil {
		j.log(slog.LevelWarn, "nav fetch failed", "fund", code, "date", day, "err", err)
		return false, err
	}
	if !ok {
		j.log(slog.LevelInfo, "nav not published", "fund", code, "date", day)
		return false, nil
	}
	if err := j.Store.UpsertNAV(ctx, code, date, nav); err != nil {
		j.log(slog.LevelWarn, "nav store failed", "fund", code, "date", day, "err", err)
		return false, err
	}
	j.log(slog.LevelInfo, "nav found", "fund", code, "date", day, "nav", nav)
	return true, nil
}

func (j *Job) evaluate(ctx context.Context, f config.Fund, date time.Time, text *lazyText) error {
	hist, err := j.Store.History(ctx, f.Code)
	if err != nil {
		return err
	}
	peak, ok := dip.Peak(hist, j.Cfg.PeakWindowDays)
	if !ok {
		return nil
	}
	latest := hist[len(hist)-1]
	drop := dip.DropPct(peak.NAV, latest.NAV)
	level := dip.Level(drop, j.Cfg.DropTriggers)
	last, err := j.Store.LastLevel(ctx, f.Code)
	if err != nil {
		return err
	}
	if level == 0 {
		if last != 0 {
			return j.Store.SetLevel(ctx, f.Code, 0)
		}
		return nil
	}
	if !dip.ShouldAlert(level, last) {
		return nil
	}

	// Facts for the LLM: public market data only, never income or amounts.
	ctxText := text.get(ctx)
	facts := fmt.Sprintf("fund: %s\ntype: %s\nlatest NAV: %.4f on %s\npeak NAV: %.4f on %s\ndrop from peak: %.1f%%\npeak window: %d trading days\n",
		f.Code, f.Type, latest.NAV, latest.Date.Format("2006-01-02"), peak.NAV, peak.Date.Format("2006-01-02"), drop, j.Cfg.PeakWindowDays)
	if strings.TrimSpace(ctxText) != "" {
		// The closing tag is stripped so fetched text cannot break out of the delimiters.
		ctxText = strings.ReplaceAll(ctxText, "</untrusted_context>", "")
		facts += "context:\n<untrusted_context>\n" + ctxText + "\n</untrusted_context>\n"
	}
	var narrative string
	if j.Advisor != nil {
		narrative, err = j.Advisor.Summarize(ctx, facts)
		narrative = strings.TrimSpace(narrative)
		if err != nil {
			j.log(slog.LevelWarn, "advisor failed", "fund", f.Code, "err", err)
			narrative = ""
		}
	}
	if narrative == "" {
		narrative = "ไม่มีบทสรุป (LLM ใช้ไม่ได้)"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s ลดลง %.1f%% จากจุดสูงสุด (ระดับ %g%%)\nNAV ล่าสุด %.4f (%s) | สูงสุด %.4f (%s)\n\n%s\n",
		f.Code, drop, level, latest.NAV, latest.Date.Format("2006-01-02"), peak.NAV, peak.Date.Format("2006-01-02"), narrative)
	if isTax(f.Type) {
		b.WriteString("\n")
		b.WriteString(j.planLines(ctx, f.Type, date.Year()))
	}
	if err := j.Notify.Send(ctx, strings.TrimRight(b.String(), "\n")); err != nil {
		return fmt.Errorf("send alert: %w", err)
	}
	return j.Store.SetLevel(ctx, f.Code, level)
}

func (j *Job) planLines(ctx context.Context, t config.FundType, year int) string {
	paid, err := j.allPaid(ctx, year)
	if err != nil {
		return "แผนลดหย่อน: คำนวณไม่ได้ (อ่านยอดซื้อไม่สำเร็จ)"
	}
	p, ok := plan.Build(j.Rules, j.Cfg.AnnualNetIncome, j.Cfg.AnnualGrossIncome, paid, t)
	if !ok {
		return fmt.Sprintf("ลดหย่อน %s: สิทธิ์เต็มแล้วในปี %d", label(t), year)
	}
	return fmt.Sprintf("ลดหย่อน %s ปี %d\nซื้อเพิ่มได้ %s บาท (สิทธิ์คงเหลือ)\nประหยัดภาษีโดยประมาณ %s บาท",
		label(t), year, money(p.Suggested), money(p.TaxSaved))
}

// RunDaily is the run for runDay: it retries RunOnce for that day's NAV date every
// RetryInterval until nothing is missing or RetryUntil passes.
func (j *Job) RunDaily(ctx context.Context, runDay time.Time) error {
	runDay = bkkDate(runDay)
	if !runsOn(runDay, j.Cfg.RunAt) {
		return nil
	}
	date := NAVDate(runDay, j.Cfg.RunAt)
	deadline := at(runDay, j.Cfg.RetryUntil)
	var missing []string
	var err error
	for {
		missing, err = j.RunOnce(ctx, date)
		if len(missing) == 0 && err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !j.now().Before(deadline) {
			break
		}
		if e := j.sleep(ctx, j.Cfg.RetryInterval); e != nil {
			return e
		}
	}

	var msg string
	switch {
	case err == nil && len(missing) == len(j.Cfg.Funds):
		msg = "ยังไม่พบ NAV ของทุกกองสำหรับวันที่ " + date.Format("2006-01-02") + " (อาจเป็นวันหยุดตลาด)"
	default:
		var parts []string
		if len(missing) > 0 {
			parts = append(parts, "ยังไม่พบ NAV ของ: "+strings.Join(missing, ", "))
		}
		if err != nil {
			parts = append(parts, "เกิดข้อผิดพลาดระหว่างประมวลผล (ดู log)")
		}
		msg = "แจ้งเตือน " + date.Format("2006-01-02") + ": " + strings.Join(parts, " | ")
	}
	if e := j.Notify.Send(ctx, msg); e != nil {
		err = errors.Join(err, fmt.Errorf("send notice: %w", e))
	}
	return err
}

// ---- year-end reminder ----

// ReminderDue reports whether a year-end reminder falls on day: from the start
// date (month/day) every 7 days, and daily during the last 5 days of the year.
// month 0 disables it.
func ReminderDue(day time.Time, month, dayOfMonth int) bool {
	if month == 0 {
		return false
	}
	d := bkkDate(day)
	start := time.Date(d.Year(), time.Month(month), dayOfMonth, 0, 0, 0, 0, bangkok)
	if d.Before(start) {
		return false
	}
	lastDay := time.Date(d.Year(), time.December, 31, 0, 0, 0, 0, bangkok)
	if int(lastDay.Sub(d).Hours()/24+0.5) < 5 {
		return true
	}
	return int(d.Sub(start).Hours()/24+0.5)%7 == 0
}

// yearEndReminder sends the "use your remaining deduction room" message at most
// once per day, once RUN_AT has passed. Failures are logged and retried next tick.
func (j *Job) yearEndReminder(ctx context.Context, now time.Time) {
	d := bkkDate(now)
	if now.Before(at(d, j.Cfg.RunAt)) || !ReminderDue(d, j.Cfg.YearEndMonth, j.Cfg.YearEndDay) {
		return
	}
	sent, err := j.Store.ReminderSent(ctx, d)
	if err != nil {
		j.log(slog.LevelWarn, "reminder check failed", "err", err)
		return
	}
	if sent {
		return
	}
	msg := j.yearEndMessage(ctx, d)
	if msg != "" {
		if err := j.Notify.Send(ctx, msg); err != nil {
			j.log(slog.LevelWarn, "year-end reminder send failed", "err", err)
			return
		}
		j.log(slog.LevelInfo, "year-end reminder sent")
	}
	// Marked even when nothing was left to say, so the check is not repeated every tick.
	if err := j.Store.MarkReminderSent(ctx, d); err != nil {
		j.log(slog.LevelWarn, "reminder mark failed", "err", err)
	}
}

// yearEndMessage lists the room left for each tracked tax fund type, or "" when
// every tracked type is full (or none is tracked).
func (j *Job) yearEndMessage(ctx context.Context, d time.Time) string {
	paid, err := j.allPaid(ctx, d.Year())
	if err != nil {
		return ""
	}
	last := time.Date(d.Year(), time.December, 31, 0, 0, 0, 0, bangkok)
	daysLeft := int(last.Sub(d).Hours()/24 + 0.5)
	seen := map[config.FundType]bool{}
	var b strings.Builder
	for _, f := range j.Cfg.Funds {
		if !isTax(f.Type) || seen[f.Type] {
			continue
		}
		seen[f.Type] = true
		if _, ok := plan.Build(j.Rules, j.Cfg.AnnualNetIncome, j.Cfg.AnnualGrossIncome, paid, f.Type); !ok {
			continue
		}
		var codes []string
		for _, g := range j.Cfg.Funds {
			if g.Type == f.Type {
				codes = append(codes, g.Code)
			}
		}
		fmt.Fprintf(&b, "\n%s (%s)\n%s\n", label(f.Type), strings.Join(codes, ", "), j.planLines(ctx, f.Type, d.Year()))
	}
	if b.Len() == 0 {
		return ""
	}
	head := fmt.Sprintf("เตือนปลายปี %d: เหลืออีก %d วัน ยังใช้สิทธิ์ลดหย่อนไม่เต็ม", d.Year(), daysLeft)
	tail := "\nรายการด้านบนใช้สิทธิ์ร่วมกันในเพดานรวม จึงรวมซื้อทุกกองไม่ได้เกินสิทธิ์คงเหลือของกลุ่ม" +
		"\nซื้อให้ทันก่อนวันทำการสุดท้ายของปี (รายการซื้อต้องอนุมัติภายในปีภาษี) ตรวจเงื่อนไขกับ บลจ. ก่อนซื้อ" +
		"\nถ้ามียอด PVD/ประกันบำนาญ ใช้ /deduct บันทึกให้ครบเพื่อให้ตัวเลขตรง"
	return head + "\n" + strings.TrimRight(b.String(), "\n") + "\n" + tail
}

// ---- commands ----

const help = "คำสั่งที่ใช้ได้:\n/paid <กอง> <จำนวนเงิน> บันทึกยอดซื้อกองลดหย่อนภาษี\n/deduct <รายการ> <ยอดสะสมทั้งปี> บันทึกค่าลดหย่อนอื่น เช่น PVD, ประกันบำนาญ (0 = ลบ, /deduct เฉยๆ ดูรายการ)\n/tax สรุปสิทธิ์ลดหย่อนปีนี้"

func commandName(text string) string {
	f := strings.Fields(text)
	if len(f) == 0 {
		return ""
	}
	c := f[0]
	if i := strings.IndexByte(c, '@'); i >= 0 {
		c = c[:i]
	}
	return strings.ToLower(c)
}

// HandleCommand serves /paid and /tax from Telegram. Replies are Thai.
func (j *Job) HandleCommand(ctx context.Context, text string) string {
	switch commandName(text) {
	case "/paid":
		return j.paid(ctx, text)
	case "/deduct":
		return j.deduct(ctx, text)
	case "/tax":
		return j.taxSummary(ctx)
	}
	return help
}

func (j *Job) paid(ctx context.Context, text string) string {
	name, amount, err := telegram.ParsePaid(text)
	if err != nil {
		return "รูปแบบไม่ถูกต้อง ใช้: /paid <กอง> <จำนวนเงิน> เช่น /paid ABC-RMF 10000 (จำนวนเงินต้องเป็นตัวเลขมากกว่า 0)"
	}
	var fund *config.Fund
	for i := range j.Cfg.Funds {
		if strings.EqualFold(j.Cfg.Funds[i].Code, name) {
			fund = &j.Cfg.Funds[i]
			break
		}
	}
	if fund == nil {
		return fmt.Sprintf("ไม่พบกอง %s ในรายการที่ติดตาม ไม่ได้บันทึกอะไร", name)
	}
	if !isTax(fund.Type) {
		return fmt.Sprintf("%s เป็นกองทั่วไป ไม่ใช้สิทธิ์ลดหย่อนภาษี จึงไม่ได้บันทึก", fund.Code)
	}
	on := bkkDate(j.now())
	if err := j.Store.AddPaid(ctx, fund.Code, fund.Type, amount, on); err != nil {
		return "บันทึกไม่สำเร็จ ลองใหม่อีกครั้ง"
	}
	paid, err := j.allPaid(ctx, on.Year())
	if err != nil {
		return fmt.Sprintf("บันทึก %s %s บาท แล้ว (อ่านยอดรวมไม่สำเร็จ)", fund.Code, amountStr(amount))
	}
	byType, _ := plan.SavedSoFar(j.Rules, j.Cfg.AnnualNetIncome, paid)
	return fmt.Sprintf("บันทึก %s %s บาท แล้ว (บันทึกซ้ำจะนับเพิ่ม)\n%s ปี %d: ซื้อแล้ว %s | สิทธิ์คงเหลือ %s | ประหยัดภาษีโดยประมาณ %s บาท",
		fund.Code, amountStr(amount), label(fund.Type), on.Year(),
		money(paid[fund.Type]), money(j.Rules.RemainingRoom(fund.Type, paid, j.Cfg.AnnualGrossIncome)),
		money(byType[fund.Type]))
}

func (j *Job) deduct(ctx context.Context, text string) string {
	year := bkkDate(j.now()).Year()
	if len(strings.Fields(text)) == 1 {
		return j.deductList(ctx, year)
	}
	key, amount, err := telegram.ParseDeduct(text)
	if err != nil {
		return "รูปแบบไม่ถูกต้อง ใช้: /deduct <รายการ> <ยอดสะสมทั้งปี> เช่น /deduct pvd 60000 (0 = ลบ) พิมพ์ /deduct เฉยๆ เพื่อดูรายการ"
	}
	k := config.FundType(key)
	switch {
	case isTax(k):
		return fmt.Sprintf("%s ซื้อผ่านกองที่ติดตาม ใช้ /paid <กอง> <จำนวนเงิน> แทน", label(k))
	case !j.Rules.IsManual(k):
		return fmt.Sprintf("ไม่รู้จักรายการ %s ไม่ได้บันทึกอะไร\n%s", key, j.deductList(ctx, year))
	}
	if err := j.Store.SetDeduction(ctx, year, k, amount); err != nil {
		return "บันทึกไม่สำเร็จ ลองใหม่อีกครั้ง"
	}
	verb := "ตั้งยอดสะสม"
	if amount == 0 {
		verb = "ลบยอด"
	}
	head := fmt.Sprintf("%s %s ปี %d: %s บาท แล้ว (ค่าเดิมถูกแทนที่)", verb, j.Rules.Label(k), year, amountStr(amount))
	paid, err := j.allPaid(ctx, year)
	if err != nil {
		return head + " (อ่านยอดรวมไม่สำเร็จ)"
	}
	return head + "\n" + j.roomLines(paid, k)
}

// roomLines shows the used / cap / remaining line for t and, when t shares the
// combined cap, the combined position.
func (j *Job) roomLines(paid map[config.FundType]float64, t config.FundType) string {
	gross := j.Cfg.AnnualGrossIncome
	out := fmt.Sprintf("%s: ใช้แล้ว %s / เพดาน %s | คงเหลือ %s", j.Rules.Label(t),
		money(paid[t]), money(j.Rules.ItemCap(t, gross)), money(j.Rules.RemainingRoom(t, paid, gross)))
	if slices.Contains(j.Rules.Combined.Types, t) {
		used := j.Rules.CombinedUsed(paid)
		out += fmt.Sprintf("\nกลุ่มเพดานรวม: ใช้แล้ว %s / %s | คงเหลือ %s",
			money(used), money(j.Rules.Combined.Cap), money(max(j.Rules.Combined.Cap-used, 0)))
	}
	return out
}

func (j *Job) deductList(ctx context.Context, year int) string {
	manual, err := j.Store.Deductions(ctx, year)
	if err != nil {
		return "อ่านข้อมูลไม่สำเร็จ ลองใหม่อีกครั้ง"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "รายการที่บันทึกเองได้ (ยอดสะสมปี %d):\n", year)
	for _, m := range j.Rules.Manual {
		fmt.Fprintf(&b, "%s (%s): %s บาท\n", m.Key, m.Label, money(manual[m.Key]))
	}
	b.WriteString("ตัวอย่าง: /deduct pvd 60000 (ยอดสะสมทั้งปี ไม่ใช่ยอดที่เพิ่ม). RMF/SSF/Thai ESG ใช้ /paid")
	return b.String()
}

func (j *Job) taxSummary(ctx context.Context) string {
	year := bkkDate(j.now()).Year()
	paid, err := j.allPaid(ctx, year)
	if err != nil {
		return "อ่านข้อมูลไม่สำเร็จ ลองใหม่อีกครั้ง"
	}
	seen := map[config.FundType]bool{}
	var keys []config.FundType
	for _, f := range j.Cfg.Funds {
		if isTax(f.Type) && !seen[f.Type] {
			seen[f.Type] = true
			keys = append(keys, f.Type)
		}
	}
	for _, m := range j.Rules.Manual {
		if paid[m.Key] > 0 {
			keys = append(keys, m.Key)
		}
	}
	if len(keys) == 0 {
		return "ไม่มีกองลดหย่อนภาษีในรายการที่ติดตาม และยังไม่มียอดที่บันทึกเอง"
	}
	gross := j.Cfg.AnnualGrossIncome
	var b strings.Builder
	fmt.Fprintf(&b, "สรุปสิทธิ์ลดหย่อนปี %d\n", year)
	byType, total := plan.SavedSoFar(j.Rules, j.Cfg.AnnualNetIncome, paid)
	for _, t := range keys {
		fmt.Fprintf(&b, "%s: ใช้แล้ว %s / เพดาน %s | คงเหลือ %s | ประหยัดภาษีโดยประมาณ %s บาท\n",
			j.Rules.Label(t), money(paid[t]), money(j.Rules.ItemCap(t, gross)),
			money(j.Rules.RemainingRoom(t, paid, gross)), money(byType[t]))
	}
	if used := j.Rules.CombinedUsed(paid); len(j.Rules.Combined.Types) > 0 {
		fmt.Fprintf(&b, "กลุ่มเพดานรวม: ใช้แล้ว %s / %s | คงเหลือ %s\n",
			money(used), money(j.Rules.Combined.Cap), money(max(j.Rules.Combined.Cap-used, 0)))
	}
	if gross <= 0 {
		b.WriteString("หมายเหตุ: ยังไม่ได้ตั้ง ANNUAL_GROSS_INCOME จึงยังไม่คิดเพดาน % ของรายได้\n")
	}
	fmt.Fprintf(&b, "รวมประหยัดภาษีโดยประมาณ %s บาท", money(total))
	return b.String()
}
