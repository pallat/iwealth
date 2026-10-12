package plan

import (
	"testing"

	"github.com/pallat/iwealth/internal/config"
	"github.com/pallat/iwealth/internal/tax"
)

func rules() *tax.Rules {
	return &tax.Rules{
		Brackets: []tax.Bracket{
			{UpTo: 150000, Rate: 0},
			{UpTo: 300000, Rate: 0.05},
			{UpTo: 500000, Rate: 0.10},
			{UpTo: 0, Rate: 0.20},
		},
		Caps: map[config.FundType]float64{config.SSF: 200000},
	}
}

func TestBuild(t *testing.T) {
	p, ok := Build(rules(), tax.Income{LegacyNet: 600000}, map[config.FundType]float64{config.SSF: 100000}, config.SSF)
	if !ok || p.Room != 100000 || p.Suggested != 100000 || p.TaxSaved != 10000 {
		t.Fatalf("%+v %v", p, ok)
	}
	if _, ok := Build(rules(), tax.Income{LegacyNet: 600000}, nil, config.Ordinary); ok {
		t.Fatal("ordinary must have no plan")
	}
	if _, ok := Build(rules(), tax.Income{LegacyNet: 600000}, map[config.FundType]float64{config.SSF: 200000}, config.SSF); ok {
		t.Fatal("no room must have no plan")
	}
}

func realRules() *tax.Rules {
	return &tax.Rules{
		Brackets: []tax.Bracket{
			{UpTo: 150000, Rate: 0}, {UpTo: 300000, Rate: 0.05}, {UpTo: 500000, Rate: 0.10},
			{UpTo: 750000, Rate: 0.15}, {UpTo: 1000000, Rate: 0.20}, {UpTo: 0, Rate: 0.25},
		},
		Caps:   map[config.FundType]float64{config.RMF: 500000, config.SSF: 200000, config.ThaiESG: 300000},
		Groups: []tax.Group{{Name: "retirement", Label: "r", Cap: 500000, Types: []config.FundType{config.RMF, config.SSF}}},
	}
}

func TestBuildAccountsForRecordedPurchases(t *testing.T) {
	// income 1M before fund deductions, RMF paid 300k, SSF room 200k: Tax(700k)-Tax(500k).
	p, ok := Build(realRules(), tax.Income{LegacyNet: 1000000}, map[config.FundType]float64{config.RMF: 300000}, config.SSF)
	if !ok || p.Room != 200000 || p.TaxSaved != 30000 {
		t.Fatalf("%+v %v", p, ok)
	}
}

func TestSavedSoFar(t *testing.T) {
	r := realRules()
	paid := map[config.FundType]float64{config.RMF: 300000, config.SSF: 100000}
	by, total := SavedSoFar(r, tax.Income{LegacyNet: 1000000}, paid)
	// total = Tax(1M)-Tax(600k); rmf first (marginal at the top), ssf next.
	want := r.Tax(1000000) - r.Tax(600000)
	if total != want {
		t.Fatalf("total %v want %v", total, want)
	}
	if by[config.RMF]+by[config.SSF]+by[config.ThaiESG] != total {
		t.Fatalf("parts do not sum: %v total %v", by, total)
	}
	if by[config.RMF] != r.Tax(1000000)-r.Tax(700000) {
		t.Fatalf("rmf %v", by[config.RMF])
	}
	if _, tot := SavedSoFar(r, tax.Income{LegacyNet: 100000}, paid); tot != 0 {
		t.Fatalf("below threshold %v", tot)
	}
}

func TestOverCapCountsOnlyUpToCap(t *testing.T) {
	r := rules() // brackets 0/5/10/20%, ssf cap 200k
	// 250k bought in SSF counts as 200k: same saving as exactly 200k
	over := map[config.FundType]float64{config.SSF: 250000}
	exact := map[config.FundType]float64{config.SSF: 200000}
	_, a := SavedSoFar(r, tax.Income{LegacyNet: 600000}, over)
	_, b := SavedSoFar(r, tax.Income{LegacyNet: 600000}, exact)
	if a != b || a == 0 {
		t.Fatalf("over-cap saving %v want %v", a, b)
	}
	if got := r.NetTaxable(tax.Income{LegacyNet: 600000}, over); got != 400000 {
		t.Fatalf("base = %v", got)
	}
}
