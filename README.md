# ReportBot

A Slack bot that helps a development team track weekly work items and generate categorized markdown reports.

Developers report completed work via slash commands. The bot also pulls merged GitLab merge requests automatically. An LLM (Anthropic Claude or OpenAI) classifies items into report categories.

## Features

- `/report` — Developers report work items via Slack
- `/fetch-mrs` — Pull merged GitLab MRs for the current calendar week
- `/generate-report` — AI-categorize items and generate a markdown report
- `/list-items` — View this week's items
- Two report modes: **team** (author per line) and **boss** (authors grouped by category)
- Manager-only permissions for report generation and MR fetching
- **Weekly nudge** — Automatically DMs team members on a configurable day to remind them to report

## Quick Start

### 1. Create a Slack App

1. Go to [api.slack.com/apps](https://api.slack.com/apps) and create a new app
2. Enable **Socket Mode** — generate an App-Level Token with `connections:write` scope
3. Under **OAuth & Permissions**, add these Bot Token Scopes:
   - `chat:write`
   - `commands`
   - `files:write`
   - `im:write` (for Friday nudge DMs)
4. Under **Slash Commands**, create these four commands:

   | Command | Description |
   |---|---|
   | `/report` | Report a completed work item |
   | `/fetch-mrs` | Fetch merged GitLab MRs for this week |
   | `/generate-report` | Generate the weekly report |
   | `/list-items` | List this week's work items |

5. Install the app to your workspace

### 2. Configure

Configuration can be provided via **`config.yaml`** file, **environment variables**, or both. Env vars take precedence over YAML values.

#### Option A: config.yaml (recommended)

Copy the example and edit:

```bash
cp config.yaml.example config.yaml
```

```yaml
# Slack
slack_bot_token: "xoxb-..."
slack_app_token: "xapp-..."

# GitLab
gitlab_url: "https://gitlab.example.com"
gitlab_token: "glpat-..."
gitlab_group_id: "my-team"

# LLM
llm_provider: "anthropic"       # "anthropic" or "openai"
anthropic_api_key: "sk-ant-..."

# Permissions
manager_slack_ids:
  - "U01ABC123"

# Team members (Slack user IDs) - receive nudge reminders
team_members:
  - "U01ABC123"
  - "U02DEF456"

# Day and time to send nudge (local timezone)
nudge_day: "Friday"
nudge_time: "10:00"

# Team name (used in report header and filename)
team_name: "Example Team"

# Report categories (order = section order in report)
categories:
  - "Backend"
  - "Frontend"
  - "Infrastructure"
  - "Bug Fixes"
  - "Documentation"
```

Set `CONFIG_PATH` env var to load from a different path (default: `./config.yaml`).

#### Option B: Environment Variables

```bash
export SLACK_BOT_TOKEN=xoxb-...
export SLACK_APP_TOKEN=xapp-...
export GITLAB_URL=https://gitlab.example.com
export GITLAB_TOKEN=glpat-...
export GITLAB_GROUP_ID=my-team
export LLM_PROVIDER=anthropic
export ANTHROPIC_API_KEY=sk-ant-...
export MANAGER_SLACK_IDS=U01ABC123      # Comma-separated Slack user IDs
```

Note: Categories cannot be configured via env vars — use `config.yaml` for that.

#### LLM Provider Defaults

| Provider | Default Model |
|---|---|
| `anthropic` | `claude-sonnet-4-5-20250929` |
| `openai` | `gpt-4o` |

Set `llm_model` in YAML or `LLM_MODEL` env var to override.

### 3. Build & Run

```bash
# Build (requires CGO for SQLite)
CGO_ENABLED=1 go build -o reportbot .

# Run
./reportbot
```

### 4. Docker

```bash
docker build -t reportbot .

docker run -d --name reportbot \
  -e SLACK_BOT_TOKEN=xoxb-... \
  -e SLACK_APP_TOKEN=xapp-... \
  -e GITLAB_URL=https://gitlab.example.com \
  -e GITLAB_TOKEN=glpat-... \
  -e GITLAB_GROUP_ID=my-team \
  -e LLM_PROVIDER=anthropic \
  -e ANTHROPIC_API_KEY=sk-ant-... \
  -e MANAGER_SLACK_IDS=U01ABC123 \
  -v reportbot-data:/app \
  reportbot
```

The volume persists the SQLite database and generated reports across restarts.

## Usage

### Reporting Work Items

Any developer can report items:

```
/report Add pagination to user list API (done)
/report Migrate auth service to Redis session store (in progress)
/report Fix flaky integration tests in CI (in testing)
```

Status is auto-extracted from the trailing parenthetical. Defaults to `done` if omitted.

### Fetching GitLab Merge Requests

Manager only. Pulls all merged MRs for the current calendar week (Monday–Sunday):

```
/fetch-mrs
```

Duplicates are skipped automatically based on MR URL.

### Generating Reports

Manager only. Two modes:

```
/generate-report team    # Author name on each line (default)
/generate-report boss    # Authors grouped in category header
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

The report is posted to the Slack channel and saved to `REPORT_OUTPUT_DIR`.

### Listing Items

Anyone can view this week's items:

```
/list-items
```

### Weekly Nudge

Every week on `nudge_day` (default Friday) at `nudge_time` (default 10:00 AM local), the bot DMs each user in `team_members` reminding them to report. To disable, leave `team_members` empty.

Accepts any day name: `Monday`, `Tuesday`, ..., `Sunday`.

Requires the `im:write` bot token scope in your Slack app.

## Permissions

Manager commands (`/fetch-mrs`, `/generate-report`) are restricted to Slack user IDs listed in `MANAGER_SLACK_IDS`.

To find your Slack user ID: click your profile picture → **Profile** → click the **⋮** menu → **Copy member ID**.

## Report Categories

Items are auto-classified by the LLM into configured categories. Defaults:

- Backend
- Frontend
- Infrastructure
- Bug Fixes
- Documentation

Customize categories in `config.yaml` under the `categories` key. The order in the list determines the section order in the generated report.

## Project Structure

```
reportbot/
  main.go         Entry point
  config.go       YAML + env var loading, permission check
  config.yaml.example  Example config file
  models.go       WorkItem, GitLabMR types, calendar week helper
  db.go           SQLite schema and CRUD operations
  llm.go          LLM integration (Anthropic + OpenAI), categorization
  gitlab.go       GitLab API client for fetching merged MRs
  report.go       Markdown report generation (team/boss modes)
  slack.go        Slack Socket Mode bot and slash command handlers
  nudge.go        Weekly reminder scheduler and DM sender
  Dockerfile      Multi-stage Docker build
```
