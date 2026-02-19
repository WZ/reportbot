# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

ReportBot is a Slack bot that collects work items from developers and generates weekly markdown reports. It has two data sources: manual `/report` slash commands from developers, and automated fetching of GitLab MRs and/or GitHub PRs (either or both can be configured). All items are AI-categorized using Anthropic Claude or OpenAI.

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
- **LLM**: `llm_provider` ("anthropic" or "openai"), `anthropic_api_key` or `openai_api_key`, `llm_critic_enabled` (bool, enables generator-critic second pass)
- **Permissions**: `manager_slack_ids` (list of Slack user IDs) — controls access to `/fetch`, `/generate-report`, `/check`, `/retrospect`, `/stats`
- **Nudge**: `team_members` (list of Slack full names or user IDs; used by `/check` and scheduled nudge), `nudge_day` (Monday-Sunday), `nudge_time` (HH:MM 24h format)
- **Auto-fetch**: `auto_fetch_schedule` (5-field cron expression, e.g. `"0 9 * * 1-5"` for weekdays at 9am; empty to disable)
- **Report**: `report_private` (bool, when true `/generate-report` DMs the report to the caller instead of posting to the channel; default false)
- **Team**: `team_name` (used in report header and filename)

See `config.yaml` and `README.md` for full reference.

## Architecture

The application uses a cmd/internal layout with the executable under `cmd/reportbot` and core logic under `internal/reportbot`:

- **cmd/reportbot/main.go** — Entry point: loads config, initializes DB, creates Slack client, starts nudge and auto-fetch schedulers, starts Socket Mode bot
- **internal/reportbot/config.go** — Config struct, YAML + env loading with validation, `IsManagerID()` permission check
- **internal/reportbot/models.go** — Core types (`WorkItem`, `GitLabMR`, `GitHubPR`, `ReportSection`) and `CurrentWeekRange()` calendar week calculator
- **internal/reportbot/db.go** — SQLite schema and CRUD: `work_items`, `classification_history`, `classification_corrections` tables
- **internal/reportbot/slack.go** — Socket Mode bot, slash command handlers (`/report`, `/fetch`, `/generate-report`, `/list`, `/check`, `/retrospect`, `/stats`, `/help`), nudge confirmation modals, edit/delete modals, uncertainty sampling, correction capture
- **internal/reportbot/slack_users.go** — User resolution helpers: Slack API lookups, name matching, team member ID resolution
- **internal/reportbot/gitlab.go** — GitLab API client: fetches merged and open MRs for a date range with pagination, filters by state and date client-side
- **internal/reportbot/github.go** — GitHub Search API client: fetches merged and open PRs for a date range, converts to `GitHubPR` structs
- **internal/reportbot/auto_fetch.go** — Reusable `FetchAndImportMRs()` function (used by `/fetch` and scheduler), `FetchResult` counters, `FormatFetchSummary()`, cron-based `StartAutoFetchScheduler()`
- **internal/reportbot/llm.go** — AI classification: parallel batch processing, Anthropic (SDK) / OpenAI (HTTP), prompt caching, generator-critic loop, prompt building with corrections and glossary, retrospective analysis
- **internal/reportbot/llm_examples.go** — TF-IDF index for relevance-based few-shot example selection from classification history
- **internal/reportbot/glossary.go** — Glossary loading from YAML, auto-growth from repeated corrections, phrase extraction
- **internal/reportbot/report_builder.go** — Template parsing, LLM classification pipeline, merge logic, status ordering, markdown rendering (team + boss modes)
- **internal/reportbot/report.go** — Report file writing (markdown `.md` and email draft `.eml`) to disk
- **internal/reportbot/nudge.go** — Scheduled weekly reminder and DM sender (`sendNudges` also used by `/check` nudge buttons)

## Key Flows

### Work Item Lifecycle

1. Developer reports via `/report` → `internal/reportbot/slack.go:handleReport()` → `internal/reportbot/db.go:InsertWorkItem()` with `source="slack"`
2. Manager runs `/fetch` (or auto-fetch scheduler fires) → `internal/reportbot/auto_fetch.go:FetchAndImportMRs()` → `internal/reportbot/gitlab.go:FetchMRs()` / `internal/reportbot/github.go:FetchGitHubPRs()` → batch insert with `source="gitlab"` or `source="github"`, deduped by `source_ref` (MR/PR URL)
3. Manager runs `/generate-report` → `internal/reportbot/slack.go:handleGenerateReport()`:
   - Load items for current calendar week (Monday-Sunday via `internal/reportbot/models.go:ReportWeekRange()`)
   - `internal/reportbot/report_builder.go:BuildReportsFromLast()` loads last report as template, classifies items via `internal/reportbot/llm.go:CategorizeItemsToSections()` in parallel batches, merges into template
   - Persists classification history and renders markdown (team or boss mode)
   - Uploads report file to Slack, sends uncertainty sampling messages for low-confidence items

### Calendar Week Calculation

`CurrentWeekRange()` returns Monday 00:00:00 and next Monday 00:00:00 for the current calendar week. Sunday is treated as day 7 (end of week). All date range queries use `reported_at >= monday AND reported_at < nextMonday`.

### AI Categorization

`internal/reportbot/llm.go:CategorizeItemsToSections()` classifies items into report sections using parallel LLM batches. Few-shot examples are selected via TF-IDF similarity (`internal/reportbot/llm_examples.go`) from 12 weeks of classification history, replacing the previous blind "first N items" approach. The system prompt (cached via Anthropic prompt caching) lists valid section IDs (derived from the previous report template), instructions for classification, status normalization, ticket extraction, duplicate detection, and confidence scoring. Recent corrections (last 4 weeks) are injected as negative examples. Glossary overrides are applied post-classification. When `llm_critic_enabled` is set, a second LLM pass reviews all assignments and corrects misclassifications before returning. Response format: `[{"id": 142, "section_id": "S0_2", "normalized_status": "in progress", "ticket_ids": "1234", "duplicate_of": "", "confidence": 0.91}]`.

### Nudge Reminders

**Scheduled**: `internal/reportbot/nudge.go:StartNudgeScheduler()` launches a goroutine that calculates the next occurrence of `nudge_day` at `nudge_time` using `nextWeekday()`, sleeps until then, DMs all `team_members`, then repeats. Disabled if `team_members` is empty.

**On-demand**: `/check` shows missing members as Block Kit sections with per-member "Nudge" buttons and a "Nudge All" button. Clicking opens a confirmation modal; on submit, `internal/reportbot/nudge.go:sendNudges()` sends the DM.

### Auto-Fetch Scheduler

`internal/reportbot/auto_fetch.go:StartAutoFetchScheduler()` uses `robfig/cron/v3` to parse `auto_fetch_schedule` (a standard 5-field cron expression). When the schedule fires, it calls `FetchAndImportMRs()` and posts a summary to `report_channel_id`. Disabled when the config field is empty or when neither GitLab nor GitHub is configured. Example schedules: `"0 9 * * *"` (daily 9am), `"0 9 * * 1-5"` (weekdays 9am).

## Slack Integration Notes

- Uses **Socket Mode** (WebSocket, not HTTP webhooks)
- Slash commands must be **acked immediately** to avoid Slack's 3-second timeout
- All command processing happens in goroutines after ack
- Responses use **ephemeral messages** (`PostEphemeral`) for feedback, except `/generate-report` which posts the full report to the channel (or DMs it to the caller when `report_private` is true)
- Permission check: `internal/reportbot/config.go:IsManagerID()` checks Slack user ID against `manager_slack_ids`

## GitLab Integration Notes

- Calls `/api/v4/groups/:id/merge_requests?state=all&updated_after=<monday>`
- Paginates through all pages (follows page number)
- Fetches both merged and open MRs; filters client-side by state (`merged` by `merged_at`, `opened` by `updated_at`) within the date range
- Deduplicates by checking `internal/reportbot/db.go:SourceRefExists()` before inserting
- Uses `net/http` with `PRIVATE-TOKEN: <gitlab_token>` header

## Testing

Automated tests exist for core logic. Run with:

```bash
CGO_ENABLED=1 go test -v ./...
```

Test files:
- **internal/reportbot/report_builder_test.go** — Template parsing, merge/sort, LLM confidence gating, duplicate detection, prefix/heading preservation, item formatting
- **internal/reportbot/llm_test.go** — Glossary overrides, prompt building (example limits, template guidance), JSON response parsing (array ticket IDs), critic response parsing
- **internal/reportbot/llm_examples_test.go** — TF-IDF index building, topK similarity search, batch deduplication, cosine similarity edge cases
- **internal/reportbot/models_test.go** — `ReportWeekRange` Monday cutoff logic
- **internal/reportbot/auto_fetch_test.go** — `FormatFetchSummary` output formatting, `FetchAndImportMRs` error when neither source configured
- **internal/reportbot/github_test.go** — GitHub Search API scope building, PR conversion, status mapping, `prReportedAt` fallback logic

Manual testing for Slack integration:

1. Set up a test Slack workspace with the app installed
2. Configure `config.yaml` with test tokens and a single-user `team_members` list
3. Run `/report Test item (done)` → verify DB insert
4. Run `/list` → verify item appears
5. Run `/fetch` → verify GitLab API call and MR import
6. Run `/generate-report team` and `/generate-report boss` → compare output formats
7. Run `/check` → verify missing members list with nudge buttons, click Nudge → verify confirmation modal and DM

## Common Issues

- **`anthropic.Model` type error**: Cast string model name with `anthropic.Model(model)`
- **SQLite CGO disabled**: Must build with `CGO_ENABLED=1`
- **Slash commands not visible**: Reinstall Slack app after creating commands
- **Permission denied on `/fetch`**: Add user's Slack user ID to `manager_slack_ids` in config
- **Scheduled nudge not firing**: Check logs for "Next nudge at..." message, verify `team_members` is not empty
- **GitLab 401**: Verify `gitlab_token` has `read_api` scope and `gitlab_group_id` is accessible
