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
