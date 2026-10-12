# iwealth — Personal Fund Agent Design

Date: 2026-10-12

## Purpose

A local, privacy-first agent that runs on a Raspberry Pi at home. Each evening it
ingests Thai mutual fund NAV from the SEC Thailand Open Data API, detects
dip/pullback from a rolling peak, and sends a Telegram alert with an LLM-written
market context and a tax-aware action plan (how much to buy to use remaining
deduction room, and how much tax that saves).

Out of scope for v1: web UI (htm frontend deferred), automatic trading, scraping
fallback (added only if the SEC API proves insufficient).

## Decisions (agreed)

- Language/stack: Go 1.27, Google ADK for Go, SQLite (single file), single binary, in-process scheduler.
- LLM: `deepseek-v4-pro` via DeepSeek official API (`https://api.deepseek.com`, Chat Completions).
- Alerts: Telegram bot.
- Tracked funds: configured in `.env`; any fund type (tax funds RMF/SSF/ThaiESG and ordinary funds).
  Ordinary funds get dip alerts only, no tax plan.
- Peak: rolling window, default 252 trading days (`PEAK_WINDOW_DAYS`).
- Architecture: deterministic pipeline; the ADK agent is used only for the narrative
  summary step and only on days an alert triggers. All money figures are computed in Go.
- Action plan: suggested purchase amount = remaining deduction room (per fund type cap
  and combined cap), plus estimated tax saved using the user's progressive tax brackets.
- Context for the LLM: pluggable `ContextProvider`s configured in `.env`.
  v1 ships a numeric `benchmark` provider; `rss` and `search` are config-driven slots
  (URL/key set = enabled, unset = disabled).
- "Actual deduction used this year" is user-entered (no API exists), via Telegram commands.

## Risks / things to verify during implementation

1. **SEC API**: V2 host is `api.sec.or.th`; daily NAV believed at `/v2/fund/daily-info/nav`.
   Requires a free subscription key (user must register). Response schema, parameters and
   publish time of NAV are unconfirmed — verify against the official portal first and
   record a sample response as a test fixture.
2. **ADK Go + DeepSeek**: ADK Go's `openaimodel` targets the OpenAI Responses API
   (experimental). DeepSeek is expected to support only Chat Completions, so a custom
   `model.LLM` adapter is planned in `internal/llm`. Verify ADK's `model.LLM` interface
   and DeepSeek endpoint behavior before committing to the adapter.
3. Tax rules change yearly; they live in `tax_rules.yaml`, not code. Initial values must
   be checked against Revenue Department rules when written.

## Layout

```
cmd/iwealth/main.go        wiring, scheduler loop
internal/config/           load .env + tax_rules.yaml
internal/sec/              SEC V2 client (daily NAV) + retry
internal/store/            SQLite: nav, alert_log, tax_paid
internal/dip/              rolling-window peak, %drop, threshold levels (pure)
internal/tax/              progressive brackets, per-type + combined caps, tax saved (pure)
internal/plan/             dip + tax -> action plan (pure)
internal/llm/              DeepSeek model.LLM adapter (Chat Completions)
internal/advisor/          ADK agent: facts + context -> Thai summary (no calc tools)
internal/contextsrc/       ContextProvider interface: benchmark, rss, search
internal/notify/telegram/  send alerts; receive /paid and /tax commands
```

Each unit has one purpose and communicates through small interfaces so it can be
tested with fakes.

## Configuration

`.env`:

| Key | Meaning |
|---|---|
| `SEC_API_KEY`, `DEEPSEEK_API_KEY`, `TELEGRAM_BOT_TOKEN`, `TELEGRAM_CHAT_ID` | credentials |
| `FUNDS` | `code:type,...` type in `rmf,ssf,thaiesg,ordinary` |
| `PEAK_WINDOW_DAYS` | default 252 |
| `DROP_TRIGGERS` | e.g. `5,10,15` (% drop levels) |
| `ANNUAL_NET_INCOME` | net taxable income for bracket lookup; never leaves the machine |
| `RUN_AT`, `RETRY_UNTIL`, `RETRY_INTERVAL` | default 20:00, 23:30, 30m |
| `CONTEXT_PROVIDERS` | e.g. `benchmark,rss,search` |
| `BENCHMARKS`, `RSS_URLS`, `SEARCH_API_URL`, `SEARCH_API_KEY` | provider settings; unset = disabled |

`tax_rules.yaml` (per tax year): progressive brackets, caps for RMF / SSF / ThaiESG,
combined cap.

## Data flow

1. At `RUN_AT`, fetch NAV for every tracked fund; upsert by (fund, date). If the day's NAV
   is not yet published, retry every `RETRY_INTERVAL` until `RETRY_UNTIL`; skip market
   holidays; if still missing, send a Telegram failure notice.
2. Compute rolling peak and %drop per fund.
3. For funds crossing a new trigger level (deduped via `alert_log`: re-alert only when a
   deeper level is crossed, or NAV recovered above the trigger and dropped again):
   gather context from enabled providers, then call the advisor.
4. For tax-type funds: plan = remaining room (cap minus `tax_paid` for the year) -> purchase
   amount and estimated tax saved from brackets. Computed in Go, appended to the message
   after the LLM text; the LLM never sees income or holdings.
5. Send Telegram message: LLM summary + action plan + sources.
6. Telegram commands: `/paid <fund> <amount>` records actual purchase; `/tax` shows the year's
   used/remaining room and estimated savings.

## Privacy

Sent to DeepSeek / search: fund names, NAV, %drop, benchmark numbers, public headlines.
Never sent: income, holdings, purchase history, plan amounts. Secrets only in `.env`
(gitignored). Telegram is the only outbound notification channel; restrict the bot to
`TELEGRAM_CHAT_ID`.

## Error handling

- LLM or context failure -> send the numeric alert and plan without narrative.
- SEC failure after retries -> failure notice, no silent skip.
- Config validation at startup; fail fast with a clear message.
- The LLM prompt instructs it to use only provided facts and say "unknown" otherwise.

## Testing

- `dip`, `tax`, `plan` are pure: table-driven tests (including bracket edges, cap
  interactions, window boundaries, dedupe).
- SEC, DeepSeek, Telegram behind interfaces with fakes; fixtures from real SEC responses.
- No tests call live APIs in CI.

## Deployment

Cross-compile `GOOS=linux GOARCH=arm64`; run as a systemd service on the Pi; SQLite file
on local disk; periodic file backup left to the user.
