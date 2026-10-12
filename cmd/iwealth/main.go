// Command iwealth is a privacy-first Thai mutual fund dip alert agent.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
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
	once := flag.Bool("once", false, "run one ingest for the latest weekday (Asia/Bangkok), log per-fund results, then exit non-zero if any fund errored")
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
	if err := cfg.Validate(); err != nil {
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
	bot.OnError = func(err error) { slog.Warn("telegram error", "err", err) }
	providers := contextsrc.Build(cfg, st, nil)
	j := &job.Job{
		Cfg: cfg, Rules: rules, Store: st, SEC: sec.New(cfg.SECAPIKey),
		Notify: bot, Advisor: adv,
		Context: func(ctx context.Context) string {
			return contextsrc.GatherWith(ctx, providers, func(name string, err error) {
				slog.Warn("context provider failed", "provider", name, "err", err)
			})
		},
		Now: time.Now,
		Log: slog.Default(),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if once {
		// One RunOnce, no retry loop and no weekend skip, for the latest weekday that
		// should have data. Per-fund results are logged by the job.
		date := job.OnceDate(time.Now(), cfg.RunAt)
		slog.Info("running once", "date", date.Format("2006-01-02"))
		missing, err := j.RunOnce(ctx, date)
		if len(missing) > 0 {
			slog.Warn("nav not published yet", "date", date.Format("2006-01-02"), "funds", missing)
		}
		if err != nil {
			return fmt.Errorf("run once: %w", err)
		}
		slog.Info("run once finished", "date", date.Format("2006-01-02"), "missing", len(missing))
		return nil
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := bot.Poll(ctx, j.HandleCommand); err != nil && ctx.Err() == nil {
			slog.Error("telegram poll stopped", "err", err)
		}
	}()

	slog.Info("iwealth started", "funds", len(cfg.Funds), "run_at", cfg.RunAt, "retry_until", cfg.RetryUntil)
	err = j.Loop(ctx)
	slog.Info("shutting down")
	stop()
	wg.Wait() // the poller uses the store; join it before st.Close runs
	return err
}
