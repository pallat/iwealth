// Command iwealth is a privacy-first Thai mutual fund dip alert agent.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/joho/godotenv"

	"github.com/pallat/iwealth/internal/advisor"
	"github.com/pallat/iwealth/internal/config"
	"github.com/pallat/iwealth/internal/contextsrc"
	"github.com/pallat/iwealth/internal/job"
	"github.com/pallat/iwealth/internal/llm"
	"github.com/pallat/iwealth/internal/notify/telegram"
	"github.com/pallat/iwealth/internal/sec"
	"github.com/pallat/iwealth/internal/store"
	"github.com/pallat/iwealth/internal/tax"
)

func main() {
	once := flag.Bool("once", false, "run today's job immediately (Asia/Bangkok date), then exit")
	flag.Parse()
	if err := run(*once); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(once bool) error {
	_ = godotenv.Load() // a missing .env is fine; real env vars still apply
	if _, err := time.LoadLocation("Asia/Bangkok"); err != nil {
		return fmt.Errorf("load time zone Asia/Bangkok: %w", err)
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := job.ValidateSchedule(cfg); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	rules, err := tax.LoadRules(cfg.TaxRulesPath)
	if err != nil {
		return fmt.Errorf("tax rules: %w", err)
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	adv, err := advisor.New(llm.NewDeepSeek(cfg.DeepSeekKey))
	if err != nil {
		return fmt.Errorf("advisor: %w", err)
	}
	bot := telegram.New(cfg.TelegramToken, cfg.TelegramChatID)
	providers := contextsrc.Build(cfg, st, nil)
	j := &job.Job{
		Cfg: cfg, Rules: rules, Store: st, SEC: sec.New(cfg.SECAPIKey),
		Notify: bot, Advisor: adv,
		Context: func(ctx context.Context) string { return contextsrc.Gather(ctx, providers) },
		Now:     time.Now,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if once {
		today := job.Today(time.Now())
		slog.Info("running once", "date", today.Format("2006-01-02"))
		return j.RunDaily(ctx, today)
	}

	go func() {
		if err := bot.Poll(ctx, j.HandleCommand); err != nil && ctx.Err() == nil {
			slog.Error("telegram poll stopped", "err", err)
		}
	}()

	slog.Info("iwealth started", "funds", len(cfg.Funds), "run_at", cfg.RunAt, "retry_until", cfg.RetryUntil)
	for {
		next := job.NextRun(time.Now(), cfg.RunAt)
		slog.Info("next run", "at", next.Format(time.RFC3339))
		t := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			t.Stop()
			slog.Info("shutting down")
			return nil
		case <-t.C:
		}
		date := job.Today(time.Now())
		if err := j.RunDaily(ctx, date); err != nil && ctx.Err() == nil {
			slog.Error("daily run failed", "date", date.Format("2006-01-02"), "err", err)
		}
	}
}
