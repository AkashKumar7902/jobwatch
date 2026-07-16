# jobwatch

Emails you when a company posts a job asking for **0–1 years of experience**.

It polls company job boards directly through their ATS APIs (Greenhouse, Lever,
Ashby, Workable, Recruitee, SmartRecruiters, BambooHR, Workday) — plain JSON
over HTTP, no browser, no scraping. Each job is emailed at most once.

```
fetch job boards → keep unseen jobs → match experience rule → email you
```

## Setup (5 minutes)

```sh
go build -o jobwatch ./cmd/jobwatch
cp config.example.yaml config.yaml
```

Edit `config.yaml` → `notifiers:` section with your email details. For Gmail,
create an [app password](https://myaccount.google.com/apppasswords) (needs 2FA):

```yaml
notifiers:
  - name: email
    params:
      smtp_host: smtp.gmail.com
      smtp_port: 587
      username: you@gmail.com
      password_env: JOBWATCH_SMTP_PASSWORD
      from: you@gmail.com
      to: you@example.com
```

Then:

```sh
export JOBWATCH_SMTP_PASSWORD='your app password'
./jobwatch -seed   # first run only: remembers current jobs WITHOUT emailing
./jobwatch         # from now on: emails only newly posted matches
```

Skipping `-seed` on the first run would email you every job currently open.

## Run it on a schedule

**Easiest: GitHub Actions (no computer needed).** The repo ships a workflow
(`.github/workflows/jobwatch.yml`) that polls every 30 minutes. Enable it by
setting three repository secrets:

```sh
gh secret set JOBWATCH_SMTP_USERNAME   # e.g. your Gmail address
gh secret set JOBWATCH_SMTP_PASSWORD   # e.g. a Gmail app password
gh secret set JOBWATCH_EMAIL_TO        # where alerts go
gh workflow run jobwatch               # optional: trigger the first run now
```

The first run seeds automatically (no email blast); seen-job state is kept
on a `state` branch between runs. Change the cadence by editing the `cron:`
line (times are UTC).

**Or locally**, pick one:

```sh
./jobwatch -interval 1h                # keep it running, poll hourly
```

```cron
0 * * * * cd $HOME/jobwatch && JOBWATCH_SMTP_PASSWORD=... ./jobwatch >> jobwatch.log 2>&1
```

## Add a company (one line)

```yaml
companies:
  - {name: Anthropic, source: greenhouse, params: {board_token: anthropic}}
```

To find the token, open the company's careers page and look at the URL of any
job or "Apply" link:

| Link looks like                          | Config line                                                        |
| ---------------------------------------- | ------------------------------------------------------------------ |
| `boards.greenhouse.io/acme`              | `source: greenhouse, params: {board_token: acme}`                  |
| `jobs.lever.co/acme`                     | `source: lever, params: {site: acme}`                              |
| `jobs.ashbyhq.com/acme`                  | `source: ashby, params: {board_name: acme}`                        |
| `apply.workable.com/acme`                | `source: workable, params: {account: acme}`                        |
| `acme.recruitee.com`                     | `source: recruitee, params: {company_slug: acme}`                  |
| `acme.bamboohr.com/careers`              | `source: bamboohr, params: {company_slug: acme}`                   |
| `jobs.smartrecruiters.com/Acme`          | `source: smartrecruiters, params: {company_id: Acme}`              |
| `acme.wd5.myworkdayjobs.com/en-US/jobs`  | `source: workday, params: {host: acme.wd5.myworkdayjobs.com, tenant: acme, site: jobs}` |

Not sure? Guess the company name in lowercase and test it — a wrong token
fails with a clear error and never blocks other companies:

```sh
./jobwatch -dry-run    # prints matches, sends nothing, saves nothing
```

`config.example.yaml` ships with 45 remote-friendly companies, every one
verified live.

## Change what counts as a match

```yaml
matcher:
  name: experience
  params:
    max_years: 1                # raise to 2, 3, ... for more senior roles
    notify_when_unlisted: false # true = also email jobs that list no experience
```

The matcher reads each posting for phrases like `1+ years`, `0-2 years`,
`6 months`, `one to three years`, `entry level`, `fresher`, `new grad` — and
ignores decoys like "founded 10 years ago" or "in your first 3 months".
When a posting lists several figures, the lowest wins, so you never miss a
borderline entry-level job. Every email shows the exact snippet that matched.

## Flags

| Flag           | What it does                                              |
| -------------- | --------------------------------------------------------- |
| `-seed`        | Remember all current jobs without emailing (first run)     |
| `-dry-run`     | Print matches, send nothing, save nothing                  |
| `-interval 1h` | Keep running, poll on that interval (default: run once)    |
| `-config path` | Use another config file (default `config.yaml`)            |

## Extending (one file each, nothing else changes)

Sources, matchers, and notifiers all follow the same pattern: implement a
small interface, call `Register("name", factory)` in `init()`, select it by
name in the config.

- **New job board (ATS)** → copy `internal/source/greenhouse.go` (~60 lines)
- **New matching rule** → implement `Matcher` in `internal/match/`
- **New channel** (Slack, Telegram, ...) → implement `Notifier` in `internal/notify/`

## Good to know

- State lives in `~/.jobwatch/state.json`. Delete it to start over.
- If sending fails, matches are retried next run — never silently lost.
- Failed companies are logged and skipped; the rest still work.
