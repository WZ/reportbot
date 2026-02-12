# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

ReportBot is a Slack bot that collects work items from developers and generates weekly markdown reports. It has two data sources: manual `/report` slash commands from developers, and automated GitLab merge request fetching. All items are AI-categorized using Anthropic Claude or OpenAI.

## Build and Run

```bash
# Build (requires CGO for SQLite)
CGO_ENABLED=1 go build -o reportbot .

# Run
./reportbot

# Docker
docker build -t reportbot .
docker run -d --name reportbot \
  -v /path/to/config.yaml:/app/config.yaml:ro \
  -v reportbot-data:/app/data \
  reportbot
```

## Configuration

Configuration is layered: `config.yaml` is loaded first, then environment variables override. The config must be set via either method:

- **Slack**: `slack_bot_token` (xoxb-...), `slack_app_token` (xapp-...)
- **GitLab**: `gitlab_url`, `gitlab_token`, `gitlab_group_id` (numeric ID or group path)
- **LLM**: `llm_provider` ("anthropic" or "openai"), `anthropic_api_key` or `openai_api_key`
- **Permissions**: `manager_slack_ids` (list of Slack user IDs allowed to run `/fetch-mrs` and `/generate-report`)
- **Nudge**: `team_members` (list of Slack user IDs to DM), `nudge_day` (Monday-Sunday), `nudge_time` (HH:MM 24h format)
- **Categories**: `categories` (list, defines report section order and AI classification options)
- **Team**: `team_name` (used in report header and filename)

See `config.yaml.example` for full reference.

## Architecture

The application has a flat structure with 9 Go files:

- **main.go** — Entry point: loads config, initializes DB, creates Slack client, starts nudge scheduler and Socket Mode bot
- **config.go** — Config struct, YAML + env loading with validation, `IsManager()` permission check
- **models.go** — Core types (`WorkItem`, `GitLabMR`, `ReportSection`) and `CurrentWeekRange()` calendar week calculator
- **db.go** — SQLite schema and CRUD: single `work_items` table with indexes on `reported_at` and `author`
- **slack.go** — Socket Mode bot, slash command handlers (`/report`, `/fetch-mrs`, `/generate-report`, `/list-items`)
- **gitlab.go** — GitLab API client: fetches merged MRs for a date range with pagination, filters by `merged_at`
- **llm.go** — AI categorization dispatcher: calls Anthropic (SDK) or OpenAI (raw HTTP), parses JSON response
- **report.go** — Markdown generation: two modes ("team" with author per line, "boss" with authors in header), writes to disk
- **nudge.go** — Weekly reminder scheduler: calculates next occurrence of configured weekday/time, DMs all team members

## Key Flows

### Work Item Lifecycle

1. Developer reports via `/report` → `slack.go:handleReport()` → `db.go:InsertWorkItem()` with `source="slack"`
2. Manager runs `/fetch-mrs` → `slack.go:handleFetchMRs()` → `gitlab.go:FetchMergedMRs()` → batch insert with `source="gitlab"`, deduped by `source_ref` (MR URL)
3. Manager runs `/generate-report` → `slack.go:handleGenerateReport()`:
   - Load items for current calendar week (Monday-Sunday via `models.go:CurrentWeekRange()`)
   - Send uncategorized items to `llm.go:CategorizeItems()` in a single batch
   - Update `category` and `ticket_ids` columns in DB
   - `report.go:GenerateReport()` formats markdown, posts to Slack, saves file

### Calendar Week Calculation

`CurrentWeekRange()` returns Monday 00:00:00 and next Monday 00:00:00 for the current calendar week. Sunday is treated as day 7 (end of week). All date range queries use `reported_at >= monday AND reported_at < nextMonday`.

### AI Categorization

`llm.go:CategorizeItems()` sends all uncategorized items in a single LLM call. The system prompt lists configured categories and asks for JSON output: `[{"id": 123, "category": "Backend", "ticket_ids": "1234,5678"}]`. Ticket IDs are extracted from descriptions by the LLM.

### Nudge Scheduler

`nudge.go:StartNudgeScheduler()` launches a goroutine that calculates the next occurrence of `nudge_day` at `nudge_time` using `nextWeekday()`, sleeps until then, DMs all `team_members`, then repeats. Disabled if `team_members` is empty.

## Slack Integration Notes

- Uses **Socket Mode** (WebSocket, not HTTP webhooks)
- Slash commands must be **acked immediately** to avoid Slack's 3-second timeout
- All command processing happens in goroutines after ack
- Responses use **ephemeral messages** (`PostEphemeral`) for feedback, except `/generate-report` which posts the full report to the channel
- Permission check: `config.go:IsManager()` compares `cmd.UserID` against `manager_slack_ids`

## GitLab Integration Notes

- Calls `/api/v4/groups/:id/merge_requests?state=merged&updated_after=<monday>`
- Paginates through all pages (follows `Link` header)
- Filters results by `merged_at` within the date range (client-side, since `updated_after` is not precise)
- Deduplicates by checking `db.go:SourceRefExists()` before inserting
- Uses `net/http` with Basic Auth header: `PRIVATE-TOKEN: <gitlab_token>`

## Testing Strategy

No automated tests exist. Manual testing workflow:

1. Set up a test Slack workspace with the app installed
2. Configure `config.yaml` with test tokens and a single-user `team_members` list
3. Run `/report Test item (done)` → verify DB insert
4. Run `/list-items` → verify item appears
5. Run `/fetch-mrs` → verify GitLab API call and MR import
6. Run `/generate-report team` and `/generate-report boss` → compare output formats
7. Set `nudge_time` to 2 minutes from now, verify DM arrives

## Common Issues

- **`anthropic.Model` type error**: Cast string model name with `anthropic.Model(model)`
- **SQLite CGO disabled**: Must build with `CGO_ENABLED=1`
- **Slash commands not visible**: Reinstall Slack app after creating commands
- **Permission denied on `/fetch-mrs`**: Add user's Slack ID (not username) to `manager_slack_ids`
- **Nudge not firing**: Check logs for "Next nudge at..." message, verify `team_members` is not empty
- **GitLab 401**: Verify `gitlab_token` has `read_api` scope and `gitlab_group_id` is accessible
