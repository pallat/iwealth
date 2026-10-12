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

// Group is a set of deductions that share one overall cap.
type Group struct {
	Name  string            `yaml:"name"`
	Label string            `yaml:"label"`
	Cap   float64           `yaml:"cap"`
	Types []config.FundType `yaml:"types"`
}

// Manual is a deduction the user reports by hand (/deduct) instead of buying it
// through a tracked fund. Kind "invest" marks ones you can still buy more of
// (insurance, pension fund); anything else depends on your circumstances.
// Auto items (personal allowance) are applied at their cap without an entry.
type Manual struct {
	Key   config.FundType `yaml:"key"`
	Label string          `yaml:"label"`
	Kind  string          `yaml:"kind"`
	Auto  bool            `yaml:"auto"`
}

// Expense is the standard expense deduction for salary income.
type Expense struct {
	Pct float64 `yaml:"pct"`
	Max float64 `yaml:"max"`
}

type Rules struct {
	Year     int                         `yaml:"year"`
	Brackets []Bracket                   `yaml:"brackets"`
	Caps     map[config.FundType]float64 `yaml:"caps"`
	// Pct is an optional per-item limit as a fraction of gross annual income
	// (0.3 = 30%). It applies together with Caps; the lower one wins.
	Pct     map[config.FundType]float64 `yaml:"pct"`
	Groups  []Group                     `yaml:"groups"`
	Manual  []Manual                    `yaml:"manual"`
	Expense Expense                     `yaml:"expense"`
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

// Invest reports whether more of t can be bought (fund or invest-kind manual item).
func (r *Rules) Invest(t config.FundType) bool {
	if fundDeductible(t) {
		return true
	}
	for _, m := range r.Manual {
		if m.Key == t {
			return m.Kind == "invest"
		}
	}
	return false
}

func (r *Rules) auto(t config.FundType) bool {
	for _, m := range r.Manual {
		if m.Key == t {
			return m.Auto
		}
	}
	return false
}

// IsAuto reports whether t is applied automatically (no /deduct entry needed).
func (r *Rules) IsAuto(t config.FundType) bool { return r.auto(t) }

// GroupOf returns the group t belongs to, or nil.
func (r *Rules) GroupOf(t config.FundType) *Group {
	for i := range r.Groups {
		if slices.Contains(r.Groups[i].Types, t) {
			return &r.Groups[i]
		}
	}
	return nil
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
		if m.Kind != "" && m.Kind != "invest" && m.Kind != "other" {
			return fmt.Errorf("manual %d (%s): kind %q must be invest or other", i+1, m.Key, m.Kind)
		}
		if m.Auto && r.Caps[m.Key] <= 0 {
			return fmt.Errorf("manual %d (%s): auto items need a positive cap", i+1, m.Key)
		}
	}
	if r.Expense.Pct < 0 || r.Expense.Pct > 1 || r.Expense.Max < 0 {
		return fmt.Errorf("expense: pct must be between 0 and 1 and max must not be negative")
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
	inGroup := map[config.FundType]string{}
	names := map[string]bool{}
	for i, g := range r.Groups {
		if g.Name == "" || names[g.Name] {
			return fmt.Errorf("groups %d: name must be unique and non-empty", i+1)
		}
		names[g.Name] = true
		if g.Cap <= 0 {
			return fmt.Errorf("groups %q: cap must be greater than 0", g.Name)
		}
		for _, t := range g.Types {
			switch {
			case !r.deductibleType(t):
				return fmt.Errorf("groups %q: unknown fund type %q (use rmf, ssf, thaiesg or a manual key, lower-case)", g.Name, t)
			case inGroup[t] != "":
				return fmt.Errorf("groups %q: %q is already in group %q", g.Name, t, inGroup[t])
			}
			inGroup[t] = g.Name
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
// capped at ItemCap, then items sharing a group cap are allocated in Deductions()
// order until it runs out. Auto items count at their cap. Keys without a cap
// entry count in full.
func (r *Rules) Deductible(paid map[config.FundType]float64, grossIncome float64) map[config.FundType]float64 {
	out := map[config.FundType]float64{}
	left := map[string]float64{}
	for _, g := range r.Groups {
		left[g.Name] = g.Cap
	}
	keys := r.Deductions()
	for t := range paid {
		if !slices.Contains(keys, t) {
			keys = append(keys, t) // unknown key: counted as recorded
		}
	}
	for _, t := range keys {
		v := paid[t]
		if r.auto(t) {
			v = r.ItemCap(t, grossIncome)
		} else if _, ok := r.Caps[t]; ok {
			v = min(v, r.ItemCap(t, grossIncome))
		}
		if g := r.GroupOf(t); g != nil {
			v = min(v, max(left[g.Name], 0))
			left[g.Name] -= v
		}
		if v != 0 {
			out[t] = v
		}
	}
	return out
}

// GroupUsed is how much of group g's cap the recorded amounts take, after the limits.
func (r *Rules) GroupUsed(g Group, paid map[config.FundType]float64, grossIncome float64) float64 {
	ded := r.Deductible(paid, grossIncome)
	var used float64
	for _, t := range g.Types {
		used += ded[t]
	}
	return used
}

// RemainingRoom is how much more can be deducted for deduction key t this year.
// grossIncome (0 = unknown) enables the percentage-of-income limits.
func (r *Rules) RemainingRoom(t config.FundType, paid map[config.FundType]float64, grossIncome float64) float64 {
	if _, ok := r.Caps[t]; !ok || r.auto(t) {
		return 0
	}
	room := r.ItemCap(t, grossIncome) - paid[t]
	if g := r.GroupOf(t); g != nil {
		room = min(room, g.Cap-r.GroupUsed(*g, paid, grossIncome))
	}
	return max(room, 0)
}

// Income describes the taxpayer's yearly income. With Gross > 0 the net income is
// derived (gross - expense - deductions). With Gross == 0 the legacy LegacyNet is
// used: net income before every deduction iwealth tracks.
type Income struct {
	Gross        float64
	LegacyNet    float64
	Expense      string // "salary" (default), "none" or "fixed"
	ExpenseFixed float64
}

// ExpenseDeduction is the expense deduction taken off Gross before other deductions.
func (r *Rules) ExpenseDeduction(inc Income) float64 {
	if inc.Gross <= 0 {
		return 0
	}
	switch inc.Expense {
	case "none":
		return 0
	case "fixed":
		return min(max(inc.ExpenseFixed, 0), inc.Gross)
	}
	e := inc.Gross * r.Expense.Pct
	if r.Expense.Max > 0 {
		e = min(e, r.Expense.Max)
	}
	return e
}

// Base is the income that recorded deductions come off: gross less expense, or in
// legacy mode LegacyNet (with auto items added back, as LegacyNet already has them out).
func (r *Rules) Base(inc Income) float64 {
	if inc.Gross > 0 {
		return max(inc.Gross-r.ExpenseDeduction(inc), 0)
	}
	b := inc.LegacyNet
	for _, m := range r.Manual {
		if m.Auto {
			b += r.Caps[m.Key]
		}
	}
	return b
}

// NetTaxable is the net taxable income after every counted deduction.
func (r *Rules) NetTaxable(inc Income, paid map[config.FundType]float64) float64 {
	net := r.Base(inc)
	for _, v := range r.Deductible(paid, inc.Gross) {
		net -= v
	}
	return max(net, 0)
}

// Position is the bracket a net income falls in. Upper 0 means no upper limit.
type Position struct {
	Index        int
	Lower, Upper float64
	Rate         float64
	PrevRate     float64 // rate of the bracket below; meaningful when Index > 0
	NextRate     float64 // rate of the bracket above; meaningful when Upper > 0
}

// BracketAt finds the bracket for net (a net of exactly UpTo is still inside it).
func (r *Rules) BracketAt(net float64) Position {
	var lower float64
	for i, b := range r.Brackets {
		if b.UpTo == 0 || net <= b.UpTo {
			p := Position{Index: i, Lower: lower, Upper: b.UpTo, Rate: b.Rate}
			if i > 0 {
				p.PrevRate = r.Brackets[i-1].Rate
			}
			if i+1 < len(r.Brackets) {
				p.NextRate = r.Brackets[i+1].Rate
			}
			return p
		}
		lower = b.UpTo
	}
	return Position{}
}
