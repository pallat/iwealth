package plan

import (
	"github.com/pallat/iwealth/internal/config"
	"github.com/pallat/iwealth/internal/tax"
)

type Plan struct{ Room, Suggested, TaxSaved float64 }

// Build returns ok=false for funds with no deduction (ordinary) or no room left.
func Build(r *tax.Rules, netIncome float64, paid map[config.FundType]float64, t config.FundType) (Plan, bool) {
	room := r.RemainingRoom(t, paid)
	if room <= 0 {
		return Plan{}, false
	}
	return Plan{Room: room, Suggested: room, TaxSaved: r.TaxSaved(taxableBase(netIncome, paid), room)}, true
}

// deductible lists the fund types that reduce taxable income, in the fixed
// order used to attribute savings per type.
var deductible = []config.FundType{config.RMF, config.SSF, config.ThaiESG}

// taxableBase is netIncome (before fund deductions) minus everything already
// bought in deductible funds, never below zero.
func taxableBase(netIncome float64, paid map[config.FundType]float64) float64 {
	base := netIncome
	for _, t := range deductible {
		base -= paid[t]
	}
	return max(base, 0)
}

// SavedSoFar is the tax saved by purchases already recorded: Tax(netIncome) -
// Tax(netIncome - sum(paid)). The per-type split is marginal in the order
// rmf, ssf, thaiesg so the parts sum to total.
func SavedSoFar(r *tax.Rules, netIncome float64, paid map[config.FundType]float64) (byType map[config.FundType]float64, total float64) {
	byType = map[config.FundType]float64{}
	cur := max(netIncome, 0)
	prev := r.Tax(cur)
	for _, t := range deductible {
		cur = max(cur-paid[t], 0)
		next := r.Tax(cur)
		byType[t] = prev - next
		total += prev - next
		prev = next
	}
	return byType, total
}
