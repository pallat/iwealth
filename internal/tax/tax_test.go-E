package tax

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pallat/iwealth/internal/config"
)

func rules() *Rules {
	return &Rules{
		Brackets: []Bracket{{150000, 0}, {300000, 0.05}, {500000, 0.10}, {0, 0.20}},
		Caps:     map[config.FundType]float64{config.RMF: 500000, config.SSF: 200000, config.ThaiESG: 300000},
		Combined: Combined{Cap: 500000, Types: []config.FundType{config.RMF, config.SSF}},
	}
}

func TestTax(t *testing.T) {
	r := rules()
	cases := []struct{ in, want float64 }{
		{0, 0}, {150000, 0}, {300000, 7500}, {500000, 27500}, {600000, 47500}, {-5, 0},
	}
	for _, c := range cases {
		if got := r.Tax(c.in); got != c.want {
			t.Errorf("Tax(%v)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestTaxSaved(t *testing.T) {
	r := rules()
	if got := r.TaxSaved(600000, 100000); got != 20000 { // 47500 - 27500
		t.Errorf("got %v", got)
	}
	if got := r.TaxSaved(100000, 50000); got != 0 {
		t.Errorf("below threshold got %v", got)
	}
	if got := r.TaxSaved(100000, 999999); got != 0 { // deduction > income
		t.Errorf("over-deduction got %v", got)
	}
}

func TestRemainingRoom(t *testing.T) {
	r := rules()
	cases := []struct {
		name string
		typ  config.FundType
		paid map[config.FundType]float64
		want float64
	}{
		{"fresh rmf limited by combined", config.RMF, nil, 500000},
		{"ssf own cap", config.SSF, nil, 200000},
		{"rmf with ssf paid, combined binds", config.RMF, map[config.FundType]float64{config.SSF: 200000}, 300000},
		{"thaiesg outside combined", config.ThaiESG, map[config.FundType]float64{config.RMF: 500000}, 300000},
		{"overpaid never negative", config.SSF, map[config.FundType]float64{config.SSF: 250000}, 0},
		{"ordinary has none", config.Ordinary, nil, 0},
	}
	for _, c := range cases {
		if got := r.RemainingRoom(c.typ, c.paid); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func writeRules(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "r.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadRulesValidation(t *testing.T) {
	const ok = "brackets:\n  - {up_to: 100, rate: 0}\n  - {up_to: 0, rate: 0.1}\n"
	if _, err := LoadRules(writeRules(t, ok+"caps: {rmf: 5}\ncombined: {cap: 5, types: [rmf, ssf]}\n")); err != nil {
		t.Fatalf("valid rules rejected: %v", err)
	}
	cases := map[string]struct{ body, want string }{
		"descending":    {"brackets:\n  - {up_to: 200, rate: 0}\n  - {up_to: 100, rate: 0.1}\n  - {up_to: 0, rate: 0.2}\n", "ascending"},
		"no open top":   {"brackets:\n  - {up_to: 100, rate: 0}\n  - {up_to: 200, rate: 0.1}\n", "open"},
		"open mid":      {"brackets:\n  - {up_to: 0, rate: 0}\n  - {up_to: 0, rate: 0.1}\n", "ascending"},
		"rate high":     {"brackets:\n  - {up_to: 0, rate: 1.5}\n", "rate"},
		"rate negative": {"brackets:\n  - {up_to: 0, rate: -0.1}\n", "rate"},
		"unknown cap":   {ok + "caps: {foo: 1}\n", "foo"},
		"upper cap":     {ok + "caps: {RMF: 1}\n", "RMF"},
		"unknown comb":  {ok + "combined: {cap: 1, types: [bar]}\n", "bar"},
		"ordinary cap":  {ok + "caps: {ordinary: 1}\n", "ordinary"},
	}
	for name, c := range cases {
		_, err := LoadRules(writeRules(t, c.body))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err=%v want mention %q", name, err, c.want)
		}
	}
}

func TestShippedRulesLoad(t *testing.T) {
	if _, err := LoadRules("../../tax_rules.yaml"); err != nil {
		t.Fatal(err)
	}
}
