# jobwatch

Emails you when a company posts a job asking for **0–1 years of experience**.

It polls company job boards directly through anonymous ATS and first-party
careers endpoints. Sources consume structured JSON, XML, or server-rendered
HTML over HTTP; scheduled runs need no login or browser. Each job is emailed
at most once.

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
export JOBWATCH_LLM_API_KEY='your Google AI Studio API key'
./jobwatch -seed   # first run only: remembers current jobs WITHOUT emailing
./jobwatch         # from now on: emails only newly posted matches
```

Skipping `-seed` on the first run would email you every job currently open.
When adding boards to an existing state file, use `-seed-new-sources`: it
baselines only those boards while existing boards continue alerting normally.
If an older state file has posting records but no exact marker for a board,
first run the unchanged source list once without `-seed-new-sources`; this
atomically records the source registry while preserving normal alerts. Jobwatch
refuses ambiguous markerless or shared-prefix sources instead of guessing. If a
genuinely new scoped source shares an old posting prefix, first complete any
legacy migration with the unchanged full config, then seed the new source alone
once with `-seed` and restore the complete config with `-seed-new-sources`.

## Run it on a schedule

**Easiest: GitHub Actions (no computer needed).** The repo ships a workflow
(`.github/workflows/jobwatch.yml`) that polls every 30 minutes. Enable it by
setting four repository secrets:

```sh
gh secret set JOBWATCH_SMTP_USERNAME   # e.g. your Gmail address
gh secret set JOBWATCH_SMTP_PASSWORD   # e.g. a Gmail app password
gh secret set JOBWATCH_EMAIL_TO        # where alerts go
gh secret set JOBWATCH_LLM_API_KEY     # Google AI Studio key for the supplied matcher
gh workflow run jobwatch -f initialize_state=true # first run only
```

The explicit initialization run seeds without sending an email blast. Seen-job
state is then kept on a `state` branch between runs. Later catalog additions
are baselined per board without suppressing alerts from boards already being
watched. Change the cadence by editing the `cron:` line (times are UTC).

### State safety

Scheduled runs fail closed if the `state` branch cannot be read. A missing
branch is initialized only by a manual run with `initialize_state=true`; a
network, authentication, or GitHub error never falls back to an empty state.
Every subsequent state commit is a normal child of the state that was restored,
and a non-fast-forward push is rejected instead of overwriting another update.

Protect the `state` branch in repository settings: disable force pushes and
deletion, and apply those restrictions to administrators. The workflow itself
writes single-parent history; requiring linear history is an optional extra
guard. Do not require pull requests or signed commits on this branch, because
the scheduled workflow writes its state commits directly.

Normal polling may add records or update notification status, but it may not
remove an existing job ID. A failed poll can still publish a valid partial
checkpoint after a successful restore; a first-time bootstrap is published
only after the complete seed succeeds. Intentional ID changes or removals are
state migrations and must follow the deterministic, fixture-tested process in
[`state/migrations`](state/migrations/README.md).

Recovery is an intentional state migration. By default, merge verified
known-good values into the current key set so newer deduplication IDs remain;
any removal must be declared and checked by the migration manifest. Publish the
repair as a new child of the current `state` head and record its exact base and
result. Never force-push or reset the branch backward: the child commit keeps
both the failure and recovery available for audit and another rollback.

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
| `ats.rippling.com/acme/jobs/...`         | `source: rippling, params: {board_slug: acme}`                     |
| `acme.freshteam.com/jobs`                | `source: freshteam, params: {company_slug: acme}`                  |
| `jobs.polymer.co/acme`                   | `source: polymer, params: {organization_slug: acme}`               |
| `acme.wd5.myworkdayjobs.com/en-US/jobs`  | `source: workday, params: {host: acme.wd5.myworkdayjobs.com, tenant: acme, site: jobs}` |

Do not rely on a company-name guess: follow an Apply link to the exact ATS
identity, then test it. A wrong token fails clearly and never blocks the other
companies:

```sh
./jobwatch -dry-run    # prints matches, sends nothing, saves nothing
```

`config.example.yaml` ships with 260 verified job-board identities. The
catalog ledgers account for every source row in two upstream lists: 145
distinct identities are represented by the 483-row
[`moreThanFAANGM` audit](catalog/morethanfaangm-audit.tsv), and 68 distinct
validated boards appear in the 131-row
[`List_OF_Companies` audit](catalog/list-of-companies-audit.tsv). Five board
identities overlap, for 208 unique identities across both audits; the other
52 boards were verified when they were added. Dead, duplicate, and
manual-review rows are documented but are not configured.

## Notifications

Every channel under `notifiers:` receives every batch — stack as many as
you like. More email recipients is just a comma-separated list:

```yaml
notifiers:
  - name: email
    params:
      # ...smtp settings...
      to: you@example.com, friend@example.com, other@example.com
  - name: webhook # Slack: create an incoming webhook, paste its URL
    params: {url_env: JOBWATCH_SLACK_WEBHOOK, format: slack}
  - name: webhook # Discord: channel settings → integrations → webhook
    params: {url_env: JOBWATCH_DISCORD_WEBHOOK, format: discord}
  - name: telegram # bot via @BotFather; chat id via /getUpdates
    params: {token_env: JOBWATCH_TELEGRAM_TOKEN, chat_id: "123456789"}
  - name: webhook # anything else: your own endpoint gets structured JSON
    params: {url: https://example.com/hook, format: json}
```

The `webhook` notifier's `format: text` also works for ntfy.sh-style
plain-text endpoints. When running via GitHub Actions, add the matching
secret and expose it in the workflow's `env:` block.

## Change what counts as a match

Matchers are composable building blocks — combine them with `all`, `any`,
and `not` to express exactly what you want, in config only:

```yaml
matcher:
  name: all # every condition must hold
  of:
    - name: experience # your 1 year fits the posting's stated range
      params: {years: 1}
    - name: employment # full-time roles only
      params: {types: "full-time"}
    - name: keywords # engineering roles only, nothing senior
      params:
        field: title
        include: "engineer, developer, sre, devops"
        exclude: "senior, staff, principal, lead, manager, director"
    - name: not # skip US-locked postings
      of:
        - name: keywords
          params: {field: location, include: "US only, United States"}
```

Built-in matchers:

| Matcher      | What it checks                                                            |
| ------------ | ------------------------------------------------------------------------- |
| `experience` | YOUR `years` falls inside the range the posting states — "0-1", "1-3", "1+", "up to 2 years", "6-18 months", "entry level"... |
| `employment` | ATS-reported employment type is in `types:` (full-time, contract, intern...) |
| `keywords`   | `include`/`exclude` term lists against `field:` title, description, location, or any (case-insensitive, whole-word) |
| `recency`    | Posting published within `max_days` (skips stale evergreen ads)            |
| `llm`        | A language model judges fit against your `profile:` through an OpenAI-compatible endpoint with JSON-schema structured-output support; see config.example.yaml |
| `all` `any` `not` | Combine other matchers under `of:`                                    |

The `llm` matcher costs one API call per new job that reaches it, so place
it last under `all` — earlier matchers veto first, and `-seed` never calls
it. If the endpoint or response is invalid, that posting stays unprocessed
for a later run. Confirmed work is still saved and delivered, then the run
exits with an error so scheduled failures are visible.

The experience matcher parses each mention into a range: "1-3 years" is
[1, 3], "2+ years" or a bare "2 years" is a floor [2, ∞), "up to 2 years"
is [0, 2]. With `years: 1`, "0-1", "1-3", and "1+" all match while "2+"
and "3-5" don't — and a "0-1 years" posting correctly rejects someone
configured with `years: 3`. It ignores decoys ("founded 10 years ago",
"in your first 3 months"), and a posting with several mentions matches if
your years fit ANY of them. Every notification shows the exact snippet
and rule that matched.

## Flags

| Flag                | What it does                                                       |
| ------------------- | ------------------------------------------------------------------ |
| `-seed`             | Remember all current jobs without emailing (first run)              |
| `-seed-new-sources` | Baseline only never-seen boards; known boards keep alerting         |
| `-dry-run`          | Print matches, send nothing, save nothing                           |
| `-interval 1h`      | Keep running, poll on that interval (default: run once)             |
| `-config path`      | Use another config file (default `config.yaml`)                     |

## Extending

Sources, matchers, and notifiers all follow the same pattern: implement a
small interface, call `Register("name", factory)` in `init()`, select it by
name in the config.

- **New job board** → add a focused adapter in `internal/source/`, register
  it, and define its stable board identity/state prefix in `source.go`
- **New matching rule** → implement `Matcher` in `internal/match/`
- **New notification channel** → implement `Notifier` in `internal/notify/`.
  Message formatting is already shared (`format.go` gives you `Headline`,
  `Text`, and per-job `Block` renderings), so a new channel is delivery
  code only — the console notifier is 12 lines, the generic webhook ~80.

## Good to know

- Local state lives in `~/.jobwatch/state.json`; deleting that file starts the
  local watcher over. Do not delete the Actions `state` branch.
- If sending fails, matches are retried next run — never silently lost.
- Failed companies are logged and skipped; the rest still work.
