package tax

import (
	"fmt"
	"os"
	"slices"
	"strings"

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

// Manual is a deduction the user reports by hand (provident fund, pension
// insurance, ...) instead of buying it through a tracked fund.
type Manual struct {
	Key   config.FundType `yaml:"key"`
	Label string          `yaml:"label"`
}

type Rules struct {
	Year     int                         `yaml:"year"`
	Brackets []Bracket                   `yaml:"brackets"`
	Caps     map[config.FundType]float64 `yaml:"caps"`
	// Pct is an optional per-item limit as a fraction of gross annual income
	// (0.3 = 30%). It applies together with Caps; the lower one wins.
	Pct      map[config.FundType]float64 `yaml:"pct"`
	Combined Combined                    `yaml:"combined"`
	Manual   []Manual                    `yaml:"manual"`
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
	if err := r.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &r, nil
}

func fundDeductible(t config.FundType) bool {
	return t == config.RMF || t == config.SSF || t == config.ThaiESG
}

// IsManual reports whether t is a user-reported deduction key.
func (r *Rules) IsManual(t config.FundType) bool {
	return slices.ContainsFunc(r.Manual, func(m Manual) bool { return m.Key == t })
}

func (r *Rules) deductibleType(t config.FundType) bool { return fundDeductible(t) || r.IsManual(t) }

// Deductions lists every deduction key in fixed order: rmf, ssf, thaiesg, then
// the manual keys in file order.
func (r *Rules) Deductions() []config.FundType {
	out := []config.FundType{config.RMF, config.SSF, config.ThaiESG}
	for _, m := range r.Manual {
		out = append(out, m.Key)
	}
	return out
}

// Label is the display name of a deduction key.
func (r *Rules) Label(t config.FundType) string {
	for _, m := range r.Manual {
		if m.Key == t && m.Label != "" {
			return m.Label
		}
	}
	return strings.ToUpper(string(t))
}

func (r *Rules) validate() error {
	seen := map[config.FundType]bool{}
	for i, m := range r.Manual {
		switch {
		case m.Key == "" || m.Key != config.FundType(strings.ToLower(string(m.Key))) || strings.ContainsAny(string(m.Key), " \t"):
			return fmt.Errorf("manual %d: key %q must be non-empty, lower-case, without spaces", i+1, m.Key)
		case fundDeductible(m.Key) || m.Key == config.Ordinary:
			return fmt.Errorf("manual %d: key %q is reserved for funds (record it with /paid)", i+1, m.Key)
		case seen[m.Key]:
			return fmt.Errorf("manual %d: duplicate key %q", i+1, m.Key)
		}
		seen[m.Key] = true
	}
	var prev float64
	for i, b := range r.Brackets {
		if b.Rate < 0 || b.Rate > 1 {
			return fmt.Errorf("bracket %d: rate %v must be between 0 and 1", i+1, b.Rate)
		}
		last := i == len(r.Brackets)-1
		switch {
		case last && b.UpTo != 0:
			return fmt.Errorf("last bracket must be open-ended (up_to: 0), got up_to %v", b.UpTo)
		case !last && b.UpTo <= prev:
			return fmt.Errorf("bracket %d: up_to values must be strictly ascending and only the last may be 0", i+1)
		}
		prev = b.UpTo
	}
	for t := range r.Caps {
		if !r.deductibleType(t) {
			return fmt.Errorf("caps: unknown fund type %q (use rmf, ssf, thaiesg or a manual key, lower-case)", t)
		}
	}
	for t, p := range r.Pct {
		if !r.deductibleType(t) {
			return fmt.Errorf("pct: unknown fund type %q (use rmf, ssf, thaiesg or a manual key, lower-case)", t)
		}
		if p <= 0 || p > 1 {
			return fmt.Errorf("pct: %q must be a fraction between 0 and 1 (0.3 = 30%%), got %v", t, p)
		}
	}
	for _, t := range r.Combined.Types {
		if !r.deductibleType(t) {
			return fmt.Errorf("combined.types: unknown fund type %q (use rmf, ssf, thaiesg or a manual key, lower-case)", t)
		}
	}
	return nil
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

// ItemCap is the yearly limit for t: its baht cap, lowered to its share of
// grossIncome when a pct limit is set and grossIncome is known (> 0).
func (r *Rules) ItemCap(t config.FundType, grossIncome float64) float64 {
	cap, ok := r.Caps[t]
	if !ok {
		return 0
	}
	if p, ok := r.Pct[t]; ok && grossIncome > 0 {
		cap = min(cap, p*grossIncome)
	}
	return cap
}

// Deductible is what each recorded amount is worth after the limits: each item is
// capped at ItemCap, then items sharing the combined cap are allocated in
// Deductions() order until it runs out. Keys without a cap entry count in full.
func (r *Rules) Deductible(paid map[config.FundType]float64, grossIncome float64) map[config.FundType]float64 {
	out := map[config.FundType]float64{}
	groupLeft := r.Combined.Cap
	keys := r.Deductions()
	for t := range paid {
		if !slices.Contains(keys, t) {
			keys = append(keys, t) // unknown key: counted as recorded
		}
	}
	for _, t := range keys {
		v := paid[t]
		if _, ok := r.Caps[t]; ok {
			v = min(v, r.ItemCap(t, grossIncome))
		}
		if slices.Contains(r.Combined.Types, t) {
			v = min(v, max(groupLeft, 0))
			groupLeft -= v
		}
		if v != 0 {
			out[t] = v
		}
	}
	return out
}

// CombinedUsed is how much of the shared cap the recorded amounts take, after the limits.
func (r *Rules) CombinedUsed(paid map[config.FundType]float64, grossIncome float64) float64 {
	ded := r.Deductible(paid, grossIncome)
	var used float64
	for _, ct := range r.Combined.Types {
		used += ded[ct]
	}
	return used
}

// RemainingRoom is how much more can be deducted for deduction key t this year.
// grossIncome (0 = unknown) enables the percentage-of-income limits.
func (r *Rules) RemainingRoom(t config.FundType, paid map[config.FundType]float64, grossIncome float64) float64 {
	if _, ok := r.Caps[t]; !ok {
		return 0
	}
	room := r.ItemCap(t, grossIncome) - paid[t]
	if slices.Contains(r.Combined.Types, t) {
		room = min(room, r.Combined.Cap-r.CombinedUsed(paid, grossIncome))
	}
	return max(room, 0)
}
