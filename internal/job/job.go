// Package job orchestrates the daily NAV ingest, dip alerts and Telegram commands.
package job

import (
	"context"
	"errors"
	"fmt"
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

// ---- ingest ----

// RunOnce ingests `date` (a Bangkok calendar date); missing lists funds whose NAV is not yet published.
func (j *Job) RunOnce(ctx context.Context, date time.Time) (missing []string, err error) {
	date = bkkDate(date)
	var errs []error
	for _, f := range j.Cfg.Funds {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		ok, e := j.ingest(ctx, f, date)
		if e != nil {
			errs = append(errs, fmt.Errorf("fund %s: %w", f.Code, e))
			continue
		}
		if !ok {
			missing = append(missing, f.Code)
			continue
		}
		if e := j.evaluate(ctx, f, date); e != nil {
			errs = append(errs, fmt.Errorf("fund %s: %w", f.Code, e))
		}
	}
	return missing, errors.Join(errs...)
}

func (j *Job) ingest(ctx context.Context, f config.Fund, date time.Time) (bool, error) {
	has, err := j.Store.HasNAV(ctx, f.Code, date)
	if err != nil {
		return false, err
	}
	if has {
		return true, nil
	}
	nav, ok, err := j.SEC.DailyNAV(ctx, f.Code, date)
	if err != nil || !ok {
		return false, err
	}
	return true, j.Store.UpsertNAV(ctx, f.Code, date, nav)
}

func (j *Job) evaluate(ctx context.Context, f config.Fund, date time.Time) error {
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
	var ctxText string
	if j.Context != nil {
		ctxText = j.Context(ctx)
	}
	facts := fmt.Sprintf("fund: %s\ntype: %s\nlatest NAV: %.4f on %s\npeak NAV: %.4f on %s\ndrop from peak: %.1f%%\npeak window: %d trading days\n",
		f.Code, f.Type, latest.NAV, latest.Date.Format("2006-01-02"), peak.NAV, peak.Date.Format("2006-01-02"), drop, j.Cfg.PeakWindowDays)
	if strings.TrimSpace(ctxText) != "" {
		facts += "context:\n" + ctxText + "\n"
	}
	var narrative string
	if j.Advisor != nil {
		narrative, err = j.Advisor.Summarize(ctx, facts)
		narrative = strings.TrimSpace(narrative)
		if err != nil {
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
	paid, err := j.Store.PaidByType(ctx, year)
	if err != nil {
		return "แผนลดหย่อน: คำนวณไม่ได้ (อ่านยอดซื้อไม่สำเร็จ)"
	}
	p, ok := plan.Build(j.Rules, j.Cfg.AnnualNetIncome, paid, t)
	if !ok {
		return fmt.Sprintf("ลดหย่อน %s: สิทธิ์เต็มแล้วในปี %d", label(t), year)
	}
	return fmt.Sprintf("ลดหย่อน %s ปี %d\nซื้อเพิ่มได้ %s บาท (สิทธิ์คงเหลือ)\nประหยัดภาษีโดยประมาณ %s บาท",
		label(t), year, money(p.Suggested), money(p.TaxSaved))
}

// RunDaily retries RunOnce every RetryInterval until nothing is missing or RetryUntil passes.
func (j *Job) RunDaily(ctx context.Context, date time.Time) error {
	date = bkkDate(date)
	if wd := date.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return nil
	}
	deadline := at(date, j.Cfg.RetryUntil)
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
		msg = "วันนี้ยังไม่พบ NAV ของทุกกอง (อาจเป็นวันหยุดตลาด)"
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

// ---- commands ----

const help = "คำสั่งที่ใช้ได้:\n/paid <กอง> <จำนวนเงิน> บันทึกยอดซื้อกองลดหย่อนภาษี\n/tax สรุปสิทธิ์ลดหย่อนปีนี้"

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
	paid, err := j.Store.PaidByType(ctx, on.Year())
	if err != nil {
		return fmt.Sprintf("บันทึก %s %s บาท แล้ว (อ่านยอดรวมไม่สำเร็จ)", fund.Code, amountStr(amount))
	}
	byType, _ := plan.SavedSoFar(j.Rules, j.Cfg.AnnualNetIncome, paid)
	return fmt.Sprintf("บันทึก %s %s บาท แล้ว (บันทึกซ้ำจะนับเพิ่ม)\n%s ปี %d: ซื้อแล้ว %s | สิทธิ์คงเหลือ %s | ประหยัดภาษีโดยประมาณ %s บาท",
		fund.Code, amountStr(amount), label(fund.Type), on.Year(),
		money(paid[fund.Type]), money(j.Rules.RemainingRoom(fund.Type, paid)),
		money(byType[fund.Type]))
}

func (j *Job) taxSummary(ctx context.Context) string {
	year := bkkDate(j.now()).Year()
	paid, err := j.Store.PaidByType(ctx, year)
	if err != nil {
		return "อ่านข้อมูลไม่สำเร็จ ลองใหม่อีกครั้ง"
	}
	seen := map[config.FundType]bool{}
	var types []config.FundType
	for _, f := range j.Cfg.Funds {
		if isTax(f.Type) && !seen[f.Type] {
			seen[f.Type] = true
			types = append(types, f.Type)
		}
	}
	if len(types) == 0 {
		return "ไม่มีกองลดหย่อนภาษีในรายการที่ติดตาม"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "สรุปสิทธิ์ลดหย่อนปี %d\n", year)
	byType, total := plan.SavedSoFar(j.Rules, j.Cfg.AnnualNetIncome, paid)
	for _, t := range types {
		fmt.Fprintf(&b, "%s: ซื้อแล้ว %s / เพดาน %s | คงเหลือ %s | ประหยัดภาษีโดยประมาณ %s บาท\n",
			label(t), money(paid[t]), money(j.Rules.Caps[t]), money(j.Rules.RemainingRoom(t, paid)),
			money(byType[t]))
	}
	fmt.Fprintf(&b, "รวมประหยัดภาษีโดยประมาณ %s บาท (แยกตามลำดับ RMF, SSF, Thai ESG)", money(total))
	return b.String()
}
