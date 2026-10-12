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
	p, ok := Build(rules(), 600000, map[config.FundType]float64{config.SSF: 100000}, config.SSF)
	if !ok || p.Room != 100000 || p.Suggested != 100000 || p.TaxSaved != 20000 {
		t.Fatalf("%+v %v", p, ok)
	}
	if _, ok := Build(rules(), 600000, nil, config.Ordinary); ok {
		t.Fatal("ordinary must have no plan")
	}
	if _, ok := Build(rules(), 600000, map[config.FundType]float64{config.SSF: 200000}, config.SSF); ok {
		t.Fatal("no room must have no plan")
	}
}
