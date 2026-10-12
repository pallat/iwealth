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
	AnnualGrossIncome                     float64 // gross yearly income; net income is derived from it
	ExpenseKind                           string  // salary (default), none or fixed
	ExpenseFixed                          float64
	RunAt, RetryUntil                     string
	YearEndMonth, YearEndDay              int // year-end reminder start (month 0 = off)
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
		RunAt:         or(getenv("RUN_AT"), "04:00"),
		RetryUntil:    or(getenv("RETRY_UNTIL"), "08:00"),
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
	if c.YearEndMonth, c.YearEndDay, err = parseMonthDay(or(getenv("YEAR_END_REMINDER_FROM"), "12-01")); err != nil {
		return nil, fmt.Errorf("YEAR_END_REMINDER_FROM: %w", err)
	}
	if v := getenv("ANNUAL_GROSS_INCOME"); v != "" {
		if c.AnnualGrossIncome, err = strconv.ParseFloat(v, 64); err != nil || c.AnnualGrossIncome < 0 {
			return nil, fmt.Errorf("ANNUAL_GROSS_INCOME must be a non-negative number")
		}
	}
	c.ExpenseKind = "salary"
	if v := strings.ToLower(strings.TrimSpace(getenv("EXPENSE_DEDUCTION"))); v != "" && v != "salary" {
		if v == "none" {
			c.ExpenseKind = "none"
		} else if c.ExpenseFixed, err = strconv.ParseFloat(v, 64); err != nil || c.ExpenseFixed < 0 {
			return nil, fmt.Errorf("EXPENSE_DEDUCTION must be salary, none or a non-negative amount")
		} else {
			c.ExpenseKind = "fixed"
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

// parseMonthDay reads "MM-DD", or "off" (month 0).
func parseMonthDay(s string) (month, day int, err error) {
	if strings.EqualFold(s, "off") {
		return 0, 0, nil
	}
	ms, ds, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, fmt.Errorf("%q must be MM-DD or off", s)
	}
	m, e1 := strconv.Atoi(ms)
	d, e2 := strconv.Atoi(ds)
	if e1 != nil || e2 != nil || m < 1 || m > 12 || d < 1 || d > 31 ||
		time.Date(2024, time.Month(m), d, 0, 0, 0, 0, time.UTC).Day() != d { // 2024 is a leap year
		return 0, 0, fmt.Errorf("%q is not a valid MM-DD date", s)
	}
	return m, d, nil
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

// Validate checks cross-field rules that Load does not enforce on its own: tax-deductible
// funds need an income (ANNUAL_GROSS_INCOME, or the legacy ANNUAL_NET_INCOME), otherwise
// every tax estimate would silently be zero.
func (c *Config) Validate() error {
	if c.AnnualGrossIncome > 0 && c.AnnualNetIncome > c.AnnualGrossIncome {
		return fmt.Errorf("ANNUAL_NET_INCOME must not exceed ANNUAL_GROSS_INCOME")
	}
	if c.AnnualGrossIncome > 0 || c.AnnualNetIncome > 0 {
		return nil
	}
	for _, f := range c.Funds {
		switch f.Type {
		case RMF, SSF, ThaiESG:
			return fmt.Errorf("ANNUAL_GROSS_INCOME is required (and must be greater than 0) because fund %s is a tax fund (%s)", f.Code, f.Type)
		}
	}
	return nil
}
