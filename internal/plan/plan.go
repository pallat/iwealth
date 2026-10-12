package plan

import (
	"slices"

	"github.com/pallat/iwealth/internal/config"
	"github.com/pallat/iwealth/internal/tax"
)

type Plan struct{ Room, Suggested, TaxSaved float64 }

// Build returns ok=false for funds with no deduction (ordinary) or no room left.
func Build(r *tax.Rules, inc tax.Income, paid map[config.FundType]float64, t config.FundType) (Plan, bool) {
	room := r.RemainingRoom(t, paid, inc.Gross)
	if room <= 0 {
		return Plan{}, false
	}
	return Plan{Room: room, Suggested: room, TaxSaved: r.TaxSaved(r.NetTaxable(inc, paid), room)}, true
}

// SavedSoFar is the tax saved by what is recorded: Tax(Base) - Tax(Base - deductible).
// Amounts count only up to their caps (Rules.Deductible). The per-key split is
// marginal in Rules.Deductions() order so the parts sum to total.
func SavedSoFar(r *tax.Rules, inc tax.Income, paid map[config.FundType]float64) (byType map[config.FundType]float64, total float64) {
	byType = map[config.FundType]float64{}
	cur := max(r.Base(inc), 0)
	prev := r.Tax(cur)
	ded := r.Deductible(paid, inc.Gross)
	for _, t := range r.Deductions() {
		cur = max(cur-ded[t], 0)
		next := r.Tax(cur)
		byType[t] = prev - next
		total += prev - next
		prev = next
	}
	return byType, total
}

// Opportunity is a deduction that still has room.
type Opportunity struct {
	Key      config.FundType
	Room     float64
	TaxSaved float64 // saving if the whole room were used on its own
	Invest   bool    // can be bought, as opposed to depending on circumstances
}

// Opportunities lists every capped, non-automatic deduction with room left, largest
// tax saving first. Items in the same group share their room, so the savings do not add up.
func Opportunities(r *tax.Rules, inc tax.Income, paid map[config.FundType]float64) []Opportunity {
	net := r.NetTaxable(inc, paid)
	var out []Opportunity
	for _, t := range r.Deductions() {
		room := r.RemainingRoom(t, paid, inc.Gross)
		if room <= 0 {
			continue
		}
		out = append(out, Opportunity{Key: t, Room: room, TaxSaved: r.TaxSaved(net, room), Invest: r.Invest(t)})
	}
	slices.SortStableFunc(out, func(a, b Opportunity) int {
		switch {
		case a.TaxSaved > b.TaxSaved:
			return -1
		case a.TaxSaved < b.TaxSaved:
			return 1
		}
		return 0
	})
	return out
}
