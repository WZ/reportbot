# ReportBot

A Slack bot that collects a development team's work items and files each one into the team's weekly report **as it arrives**, then publishes that report.

Developers report work via slash commands. The bot also pulls merged/open GitLab MRs, GitLab branch commits and GitHub PRs (manually or on a cron schedule). Every Work Item is filed into the report's Taxonomy (Area → Category → Section, kept in the database) right away:

1. A decision model (System One protocol) picks the Section in one request, and in the same request answers the narrow questions code cannot decide — Status, ticket ID, whether a report is an Update to an existing Work Item — with calibrated probabilities.
2. Confident picks are filed silently; doubtful ones ask the reporter "Where does this go?" with the model's top three Sections.
3. If nobody answers within 24 hours (or before publishing), the Work Item is filed under the model's first choice; the reporter can still change it.
4. An optional LLM (Anthropic Claude or an OpenAI-compatible endpoint) is used only for Structure Proposals: suggested new, split or retired Sections that a manager approves.

The team sees the live Weekly Report in the web UI; managers publish it to Slack as markdown and an email draft. See `CONTEXT.md` for the domain terms and `docs/adr/` for the design decisions.

## Features

### Commands

- `/report` (or `/rpt`) — Report work items; each is filed on arrival and the reply shows where it went
- `/fetch` — Pull merged and open GitLab MRs, branch commits and/or GitHub PRs for the current calendar week
- `/generate-report` (or `/gen`) — Publish this week's report (team `.md` or boss `.eml`) and upload it to Slack
- `/list` — View your items on this week's report with inline edit/delete actions (`/list all` for the team view)
- `/check` — Managers: list missing members with nudge buttons, and merged code changes waiting in testing with Done buttons
- `/retrospect` — Managers: generate Structure Proposals now (new, split or retired Sections, with Approve/Reject)
- `/stats` — Managers: this week's filing (auto-filed vs asked, filed automatically because the reporter didn't answer, corrections by Section, Undetermined count)
- `/help` — Show all commands and example usage

### Filing on arrival

- **One decision-model request per Work Item** picks the Section — a Choice over the active Sections (path plus any manager-written description), given the most similar past Work Items and where they were filed (TF-IDF) — and, when code cannot decide, asks for the Status, the ticket ID among several candidate numbers, and whether the report updates an Open Work Item. The Choice's confidence is the Placement confidence.
- **Below `decision_model_confidence`** the item waits in Undetermined and the reporter gets "Where does this go?" buttons for the model's three most likely Sections; a click is a person Placement.
- **Updates** are always confirmed by the reporter ("Looks like an update to …" [Update it] [It's new]).
- **Code Changes** link to an Open Work Item on an exact ticket match; a merge moves an `in progress` Work Item to `in testing`. Unlinked ones become their own Work Items (`in testing` once merged — `done` is always a person's decision).
- **Taxonomy edits** (web UI) re-file Open Work Items; confident moves happen automatically and a person whose choice was overridden gets an Undo button.
- **Never lost**: if the decision model is unreachable, the item is filed as Undetermined and a sweep retries it.
- **Fallback**: an unsure Work Item nobody placed within 24 hours, or still Undetermined when a manager confirms publishing, is filed under the decision model's most likely Section from its stored answer (no new model call). After a Taxonomy change the re-filing job re-asks unsure Work Items first, so the answer always matches the current Sections. The reporter is told "Filed under X automatically" with a Change button.

### Structure Proposals (optional LLM)

After each publish (and on `/retrospect`), the LLM proposes 0–3 changes: a new Section for a recurring theme among Undetermined and "Other…" Work Items, a split of a large Section, or retiring one unused for 8 weeks. Managers get Approve/Reject by DM; Approve applies it like a Taxonomy edit, a rejected change is not proposed again for 8 weeks, and an undecided one expires after 2 weeks. A retirement is checked again at approval. LLM-written notes for the decision model were tried and rejected (ADR 0003).

### Edit history

Every Work Item edit (text, Status, Status Note, tickets), delete and confirmed Update is recorded in `work_item_edits` with before/after, who and where (Slack, the nudge DM, web, fetch, cut-over). Every Friday at 09:00 (`timezone`) a Draft Snapshot of the Weekly Report is stored in `draft_snapshots`, next to the Published Report. Together they show what people change by hand.

See [docs/agentic-features-overview.md](docs/agentic-features-overview.md) for a detailed overview.

## Quick Start

### 1. Create a Slack App

1. Go to [api.slack.com/apps](https://api.slack.com/apps) and create a new app
2. Enable **Socket Mode** — generate an App-Level Token with `connections:write` scope
3. Under **OAuth & Permissions**, add these Bot Token Scopes:
   - `chat:write`
   - `commands`
   - `files:write`
   - `im:write` (for nudge DMs)
   - `users:read` (to resolve full names for managers/team members)
4. Under **Event Subscriptions**, subscribe to these bot events:
   - `member_joined_channel` (sends welcome message to new members)
5. Under **Interactivity & Shortcuts**, toggle **Interactivity** on (required for edit/delete modals in `/list`)
6. Under **Slash Commands**, create these commands:

   | Command | Description |
   |---|---|
   | `/report` | Report a work item |
   | `/rpt` | Alias of `/report` |
   | `/fetch` | Fetch merged and open GitLab MRs, branch commits and/or GitHub PRs for this week |
   | `/generate-report` | Publish the weekly report (`team`/`boss`) or post latest team report (`post`), optional `private` |
   | `/gen` | Alias of `/generate-report` |
   | `/list` | List your work items for this week (`/list all` for the team view) |
   | `/check` | List missing members with nudge buttons, and merged code changes waiting in testing |
   | `/nudge` | Send a test nudge DM (self by default; managers can target one member) |
   | `/retrospect` | Generate Structure Proposals now |
   | `/stats` | View this week's filing stats |
   | `/help` | Show help and usage |

7. Install the app to your workspace

### 2. Configure

Configuration can be provided via **`config.yaml`** file, **environment variables**, or both. Env vars take precedence over YAML values.
The repository `config.yaml` is committed as an example; never put real values in it.
Put environment-specific settings under `override/` (git-ignored), for example `override/config.yaml`, and point `CONFIG_PATH` at it, or use environment variables.

#### Option A: config.yaml (recommended)

Create `config.yaml` and edit:

```yaml
# Slack
slack_bot_token: "xoxb-..."
slack_app_token: "xapp-..."

# GitLab sources (YAML only). Each entry is one GitLab endpoint:
#   - group sources (group_id) fetch all MRs in that group
#   - branch sources (project + branch) fetch commits from that branch; each commit
#     becomes a Work Item with Status "done", owned via the author_ids plain name key
#   Merge/revert commits are skipped; one failing source does not abort the others.
# Tokens may reference environment variables: token: "${MY_GITLAB_TOKEN}"
# Exactly one source must have default: true (the source unqualified gitlab: author_ids
# keys resolve against). Names are unique, lowercase letters/digits/_/-.
gitlab_sources:
  - name: dops
    url: "https://gitlab.example.com"
    token: "${DOPS_GITLAB_TOKEN}"
    group_id: "my-team"
    ref_ticket_label: "Jira"      # optional: parse ticket IDs from "Jira:" field in GitLab MR description; empty disables parsing
    default: true
  - name: corp
    url: "https://gitlab.corp.example.com"
    token: "${CORP_GITLAB_TOKEN}"
    project: "my-group/my-project"
    branch: "main"
    ref_ticket_label: "Jira"      # optional: parse ticket IDs from "Jira <id>:" in commit messages; empty disables parsing

# GitHub (optional)
github_ref_ticket_label: "Jira"   # optional: parse ticket IDs from "Jira:" field in GitHub PR description; empty disables parsing

# Decision model (System One protocol): files every Work Item
decision_model_url: "https://api.compat.example"
decision_model_api_key: "..."
decision_model_name: "..."
decision_model_confidence: 0.85  # Section confidence needed to file without asking
similar_item_count: 30           # optional: similar past Work Items the decision model sees

# Optional LLM, only for Structure Proposals (and /retrospect); leave llm_provider empty to skip
llm_provider: "anthropic"       # "anthropic" or "openai"
llm_reasoning_effort: ""        # optional (openai provider): low, medium or high; empty sends none
anthropic_api_key: "sk-ant-..."
openai_api_key: ""
openai_base_url: "https://api.openai.com/v1"  # optional: OpenAI-compatible base URL

# Permissions (Slack user IDs)
manager_slack_ids:
  - "U01ABC123"

# Team members (Slack full names or user IDs) - used by /check, scheduled nudge and the /fetch team filter
team_members:
  - "Member One"
  - "Member Two"

# Owners of fetched and imported items -> Slack user ID. Plain keys are report author names
# (migrate-v2 import and branch commits); "gitlab@<source name>:<numeric id or username>" are
# GitLab accounts on that source, "gitlab:<...>" resolves against the default source only,
# "github:<numeric id or login>" are GitHub accounts (fetch only). Usernames match exactly,
# ignoring case, and must be updated here if renamed or reused; numeric IDs never change.
# When an ID key and a username key both match and disagree, the ID key wins (logged).
# Unmapped items are manager-only. Draft with migrate-v2 --suggest-author-ids.
author_ids:
  "Member One": "U02MEMBER1"          # plain name key (migrate-v2 import, branch commits)
  "gitlab@dops:mone": "U02MEMBER1"    # GitLab username key on source "dops"
  "gitlab:asmith": "U02MEMBER1"       # GitLab account on the default source
  "github:1234": "U02MEMBER1"         # numeric account ID key

# Automatic MR/PR fetching (cron expression, empty to disable)
auto_fetch_schedule: "0 9 * * 1-5"  # weekdays at 9am

# Day and time for scheduled nudge (configured timezone)
nudge_day: "Friday"
nudge_time: "10:00"
monday_cutoff_time: "12:00"  # Monday before this time uses previous week

# Timezone for week range and scheduled nudge (IANA format)
timezone: "America/Los_Angeles"

# Team name (used in report header and filename)
team_name: "Example Team"

# Report channel (Slack channel ID for reminders)
report_channel_id: "C01234567"
external_http_timeout_seconds: 90  # optional: timeout for GitLab/GitHub/model HTTP calls
tls_skip_verify: false             # optional: skip TLS cert verification for internal/corporate CAs

# Web UI (optional)
web_enabled: false
web_port: 8080
web_client_id: ""                  # Slack OAuth client ID
web_client_secret: ""              # Slack OAuth client secret
web_session_secret: ""             # Random secret for cookie signing (openssl rand -hex 32)
web_base_url: "http://localhost:8080"
```

Set `CONFIG_PATH` env var to load from a different path (default: `./config.yaml`).

#### Option B: Environment Variables

```bash
export SLACK_BOT_TOKEN=xoxb-...
export SLACK_APP_TOKEN=xapp-...
# GitLab sources are YAML-only (config.yaml gitlab_sources); use ${ENV_VAR} placeholders
# for tokens, e.g. token: "${DOPS_GITLAB_TOKEN}", then export them here:
export DOPS_GITLAB_TOKEN=glpat-...
export CORP_GITLAB_TOKEN=glpat-...
export GITHUB_REF_TICKET_LABEL=Jira            # Optional: field label used for GitHub PR ticket parsing
export DECISION_MODEL_URL=https://api.compat.example
export DECISION_MODEL_API_KEY=...
export DECISION_MODEL_NAME=...
export DECISION_MODEL_CONFIDENCE=0.85           # Optional: default 0.85
export SIMILAR_ITEM_COUNT=30                    # Optional: similar past Work Items the decision model sees
export LLM_PROVIDER=anthropic                   # Optional: only Structure Proposals use the LLM
export ANTHROPIC_API_KEY=sk-ant-...
export OPENAI_API_KEY=
export OPENAI_BASE_URL=https://api.openai.com/v1
export LLM_REASONING_EFFORT=low                 # Optional (openai provider): low, medium or high
export MANAGER_SLACK_IDS="U01ABC123,U02DEF456"  # Comma-separated Slack user IDs
export AUTHOR_IDS='{"Member One": "U02MEMBER1", "gitlab@dops:mone": "U02MEMBER1", "gitlab:asmith": "U02MEMBER1", "github:1234": "U02MEMBER1"}'  # JSON: report name or gitlab@<source>:/gitlab:/github:<numeric id or username> -> Slack user ID
export REPORT_CHANNEL_ID=C01234567
export EXTERNAL_HTTP_TIMEOUT_SECONDS=90          # Optional: timeout for external API HTTP calls
export TLS_SKIP_VERIFY=true                      # Optional: skip TLS cert verification
export AUTO_FETCH_SCHEDULE="0 9 * * 1-5"        # Optional: cron schedule for auto-fetch
export WEB_ENABLED=true                          # Optional: enable web UI
export WEB_PORT=8080
export WEB_CLIENT_ID=your-slack-client-id        # Slack OAuth client ID
export WEB_CLIENT_SECRET=your-slack-client-secret
export WEB_SESSION_SECRET=$(openssl rand -hex 32)
export WEB_BASE_URL=http://localhost:8080
export MONDAY_CUTOFF_TIME=12:00
export TIMEZONE=America/Los_Angeles
```

Note: the Taxonomy lives in the database and is edited in the web UI (`/taxonomy`); published `.md` files are output only.

#### LLM (optional, Structure Proposals only)

| Provider | Default Model |
|---|---|
| `anthropic` | `claude-sonnet-4-5-20250929` |
| `openai` | `gpt-4o-mini` |

Set `llm_model` in YAML or `LLM_MODEL` env var to override. Without `llm_provider`, everything works except Structure Proposals: `/retrospect` says the LLM isn't configured and `/stats` says proposals are off.
When `llm_provider=openai`, proposals use the OpenAI-compatible `responses` API.
Set `openai_base_url` / `OPENAI_BASE_URL` when `llm_provider=openai` and you want an OpenAI-compatible endpoint instead of `api.openai.com`.
Set `llm_reasoning_effort` / `LLM_REASONING_EFFORT` (`low`, `medium`, `high`) to send a reasoning effort to an OpenAI-compatible reasoning model; empty sends none.
Set `decision_model_url`, `decision_model_api_key` and `decision_model_name` (or `DECISION_MODEL_*`) for the decision model; requests go to `{decision_model_url}/v1/systemone`. `decision_model_confidence` is the Section Choice confidence needed to file without asking; `similar_item_count` sets how many similar past Work Items the decision model sees.

Evaluated production settings (ship-gate eval on 592 past items):

```yaml
decision_model_name: "clef"
decision_model_confidence: 0.85
similar_item_count: 30
```

At 0.85, about 43% of items were filed automatically, at about 84% accuracy (95% CI 79–89%) against an answer key derived from the previous reports; 0.91 is the lowest setting that reached 90% (27% filed). The rest get "Where does this go?", whose three buttons contained the right Section for 88% of items. Watch `/stats` and revisit the threshold after a few weeks of real corrections.
Set `external_http_timeout_seconds` / `EXTERNAL_HTTP_TIMEOUT_SECONDS` to tune timeout limits for GitLab/GitHub/LLM API requests.
Set `tls_skip_verify` / `TLS_SKIP_VERIFY` to skip TLS certificate verification when connecting to internal or corporate API servers with self-signed or internal CA certificates.
Set `web_enabled` / `WEB_ENABLED` to serve the report editor web UI alongside the Slack bot. Configure `web_client_id`, `web_client_secret` with your Slack app's OAuth credentials and set `web_session_secret` to a random 32-byte hex string for cookie signing. The web UI uses Slack OAuth for authentication and the same `manager_slack_ids` for permissions.

### Cut-over from report-time classification

Run once against the existing database, with the latest published team report:

```bash
./reportbot migrate-v2 --report reports/TEAM_20260925.md --dry-run   # runs on a copy, prints every count
./reportbot migrate-v2 --report reports/TEAM_20260925.md             # seed Taxonomy, import open lines, promote rows
./reportbot migrate-v2 --report reports/TEAM_20260925.md --drop-v1   # back up the DB, then drop V1 tables/columns
```

Imported lines get an owner only through `author_ids`. The run prints a binding report (author → Slack user ID, or "unmapped"); review it before `--drop-v1`. To draft `author_ids`, run `./reportbot migrate-v2 --report <latest.md> --suggest-author-ids`: it prints a YAML block, marked unverified, of exact name matches among `manager_slack_ids` and the Slack user IDs in `team_members`: report author names, plus `gitlab@<source>:<username>` / `github:<login>` lines for the last four weeks' MR authors (annotated with the numeric ID and display name, e.g. `"gitlab@dops:dlee": "U02MEMBER1"  # id 24, Dana Lee`; GitHub keys stay unqualified). Unmatched entries are commented out. It writes nothing, and prints no suggestions if any roster lookup fails.

Each step is idempotent. It seeds the Taxonomy from the report's headings, imports its open authored lines as Work Items with a person Placement, promotes existing rows that match a published line (Placement from the latest report showing them; rows still open in V1 but missing from the latest report are closed as done before that report's week, keeping their V1 status text as the Status Note), queues rows reported after that report for filing, and records fetched MRs/PRs/commits as Code Changes (merged when the row was done, otherwise open). `--dry-run` runs all of this on a temporary copy and prints the counts: Sections, open lines, rows promoted and unmatched, old open rows closed as done, Statuses mapped, Code Changes, rows queued for filing, and the binding report. Until it has run, the web report shows only a "run the cut-over import" banner. `--drop-v1` refuses to run until those steps are done, and only after writing `<db>.<timestamp>.backup`.

### 3. Build & Run

```bash
# Build (requires CGO for SQLite)
CGO_ENABLED=1 go build -o reportbot ./cmd/reportbot

# Run
./reportbot
```

### 4. Docker

```bash
docker build -t reportbot .
```

#### Option A: With config.yaml (recommended)

```bash
docker run -d --name reportbot \
  -v /path/to/config.yaml:/app/config.yaml:ro \
  -v reportbot-data:/app/data \
  -v reports:/app/reports \
  reportbot
```

#### Option B: With environment variables

```bash
docker run -d --name reportbot \
  -e SLACK_BOT_TOKEN=xoxb-... \
  -e SLACK_APP_TOKEN=xapp-... \
  -e DOPS_GITLAB_TOKEN=glpat-... \
  -e LLM_PROVIDER=anthropic \
  -e ANTHROPIC_API_KEY=sk-ant-... \
  -e MANAGER="Member One,Member Two" \
  -e REPORT_CHANNEL_ID=C01234567 \
  -e MONDAY_CUTOFF_TIME=12:00 \
  -e TIMEZONE=America/Los_Angeles \
  -v reportbot-data:/app/data \
  -v reports:/app/reports \
  reportbot
```

The volume persists the SQLite database and generated reports across restarts.

#### Option C: Docker Compose with Web UI (HTTPS via Caddy)

For running the web report editor alongside the Slack bot, use Docker Compose with
the included Caddy reverse proxy for automatic HTTPS (self-signed cert, works with
IP addresses on internal networks):

```bash
# 1. Set environment variables
export SLACK_BOT_TOKEN=xoxb-...
export SLACK_APP_TOKEN=xapp-...
export DOPS_GITLAB_TOKEN=glpat-...
export OPENAI_API_KEY=sk-...
export WEB_HOST=https://192.168.1.100          # your server IP or domain
export WEB_CLIENT_SECRET=your-slack-secret     # from Slack app Basic Information
export WEB_SESSION_SECRET=$(openssl rand -hex 32)

# 2. Configure config.yaml with web settings
#    web_enabled: true
#    web_port: 8088
#    web_client_id: "your-slack-client-id"
#    web_base_url: "https://192.168.1.100"

# 3. Add OAuth redirect URL in Slack app settings:
#    https://192.168.1.100/auth/slack/callback

# 4. Build and run
docker build -t reportbot .
docker-compose --project-name reportbot up -d
```

Caddy handles TLS termination with a self-signed certificate (no internet or domain
required). Your browser will show a certificate warning on first visit — accept it
once. The Slack OAuth flow works because the redirect happens in the browser, not
server-to-server.

## Usage

### Reporting Work Items

Any developer can report items:

```
/report Add pagination to user list API (done)
/rpt Migrate auth service to Redis session store (in progress)
/report Fix flaky integration tests in CI (in testing)
```

Managers can report on behalf of a team member:

```
/report {Member One} Research for agentic AI (in progress)
```

Delegated names support fuzzy matching against `team_members` (for example `{Member}` -> `Member Full Name`).

Status is taken from the trailing parenthetical when it is one of `in progress`, `in testing`, `blocked`, `done` (or a close synonym). Other wording is kept as the Status Note and the decision model picks the Status; with no parenthetical, it decides from the description.

Once the Work Item is saved, the reply says "Saved — filing it…" (or shows the error), then shows where it was filed (Section, the report line, Status, tickets) or asks "Where does this go?".

### Fetching MRs/PRs/commits

Manager only. Pulls merged and open GitLab MRs, GitLab branch commits and/or GitHub PRs for the current calendar week (Monday–Sunday), from every configured `gitlab_sources` entry (in config order, one failing source does not abort the others) plus GitHub:

```
/fetch
```

Already-tracked MRs/PRs/commits are updated, not duplicated; a merge moves a linked `in progress` Work Item to `in testing`. Closed-unmerged ones are skipped. Branch commits become Work Items with Status `done`, owned via the `author_ids` plain name key (commit author name), with tickets parsed from `<ref_ticket_label>: <id>` in the message; merge/revert commits are skipped. Non-team authors are filtered out. An author is on the team if a name entry in `team_members` matches their name or username, or, for a Slack user ID entry, if `author_ids` maps their `gitlab@<source>:` / `gitlab:` / `github:` account key (numeric ID or username; unqualified `gitlab:` resolves against the default source) to that ID or that user's Slack real or display name matches (looked up via `users.info`, cached for an hour; on a lookup failure the filter uses name entries and `author_ids` only and logs a warning). This filter only decides what is imported; owners still come only from `author_ids`.

**Automatic fetching**: Set `auto_fetch_schedule` to a cron expression and MRs/PRs/commits will be imported on a schedule, with a per-source summary posted to `report_channel_id`. Examples:

```yaml
auto_fetch_schedule: "0 9 * * *"     # daily at 9am
auto_fetch_schedule: "0 9 * * 1-5"   # weekdays at 9am
auto_fetch_schedule: "0 9 * * 5"     # Fridays at 9am
```

### Generating Reports

Manager only. If items are still Undetermined, you are warned first (including how many will be filed under the decision model's first choice) and can publish anyway. Once confirmed, unanswered unsure Work Items are filed under the model's first choice and their reporters are told; then the live report is rendered — no model calls — stored as the week's Published Report (re-publishing replaces it), both the team `.md` and boss `.eml` are written, and the requested one is uploaded. With the LLM configured, the week's Structure Proposals are then sent to managers.

```
/generate-report team            # Publish and upload the team markdown (.md) to the channel (default)
/generate-report boss            # Publish and upload the boss email draft (.eml)
/generate-report boss private    # Send it to your DM instead
/generate-report post            # Post latest generated team markdown report to the current channel
/generate-report post private    # Post latest generated team markdown report to your DM
/gen private                     # Publish the team report and send it to your DM
/gen team                # Alias of /generate-report team
```

**Team mode** output:

```markdown
#### Backend

- **Member One** - Add pagination to user list API (done)
- **Member Two** - Optimize database query for dashboard metrics (done)
```

**Boss mode** output:

```markdown
#### Backend (Member One, Member Two)

- Add pagination to user list API (done)
- Optimize database query for dashboard metrics (done)
```

Generated files are saved to `REPORT_OUTPUT_DIR` and uploaded to Slack as files.
Filename date suffix uses Friday of the reporting week, e.g. `TEAMX_20260220.md`.

### Listing Items

By default, `/list` shows only the caller's items on the current Weekly Report (Open Work Items from any week, plus items done this week):

```
/list
```

Managers and members can use `/list all` to see the full team list:

```
/list all
```

`/list` now includes inline actions:
- Members can edit/delete only their own items, matched by Slack user ID (never by name). Fetched and imported items get an owner only through `author_ids` (code changes by GitLab/GitHub numeric account ID or username, `gitlab@<source>` qualified for multi-server setups; branch commits by plain name key; never a display name); items without one are manager-only.
- Managers can edit/delete all items.
- Delete uses a confirmation modal.
- Edit opens a modal with the description, Status, Status Note and Section. Choosing a Section is a person Placement; changing only the text files the item again.

### Nudge Reminders

**Scheduled**: Every week on `nudge_day` (default Friday) at `nudge_time` (default 10:00 AM local), the bot DMs each user in `team_members` reminding them to report. To disable, leave `team_members` empty.

**On-demand**: `/check` lists team members who haven't reported this week, with a "Nudge" button next to each member and a "Nudge All" button at the bottom. Clicking opens a confirmation before sending the DM. It also lists merged code changes waiting in testing, each with a Done button.

The nudge DM lists the member's Open Work Items with Done / status buttons.

**Testing**:

```text
/nudge                 # Send a test nudge to your own DM
/nudge Member Name     # Manager only: send a test nudge to one member
/nudge U123ABC456      # Manager only: target a Slack user ID directly
```

On Monday before `monday_cutoff_time` (default `12:00`) in configured `timezone`, report commands use the previous calendar week.

Accepts any day name: `Monday`, `Tuesday`, ..., `Sunday`.

Requires the `im:write` bot token scope in your Slack app.

## Permissions

Manager commands (`/fetch`, `/generate-report`, `/check`, `/retrospect`, `/stats`) are restricted to Slack user IDs listed in `manager_slack_ids`.

## Report Structure

The Taxonomy (Area → Category → Section, with optional one-line Section descriptions) is stored in the database and edited by managers in the web UI. A Category with no Sections is itself a Section. Within a Section, items are ordered by Status (`in progress`, `in testing`, `blocked`, `done`), then by report time. An Open Work Item appears on every Weekly Report until it is done, then once more in the week it became done.

## Project Structure

```
reportbot/
  cmd/reportbot/main.go     Entry point
  internal/app/             Bootstrap/wiring
  internal/config/          YAML + env var loading, validation, permission checks
  internal/domain/          Core types and calendar/week helpers
  internal/storage/sqlite/  SQLite schema, versioned migrations and queries
  internal/httpx/           Shared external HTTP client/timeout config
  internal/integrations/slack/   Socket Mode bot, slash commands, member resolution helpers
  internal/integrations/github/  GitHub Search API client for merged/open PRs
  internal/integrations/gitlab/  GitLab API client for merged/open MRs and branch commits
  internal/integrations/llm/     LLM prompts and parsing for Structure Proposals
  internal/integrations/decision/ Decision-model client (System One protocol) with retries
  internal/filing/          Filing on arrival: decision-model request, job queue, sweep, 24 h fallback, re-classification
  internal/proposals/       Structure Proposals: evidence, validation, Approve/Reject
  internal/cutover/         One-off migrate-v2 import from published reports
  internal/publish/         Live Weekly Report and Published Report snapshots
  internal/report/          Report view, markdown/EML rendering
  internal/fetch/           Reusable fetch-import logic and cron auto-fetch scheduler
  internal/nudge/           Scheduled and on-demand nudge DM sender
  Dockerfile           Multi-stage Docker build
  docs/adr/            Architecture decision records
```

## CI Coverage Visualization

Code coverage is published automatically by GitHub Actions workflow:
`/.github/workflows/ci-coverage.yml`

- On every PR and push to `main`, CI runs `go test ./...` with coverage.
- The run summary includes total and per-function coverage.
- A downloadable artifact is uploaded for each run:
  - `coverage.out`
  - `coverage.txt`
  - `coverage.html` (line-level visualization)

Optional external dashboard:
- Set repository secret `CODECOV_TOKEN` to enable automatic upload to Codecov.
