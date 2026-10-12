package config

import "testing"

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func base() map[string]string {
	return map[string]string{
		"SEC_API_KEY": "s", "DEEPSEEK_API_KEY": "d", "TELEGRAM_BOT_TOKEN": "t",
		"TELEGRAM_CHAT_ID": "42", "FUNDS": "ABC-RMF:rmf, XYZ:ordinary",
		"ANNUAL_NET_INCOME": "800000",
	}
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(base()))
	if err != nil {
		t.Fatal(err)
	}
	if c.PeakWindowDays != 252 || c.RunAt != "20:00" || c.RetryUntil != "23:30" {
		t.Fatalf("bad defaults: %+v", c)
	}
	if len(c.Funds) != 2 || c.Funds[0] != (Fund{"ABC-RMF", RMF}) || c.Funds[1].Type != Ordinary {
		t.Fatalf("funds: %+v", c.Funds)
	}
	if len(c.DropTriggers) != 3 || c.DropTriggers[0] != 5 {
		t.Fatalf("triggers: %v", c.DropTriggers)
	}
	if c.TelegramChatID != 42 || c.RetryInterval.Minutes() != 30 {
		t.Fatalf("misc: %+v", c)
	}
}

func TestLoadErrors(t *testing.T) {
	cases := map[string]func(map[string]string){
		"missing sec key": func(m map[string]string) { delete(m, "SEC_API_KEY") },
		"bad fund type":   func(m map[string]string) { m["FUNDS"] = "A:bogus" },
		"bad fund format": func(m map[string]string) { m["FUNDS"] = "A" },
		"no funds":        func(m map[string]string) { m["FUNDS"] = "" },
		"bad chat id":     func(m map[string]string) { m["TELEGRAM_CHAT_ID"] = "x" },
		"bad trigger":     func(m map[string]string) { m["DROP_TRIGGERS"] = "5,abc" },
		"bad window":      func(m map[string]string) { m["PEAK_WINDOW_DAYS"] = "0" },
	}
	for name, mut := range cases {
		m := base()
		mut(m)
		if _, err := Load(env(m)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestValidateTaxFundNeedsIncome(t *testing.T) {
	m := base()
	m["ANNUAL_NET_INCOME"] = ""
	c, err := Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err == nil {
		t.Fatal("tax fund without income must fail")
	}
	m["FUNDS"] = "XYZ:ordinary"
	c, _ = Load(env(m))
	if err := c.Validate(); err != nil {
		t.Fatalf("ordinary-only needs no income: %v", err)
	}
	c, _ = Load(env(base()))
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
