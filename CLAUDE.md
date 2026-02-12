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
- **Permissions**: `manager_slack_ids` (list of Slack user IDs, recommended) or `manager` (list of Slack full names, legacy fallback) — controls access to `/fetch-mrs`, `/generate-report`, `/check`, `/nudge`, `/retrospective`
- **Nudge**: `team_members` (list of Slack user IDs to DM), `nudge_day` (Monday-Sunday), `nudge_time` (HH:MM 24h format)
- **Categories**: `categories` (list, defines report section order and AI classification options)
- **Team**: `team_name` (used in report header and filename)

See `config.yaml.example` for full reference.

## Architecture

The application has a flat structure with 12 Go source files (+ 3 test files):

- **main.go** — Entry point: loads config, initializes DB, creates Slack client, starts nudge scheduler and Socket Mode bot
- **config.go** — Config struct, YAML + env loading with validation, `IsManagerName()` permission check
- **models.go** — Core types (`WorkItem`, `GitLabMR`, `ReportSection`) and `CurrentWeekRange()` calendar week calculator
- **db.go** — SQLite schema and CRUD: `work_items`, `classification_history`, `classification_corrections` tables
- **slack.go** — Socket Mode bot, slash command handlers (`/report`, `/fetch-mrs`, `/generate-report`, `/list`, `/check`, `/nudge`, `/retrospective`, `/help`), edit/delete modals, uncertainty sampling, correction capture
- **slack_users.go** — User resolution helpers: Slack API lookups, name matching, team member ID resolution
- **gitlab.go** — GitLab API client: fetches merged and open MRs for a date range with pagination, filters by state and date client-side
- **llm.go** — AI classification: parallel batch processing, Anthropic (SDK) / OpenAI (HTTP), prompt building with corrections and glossary, retrospective analysis
- **glossary.go** — Glossary loading from YAML, auto-growth from repeated corrections, phrase extraction
- **report_builder.go** — Template parsing, LLM classification pipeline, merge logic, status ordering, markdown rendering (team + boss modes)
- **report.go** — Report file writing (markdown `.md` and email draft `.eml`) to disk
- **nudge.go** — Weekly reminder scheduler: calculates next occurrence of configured weekday/time, DMs all team members

## Key Flows

### Work Item Lifecycle

1. Developer reports via `/report` → `slack.go:handleReport()` → `db.go:InsertWorkItem()` with `source="slack"`
2. Manager runs `/fetch-mrs` → `slack.go:handleFetchMRs()` → `gitlab.go:FetchMRs()` → batch insert with `source="gitlab"`, deduped by `source_ref` (MR URL)
3. Manager runs `/generate-report` → `slack.go:handleGenerateReport()`:
   - Load items for current calendar week (Monday-Sunday via `models.go:ReportWeekRange()`)
   - `report_builder.go:BuildReportsFromLast()` loads last report as template, classifies items via `llm.go:CategorizeItemsToSections()` in parallel batches, merges into template
   - Persists classification history and renders markdown (team or boss mode)
   - Uploads report file to Slack, sends uncertainty sampling messages for low-confidence items

### Calendar Week Calculation

`CurrentWeekRange()` returns Monday 00:00:00 and next Monday 00:00:00 for the current calendar week. Sunday is treated as day 7 (end of week). All date range queries use `reported_at >= monday AND reported_at < nextMonday`.

### AI Categorization

`llm.go:CategorizeItemsToSections()` classifies items into report sections using parallel LLM batches. The system prompt lists valid section IDs (derived from the previous report template), instructions for classification, status normalization, ticket extraction, duplicate detection, and confidence scoring. Recent corrections (last 4 weeks) are injected as negative examples. Glossary overrides are applied post-classification. Response format: `[{"id": 142, "section_id": "S0_2", "normalized_status": "in progress", "ticket_ids": "1234", "duplicate_of": "", "confidence": 0.91}]`.

### Nudge Scheduler

`nudge.go:StartNudgeScheduler()` launches a goroutine that calculates the next occurrence of `nudge_day` at `nudge_time` using `nextWeekday()`, sleeps until then, DMs all `team_members`, then repeats. Disabled if `team_members` is empty.

## Slack Integration Notes

- Uses **Socket Mode** (WebSocket, not HTTP webhooks)
- Slash commands must be **acked immediately** to avoid Slack's 3-second timeout
- All command processing happens in goroutines after ack
- Responses use **ephemeral messages** (`PostEphemeral`) for feedback, except `/generate-report` which posts the full report to the channel
- Permission check: `config.go:IsManagerID()` checks Slack user ID against `manager_slack_ids` first, then `IsManagerName()` falls back to name matching against `manager` list

## GitLab Integration Notes

- Calls `/api/v4/groups/:id/merge_requests?state=all&updated_after=<monday>`
- Paginates through all pages (follows page number)
- Fetches both merged and open MRs; filters client-side by state (`merged` by `merged_at`, `opened` by `updated_at`) within the date range
- Deduplicates by checking `db.go:SourceRefExists()` before inserting
- Uses `net/http` with `PRIVATE-TOKEN: <gitlab_token>` header

## Testing

Automated tests exist for core logic. Run with:

```bash
CGO_ENABLED=1 go test -v ./...
```

Test files:
- **report_builder_test.go** — Template parsing, merge/sort, LLM confidence gating, duplicate detection, prefix/heading preservation, item formatting
- **llm_test.go** — Glossary overrides, prompt building (example limits, template guidance), JSON response parsing (array ticket IDs)
- **models_test.go** — `ReportWeekRange` Monday cutoff logic

Manual testing for Slack integration:

1. Set up a test Slack workspace with the app installed
2. Configure `config.yaml` with test tokens and a single-user `team_members` list
3. Run `/report Test item (done)` → verify DB insert
4. Run `/list` → verify item appears
5. Run `/fetch-mrs` → verify GitLab API call and MR import
6. Run `/generate-report team` and `/generate-report boss` → compare output formats
7. Set `nudge_time` to 2 minutes from now, verify DM arrives

## Common Issues

- **`anthropic.Model` type error**: Cast string model name with `anthropic.Model(model)`
- **SQLite CGO disabled**: Must build with `CGO_ENABLED=1`
- **Slash commands not visible**: Reinstall Slack app after creating commands
- **Permission denied on `/fetch-mrs`**: Add user's Slack full name to `manager` list in config
- **Nudge not firing**: Check logs for "Next nudge at..." message, verify `team_members` is not empty
- **GitLab 401**: Verify `gitlab_token` has `read_api` scope and `gitlab_group_id` is accessible
