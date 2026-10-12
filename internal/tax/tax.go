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
	if err := r.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &r, nil
}

func deductibleType(t config.FundType) bool {
	return t == config.RMF || t == config.SSF || t == config.ThaiESG
}

func (r *Rules) validate() error {
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
		if !deductibleType(t) {
			return fmt.Errorf("caps: unknown fund type %q (use rmf, ssf or thaiesg, lower-case)", t)
		}
	}
	for _, t := range r.Combined.Types {
		if !deductibleType(t) {
			return fmt.Errorf("combined.types: unknown fund type %q (use rmf, ssf or thaiesg, lower-case)", t)
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
