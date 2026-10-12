# iwealth

A small personal agent for Thai mutual funds. It checks your funds' NAV (unit price) once a day after the market data comes out. When a fund falls 5%, 10% or 15% from its recent peak, it sends you a Telegram alert with a short Thai summary. It also tracks how much you have bought in tax-deductible funds (RMF, SSF, Thai ESG) so you can see the tax you may save.

It is one Go binary. It runs as a service on a Raspberry Pi.

## Prerequisites

You need three keys:

1. **SEC Open Data API key** for NAV data. Register at the SEC Open Data portal and copy the key into `SEC_API_KEY`.
   - Warning: the SEC client was written from a community client, not from official docs. It has **not been checked against the live API yet**. On your first run with a real key, run `./iwealth-arm64 -once` and check the log. If the fields do not match, fix them in `internal/sec/client.go`.
2. **DeepSeek API key** for the Thai summary. Put it in `DEEPSEEK_API_KEY`. The model name `deepseek-v4-pro` is also **unverified** until the first live call. If the call fails, check the model name in `internal/llm/deepseek.go`.
3. **Telegram bot and chat id**:
   - Create a bot with [@BotFather](https://t.me/BotFather). Copy the token into `TELEGRAM_BOT_TOKEN`.
   - Open a **private** chat with your bot and send it any message.
   - Open `https://api.telegram.org/bot<TOKEN>/getUpdates` in a browser. Find `"chat":{"id": ...}` and copy that number into `TELEGRAM_CHAT_ID`.
   - Use a private chat only. Alerts and your purchase totals go to this chat.

## Setup

```sh
cp .env.example .env
chmod 600 .env
```

Edit `.env`. The main keys:

| Key | Meaning |
| --- | --- |
| `FUNDS` | Your funds as `CODE:type`, separated by commas. Type is `rmf`, `ssf`, `thaiesg` or `ordinary`. Example: `EXAMPLE-RMF:rmf,EXAMPLE-FUND:ordinary` |
| `ANNUAL_NET_INCOME` | Your yearly net taxable income in baht **before** every deduction iwealth tracks: RMF, SSF, Thai ESG and the `/deduct` items (PVD, pension insurance, ...). Other deductions (family, life insurance, ...) should already be taken out. What you record with `/paid` and `/deduct` is subtracted from it for the estimate. Required if any fund is a tax fund. |
| `ANNUAL_GROSS_INCOME` | Optional. Your yearly gross income before any deduction. Turns on the percentage-of-income limits (`pct:` in `tax_rules.yaml`, for example RMF 30%). Without it only the baht caps apply, and `/tax` says so. |
| `RUN_AT` / `RETRY_UNTIL` / `RETRY_INTERVAL` | Daily run time (default `04:00`), last retry time (`08:00`), and gap between retries (`30m`). Times are Asia/Bangkok. A `RUN_AT` before 12:00 ingests the **previous weekday's** NAV and runs Tuesday to Saturday (Saturday fetches Friday). A `RUN_AT` from 12:00 on ingests the same day's NAV and runs Monday to Friday. |
| `YEAR_END_REMINDER_FROM` | `MM-DD` (default `12-01`) or `off`. From that date, once `RUN_AT` has passed, sends a reminder to use the remaining deduction room: weekly, then daily over the last 5 days of the year. Silent when every tracked tax fund type is full. |
| `DROP_TRIGGERS` | Alert levels in percent (default `5,10,15`). |
| `PEAK_WINDOW_DAYS` | Trading days used to find the peak (default `252`). |
| `BENCHMARKS` | SEC fund abbreviations to compare against. Empty means your tracked funds' own history. Codes not in `FUNDS` are fetched daily too (history only, never alerted). |
| `DB_PATH` / `TAX_RULES_PATH` | SQLite file and tax rules file (default `iwealth.db`, `tax_rules.yaml`). |
| `CONTEXT_PROVIDERS` | Extra context for the summary: `benchmark`, `rss`, `search`. `.env.example` sets `benchmark`. Empty turns all off. |

Other keys are described in `.env.example`. Leave an optional key empty to turn that feature off.

### Tax rules

`tax_rules.yaml` holds the tax brackets and the RMF, SSF and Thai ESG caps. **The values in this file are placeholders.** Check them against the current Revenue Department rules every year, and update the file before the new tax year.

Items that share the overall retirement cap (RMF, SSF, provident fund, GPF, national savings fund, teacher fund, pension insurance) are listed under `combined:`. Thai ESG is outside it. Percentage-of-income limits live under `pct:` and need `ANNUAL_GROSS_INCOME`. All numbers come from news summaries, so check them against the Revenue Department before relying on them.

## Telegram commands

- `/paid <fund> <amount>`: record a purchase of a tax-deductible fund. Example: `/paid EXAMPLE-RMF 10000`.
- `/deduct <key> <year-to-date total>`: record a deduction you do not buy through a tracked fund (keys: `pvd`, `gpf`, `nsf`, `teacher_fund`, `pension_ins`). The amount **replaces** the previous value, so send the running total for the year; `0` removes it. Example: `/deduct pvd 60000`. Plain `/deduct` lists the keys. RMF, SSF and Thai ESG use `/paid`.
- `/tax`: show this year's used, cap and remaining room per item, the shared-cap position, and the estimated tax saved.

## Privacy

What goes to DeepSeek (the summary):
- Fund code and type, latest NAV, peak NAV, percent drop, and the peak window length.
- The benchmark NAV changes, plus any RSS headlines or search results you turned on.

What never goes to DeepSeek or search:
- Your income, your purchase amounts, your `/paid` records, and your tax figures.

The search provider (if enabled) gets only a fixed query, `Thai mutual fund market news`. It does not get your fund codes or amounts.

Your data stays in `iwealth.db` on the Pi.

## Build

On your Mac or Linux machine:

```sh
make test build-pi
file bin/iwealth-arm64    # expect: ELF 64-bit ... ARM aarch64
```

`bin/` is in `.gitignore`. Do not commit the binary.

## Install on the Raspberry Pi

The service runs as a user called `iwealth`. Its working directory is `/opt/iwealth`.

**The service reads `.env` from its working directory.** The program calls `godotenv.Load()` with no path, so it loads `.env` from the current directory. The database path (`iwealth.db`) and the tax file path (`tax_rules.yaml`) also default to the current directory. So all three files must sit in `/opt/iwealth`.

1. Create the user and folder on the Pi (replace `pi@raspberrypi.local` with your host):

   ```sh
   ssh pi@raspberrypi.local 'sudo useradd --system --home-dir /opt/iwealth --shell /usr/sbin/nologin iwealth; sudo mkdir -p /opt/iwealth; sudo chown iwealth:iwealth /opt/iwealth'
   ```

2. Copy the files to the Pi:

   ```sh
   scp bin/iwealth-arm64 tax_rules.yaml .env deploy/iwealth.service pi@raspberrypi.local:~/
   ```

3. Move them into place and install the unit:

   ```sh
   ssh pi@raspberrypi.local 'sudo mv ~/iwealth-arm64 ~/tax_rules.yaml ~/.env /opt/iwealth/ && sudo chown -R iwealth:iwealth /opt/iwealth && sudo chmod 600 /opt/iwealth/.env && sudo chmod 755 /opt/iwealth/iwealth-arm64 && sudo mv ~/iwealth.service /etc/systemd/system/ && sudo systemctl daemon-reload'
   ```

4. Test once, as the `iwealth` user. Run it as that user so the database file is owned by it:

   ```sh
   ssh pi@raspberrypi.local 'cd /opt/iwealth && sudo -u iwealth ./iwealth-arm64 -once'
   ```

   `-once` runs a single ingest, with no retry loop, for the NAV date the scheduler would fetch at the latest `RUN_AT` (with the default `04:00`: yesterday's NAV if it was a weekday, otherwise the previous Friday). It logs one line per fund: NAV found (code, date, nav), not published, or error. It exits non-zero if any fund errored, so a wrong SEC field mapping shows up here. Check the output and your Telegram chat. Fix any SEC or DeepSeek mismatch (see Prerequisites) before going on.

5. The Pi has no battery clock, so on boot the time can be wrong until NTP syncs. The unit waits for `time-sync.target`; enable the wait service so that target means the clock is really synced:

   ```sh
   ssh pi@raspberrypi.local 'sudo systemctl enable systemd-time-wait-sync.service'
   ```

   The scheduler also re-reads the clock every minute, so a late time sync does not skip the run.

6. Start the service:

   ```sh
   ssh pi@raspberrypi.local 'sudo systemctl enable --now iwealth'
   ssh pi@raspberrypi.local 'sudo journalctl -u iwealth -n 50 --no-pager'
   ```

If you change `.env` or `tax_rules.yaml` later, copy the new file to `/opt/iwealth` and run `sudo systemctl restart iwealth`.

## Correcting a mistyped /paid

Repeating `/paid` adds up, so a typo cannot be undone from Telegram. Stop the service and delete the last row of table `tax_paid`:

```sh
ssh pi@raspberrypi.local 'sudo systemctl stop iwealth && sudo -u iwealth sqlite3 /opt/iwealth/iwealth.db "DELETE FROM tax_paid WHERE id = (SELECT max(id) FROM tax_paid);" && sudo systemctl start iwealth'
```

## Backup

`/opt/iwealth/iwealth.db` holds your purchase records, so back it up. Stop the service first so the copy is consistent:

```sh
ssh pi@raspberrypi.local 'sudo systemctl stop iwealth && sudo cp /opt/iwealth/iwealth.db* ~/ && sudo chown $(id -un) ~/iwealth.db* && sudo systemctl start iwealth'
scp 'pi@raspberrypi.local:~/iwealth.db*' ./backup/
```

Keep the backup somewhere private. `.env` holds your API keys, so keep a private copy of it too.
