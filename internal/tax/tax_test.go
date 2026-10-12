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
		Groups:   []Group{{Name: "retirement", Label: "r", Cap: 500000, Types: []config.FundType{config.RMF, config.SSF}}},
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
		if got := r.RemainingRoom(c.typ, c.paid, 0); got != c.want {
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
	if _, err := LoadRules(writeRules(t, ok+"caps: {rmf: 5}\ngroups: [{name: g, cap: 5, types: [rmf, ssf]}]\n")); err != nil {
		t.Fatalf("valid rules rejected: %v", err)
	}
	cases := map[string]struct{ body, want string }{
		"descending":      {"brackets:\n  - {up_to: 200, rate: 0}\n  - {up_to: 100, rate: 0.1}\n  - {up_to: 0, rate: 0.2}\n", "ascending"},
		"no open top":     {"brackets:\n  - {up_to: 100, rate: 0}\n  - {up_to: 200, rate: 0.1}\n", "open"},
		"open mid":        {"brackets:\n  - {up_to: 0, rate: 0}\n  - {up_to: 0, rate: 0.1}\n", "ascending"},
		"rate high":       {"brackets:\n  - {up_to: 0, rate: 1.5}\n", "rate"},
		"rate negative":   {"brackets:\n  - {up_to: 0, rate: -0.1}\n", "rate"},
		"unknown cap":     {ok + "caps: {foo: 1}\n", "foo"},
		"upper cap":       {ok + "caps: {RMF: 1}\n", "RMF"},
		"unknown comb":    {ok + "groups: [{name: g, cap: 1, types: [bar]}]\n", "bar"},
		"ordinary cap":    {ok + "caps: {ordinary: 1}\n", "ordinary"},
		"manual reserved": {ok + "manual: [{key: rmf}]\n", "reserved"},
		"manual upper":    {ok + "manual: [{key: PVD}]\n", "lower-case"},
		"manual dup":      {ok + "manual: [{key: a}, {key: a}]\n", "duplicate"},
		"pct too big":     {ok + "caps: {rmf: 1}\npct: {rmf: 30}\n", "pct"},
		"pct unknown":     {ok + "pct: {zzz: 0.3}\n", "zzz"},
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

func TestManualAndPctRoom(t *testing.T) {
	r := rules()
	r.Manual = []Manual{{Key: "pvd", Label: "PVD"}, {Key: "pension_ins", Label: "Pension"}}
	r.Caps["pvd"] = 500000
	r.Caps["pension_ins"] = 200000
	r.Groups[0].Types = []config.FundType{config.RMF, config.SSF, "pvd", "pension_ins"}
	r.Pct = map[config.FundType]float64{config.RMF: 0.3, "pension_ins": 0.15}

	paid := map[config.FundType]float64{"pvd": 100000, config.RMF: 50000}
	if got := r.RemainingRoom(config.RMF, paid, 0); got != 350000 { // 500k group - 150k; pct ignored w/o gross
		t.Errorf("no gross: %v", got)
	}
	if got := r.RemainingRoom(config.RMF, paid, 600000); got != 130000 { // 30%*600k=180k - 50k
		t.Errorf("gross pct: %v", got)
	}
	if got := r.RemainingRoom("pension_ins", paid, 600000); got != 90000 { // 15%*600k
		t.Errorf("pension pct: %v", got)
	}
	if got := r.RemainingRoom("pension_ins", map[config.FundType]float64{"pvd": 400000, config.RMF: 50000}, 0); got != 50000 { // group binds
		t.Errorf("group binds: %v", got)
	}
	if got := r.RemainingRoom("unknown", paid, 0); got != 0 {
		t.Errorf("unknown: %v", got)
	}
	if !r.IsManual("pvd") || r.IsManual(config.RMF) || r.Label("pvd") != "PVD" || r.Label(config.RMF) != "RMF" {
		t.Error("IsManual/Label")
	}
}

func TestDeductibleClampsToCaps(t *testing.T) {
	r := rules()
	r.Manual = []Manual{{Key: "pvd"}, {Key: "pension_ins"}}
	r.Caps["pvd"] = 500000
	r.Caps["pension_ins"] = 200000
	r.Groups[0].Types = []config.FundType{config.RMF, config.SSF, "pvd", "pension_ins"}
	r.Pct = map[config.FundType]float64{"pension_ins": 0.15}

	// pension insurance 250k -> 200k item cap
	d := r.Deductible(map[config.FundType]float64{"pension_ins": 250000}, 0)
	if d["pension_ins"] != 200000 {
		t.Errorf("item cap: %v", d)
	}
	// with gross 1,000,000, 15% = 150k wins
	d = r.Deductible(map[config.FundType]float64{"pension_ins": 250000}, 1000000)
	if d["pension_ins"] != 150000 {
		t.Errorf("pct cap: %v", d)
	}
	// group cap: rmf 400k + pvd 300k + pension 200k = 900k raw -> 500k allocated in order
	paid := map[config.FundType]float64{config.RMF: 400000, "pvd": 300000, "pension_ins": 200000}
	d = r.Deductible(paid, 0)
	if d[config.RMF] != 400000 || d["pvd"] != 100000 || d["pension_ins"] != 0 {
		t.Errorf("group allocation: %v", d)
	}
	if got := r.GroupUsed(r.Groups[0], paid, 0); got != 500000 {
		t.Errorf("GroupUsed = %v", got)
	}
	// over-cap entry must not eat into group room beyond its cap
	if got := r.RemainingRoom(config.RMF, map[config.FundType]float64{"pension_ins": 250000}, 0); got != 300000 {
		t.Errorf("RMF room = %v want 300000", got)
	}
}

func TestIncomeBaseExpenseAndPosition(t *testing.T) {
	r := rules()
	r.Expense = Expense{Pct: 0.5, Max: 100000}
	r.Manual = []Manual{{Key: "personal", Auto: true}}
	r.Caps["personal"] = 60000

	cases := []struct {
		name string
		inc  Income
		want float64
	}{
		{"salary capped", Income{Gross: 1000000}, 900000},
		{"salary half", Income{Gross: 120000}, 60000},
		{"none", Income{Gross: 1000000, Expense: "none"}, 1000000},
		{"fixed", Income{Gross: 1000000, Expense: "fixed", ExpenseFixed: 30000}, 970000},
		{"fixed never above gross", Income{Gross: 20000, Expense: "fixed", ExpenseFixed: 30000}, 0},
		{"legacy adds auto back", Income{LegacyNet: 500000}, 560000},
	}
	for _, c := range cases {
		if got := r.Base(c.inc); got != c.want {
			t.Errorf("%s: Base = %v want %v", c.name, got, c.want)
		}
	}
	// legacy: counting the auto item again lands back on the configured net
	if got := r.NetTaxable(Income{LegacyNet: 500000}, nil); got != 500000 {
		t.Errorf("legacy net = %v", got)
	}
	// gross mode deducts personal automatically
	if got := r.NetTaxable(Income{Gross: 1000000}, nil); got != 840000 {
		t.Errorf("gross net = %v", got)
	}
	if r.RemainingRoom("personal", nil, 0) != 0 {
		t.Error("auto item has no room")
	}

	pos := r.BracketAt(300000) // top edge stays in the 5% bracket
	if pos.Rate != 0.05 || pos.Lower != 150000 || pos.Upper != 300000 || pos.PrevRate != 0 || pos.NextRate != 0.10 {
		t.Errorf("%+v", pos)
	}
	if pos := r.BracketAt(0); pos.Index != 0 || pos.Rate != 0 {
		t.Errorf("%+v", pos)
	}
	if pos := r.BracketAt(9e9); pos.Upper != 0 || pos.Rate != 0.20 {
		t.Errorf("%+v", pos)
	}
}

func TestGroupsAndKindValidation(t *testing.T) {
	const ok = "brackets:\n  - {up_to: 0, rate: 0.1}\n"
	bad := map[string]struct{ body, want string }{
		"item in two groups": {ok + "groups: [{name: a, cap: 1, types: [rmf]}, {name: b, cap: 1, types: [rmf]}]\n", "already in group"},
		"dup group":          {ok + "groups: [{name: a, cap: 1, types: [rmf]}, {name: a, cap: 1, types: [ssf]}]\n", "unique"},
		"zero group cap":     {ok + "groups: [{name: a, cap: 0, types: [rmf]}]\n", "cap"},
		"bad kind":           {ok + "manual: [{key: x, kind: buy}]\n", "kind"},
		"auto without cap":   {ok + "manual: [{key: x, auto: true}]\n", "auto"},
		"bad expense":        {ok + "expense: {pct: 2, max: 1}\n", "expense"},
	}
	for name, c := range bad {
		if _, err := LoadRules(writeRules(t, c.body)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err=%v want %q", name, err, c.want)
		}
	}
	good := ok + "caps: {x: 5}\nmanual: [{key: x, kind: invest}]\ngroups: [{name: a, cap: 9, types: [rmf, x]}]\n"
	r, err := LoadRules(writeRules(t, good))
	if err != nil || !r.Invest("x") || !r.Invest(config.RMF) || r.GroupOf("x") == nil {
		t.Fatalf("%v %+v", err, r)
	}
}
