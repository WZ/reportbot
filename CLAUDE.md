# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

ReportBot is a Slack bot that collects Work Items from developers and files each one into the weekly report **as it arrives**, then publishes the report. Sources: `/report` slash commands, and fetched GitLab MRs and/or GitHub PRs (Code Changes). A generative LLM (Anthropic or an OpenAI-compatible endpoint) picks the Section; a decision model (System One protocol) checks the pick and answers Status, ticket and Update questions with calibrated probabilities. Domain terms are defined in `CONTEXT.md`; decisions in `docs/adr/`.

## Build and Run

```bash
# Build (requires CGO for SQLite)
CGO_ENABLED=1 go build -o reportbot .

# Run
./reportbot

# One-off cut-over from V1 (idempotent; --drop-v1 backs up the DB first)
./reportbot migrate-v2 --report reports/TEAM_YYYYMMDD.md [--history dir] [--dry-run] [--drop-v1]

# Docker
docker build -t reportbot .
docker run -d --name reportbot \
  -v /path/to/config.yaml:/app/config.yaml:ro \
  -v reportbot-data:/app/data \
  reportbot

# Docker Compose with Web UI (HTTPS via Caddy)
export WEB_HOST=https://192.168.1.100
export WEB_CLIENT_SECRET=xxx
export WEB_SESSION_SECRET=$(openssl rand -hex 32)
docker-compose --project-name reportbot up -d
```

## Configuration

Configuration is layered: `config.yaml` is loaded first, then environment variables override. The config must be set via either method:

- **Slack**: `slack_bot_token` (xoxb-...), `slack_app_token` (xapp-...)
- **GitLab**: `gitlab_url`, `gitlab_token`, `gitlab_group_id` (numeric ID or group path)
- **LLM**: `llm_provider` ("anthropic" or "openai"), `llm_model`, `anthropic_api_key` or `openai_api_key`, `openai_base_url`, `llm_batch_size` (re-classification batches), `llm_example_count`, `llm_example_max_chars` (similar-item context), `llm_reasoning_effort` (low/medium/high for OpenAI-compatible reasoning models; empty sends none)
- **Decision model**: `decision_model_url` (requests go to `{url}/v1/systemone`), `decision_model_api_key`, `decision_model_name`, `decision_model_confidence` (Section-check probability needed to file without asking; default 0.70); env `DECISION_MODEL_URL`, `DECISION_MODEL_API_KEY`, `DECISION_MODEL_NAME`, `DECISION_MODEL_CONFIDENCE`
- **Permissions**: `manager_slack_ids` (list of Slack user IDs) — controls access to `/fetch`, `/generate-report`, `/check`, `/retrospect`, `/stats`
- **Nudge**: `team_members` (list of Slack full names or user IDs; used by `/check` and scheduled nudge), `nudge_day` (Monday-Sunday), `nudge_time` (HH:MM 24h format)
- **Author binding**: `author_ids` (env `AUTHOR_IDS` as JSON; values must be Slack user IDs) is the only way fetched or imported Work Items get an owner (`author_id` grants edit rights and receives DMs). Plain keys are report author names, used only by the `migrate-v2` import (exact after trim + case-fold); `gitlab:<numeric id>` / `github:<numeric id>` keys are code-hosting accounts, used only by fetch, which binds by the immutable account ID and never by display name or username. Delegated `/report {Name}` uses the plain keys. Unmapped items are manager-only. `migrate-v2 --suggest-author-ids` prints an unverified suggestion from roster names (fails closed); `/report` items use the command's own user ID
- **Auto-fetch**: `auto_fetch_schedule` (5-field cron expression, e.g. `"0 9 * * 1-5"` for weekdays at 9am; empty to disable)
- **Network**: `tls_skip_verify` (bool, skip TLS cert verification for internal/corporate CAs; default false)
- **Team**: `team_name` (used in report header and filename)
- **Web UI**: `web_enabled` (bool), `web_port` (int, default 8080), `web_client_id`, `web_client_secret` (Slack OAuth), `web_session_secret` (cookie HMAC key), `web_base_url` (OAuth redirect base)

See `config.yaml` and `README.md` for full reference.

## Architecture

cmd/internal layout; the executable is `cmd/reportbot`, core logic is under `internal/*`:

- **cmd/reportbot/main.go** → **internal/app/app.go** — loads config, runs migrations, starts the filing queue, nudge and auto-fetch schedulers, the web UI, and the Socket Mode bot; `migrate-v2` sub-command runs the cut-over
- **internal/config/config.go** — Config struct, YAML + env loading with validation, `IsManagerID()`
- **internal/domain/models.go** — `WorkItem` (Status, Status Note, `DoneAt`, Placement fields), `CodeChange`, `PlacedItem`, Status order, filing states, `ReportWeekRange()`
- **internal/domain/taxonomy.go** — `Taxonomy` tree (Area → Category → Section), fileable `Sections()`, `Section.Path()`
- **internal/storage/sqlite/** — `db.go` versioned additive migrations (`schema_migrations`); `work_items.go` (week membership, status/done_at, Updates), `placements.go` (Placement + `placement_history`, `model_judgments`, corrections, `/stats`), `taxonomy.go`, `jobs.go` (durable filing queue), `code_changes.go`, `published.go`
- **internal/integrations/llm/** — `section.go` Section prompt (stable `S<id>` keys, similar items, corrections) + OpenAI JSON schema + parser; `retro.go` Section-description suggestions; `llm_examples.go` TF-IDF `SimilarIndex`; `providers.go` Anthropic SDK / OpenAI Responses API
- **internal/integrations/decision/decision.go** — decision-model client: one request per Work Item, text-only instructions, retries on 429/5xx/529 with `Retry-After`, 8 in flight
- **internal/filing/** — `filing.go` one filing pass (LLM pick → one decision-model request → Placement/Status/tickets/judgments → notify); `questions.go` exact request state and questions; `text.go` status/ticket rules; `worker.go` job workers, retries, sweep, re-classification after Taxonomy edits
- **internal/integrations/slack/** — `slack.go` Socket Mode bot and slash commands; `notify.go` filing replies, "Where does this go?", Update/Link/Undo/Done buttons, edit modal; `retro.go` `/retrospect` and `/stats`; `slack_users.go` user resolution
- **internal/integrations/gitlab/**, **internal/integrations/github/** — MR/PR clients
- **internal/fetch/auto_fetch.go** — records MRs/PRs as Code Changes (exact-ticket link, merge → in testing, own Work Item otherwise), cron scheduler
- **internal/publish/publish.go** — live Weekly Report for a week; Published Report snapshot + `.md`/`.eml` files
- **internal/report/** — `report_builder.go` view building and team/boss markdown rendering; `report.go` file/EML writers
- **internal/cutover/** — `migrate-v2`: markdown parser (seed only), import, promotion, backup-then-drop
- **internal/nudge/nudge.go** — scheduled/on-demand DMs listing Open Work Items with Done buttons
- **internal/web/** — chi + Templ + HTMX UI: live report, item edit/move, Taxonomy editor, publish, Published Reports; Slack OAuth sessions; `deps.go` function-var seams

## Key Flows

### Arrival (`/report`, edit with new text, fetched Code Change)

1. `slack.go:handleReport()` parses lines; code maps obvious Status words and leading `[tickets]`, otherwise leaves Status to the model and keeps the wording as the Status Note. It replies "Saved — filing it…" and `sqlite.InsertWorkItemsForFiling()` saves the item(s) and a `file` job in one transaction.
2. `filing.Queue` workers claim the job: `Filer.FileItem()` runs one LLM call (`llm.ClassifySections`), then one decision-model request (`filing.BuildRequest`: Section check + Status/ticket/Update questions as needed).
3. Check probability ≥ `decision_model_confidence` → Placement `model`; else Undetermined and the reporter gets "Where does this go?" (LLM pick + top 2 by similar-item vote). The reply (via `response_url`, else ephemeral, else DM) shows the Section path, the line `**Author** - [tickets] description (status)`, Status/Note and tickets.
4. Failures retry with backoff (5 attempts); then the item is Undetermined with `model_unreachable` and the 15-minute sweep re-queues it.

### Taxonomy edits

Every edit in the web Taxonomy editor (and an applied `/retrospect` description) queues one coalesced `reclassify` job: LLM picks in batches of `llm_batch_size`, the decision model checks changed picks, passing items move (person-placed ones too, with an Undo DM).

### Weekly Report and publishing

`sqlite.GetWeekItems()` = open items reported before the week ends + items done during the week. `/generate-report` and web Generate render the live view (no model calls), warn about Undetermined items, store the Published Report and write `.md`/`.eml`.

### Calendar Week Calculation

`ReportWeekRange()` returns Monday 00:00 and next Monday 00:00 for the reporting week; before `monday_cutoff_time` on a Monday it is the previous week. Timestamps are stored with mixed UTC offsets, so queries compare them with `julianday()`.

### Nudge Reminders

**Scheduled**: `internal/nudge/nudge.go:StartNudgeScheduler()` DMs all `team_members` on `nudge_day` at `nudge_time`, listing their Open Work Items with Done/status buttons. Disabled if `team_members` is empty.

**On-demand**: `/check` shows missing members with "Nudge"/"Nudge All" buttons (confirmation modal → `sendNudges()`), and merged Code Changes waiting in testing with Done buttons.

### Auto-Fetch Scheduler

`internal/fetch/auto_fetch.go:StartAutoFetchScheduler()` parses `auto_fetch_schedule` (5-field cron), calls `FetchAndImportMRs()` and posts a summary to `report_channel_id`. Disabled when empty or when neither GitLab nor GitHub is configured.

## Slack Integration Notes

- Uses **Socket Mode** (WebSocket, not HTTP webhooks)
- Slash commands must be **acked immediately** to avoid Slack's 3-second timeout
- All command processing happens in goroutines after ack
- Responses use **ephemeral messages** for feedback, except `/generate-report` which uploads the report to the channel (or DMs it to the caller with the `private` argument)
- Never call the LLM or the decision model on the ack path; filing runs in `internal/filing` workers
- Permission check: `internal/config/config.go:IsManagerID()` checks Slack user ID against `manager_slack_ids`

## GitLab Integration Notes

- Calls `/api/v4/groups/:id/merge_requests?state=all&updated_after=<monday>`
- Paginates through all pages (follows page number)
- Fetches both merged and open MRs; filters client-side by state (`merged` by `merged_at`, `opened` by `updated_at`) within the date range
- Deduplicates by `code_changes(source, source_ref)`
- Uses `net/http` with `PRIVATE-TOKEN: <gitlab_token>` header

## Testing

Automated tests exist for core logic. Run with:

```bash
CGO_ENABLED=1 go test -v ./...
```

Test files (unit tests never make live model calls; the LLM and the gateway are `httptest` fakes):
- **internal/filing/filing_test.go** — arrival filing pass, threshold routing, LLM `UND`, Status/ticket/Update questions in one request, retries and "unreachable → Undetermined", sweep, re-classification with Undo notification, retired Sections
- **internal/filing/text_test.go** — status/ticket rules, request building only asks what code cannot decide
- **internal/integrations/decision/decision_test.go** — text-only instructions, answer parsing, 429/5xx/529 retries with `Retry-After`, no retry on 422
- **internal/integrations/llm/llm_test.go**, **llm_examples_test.go** — Section prompt, schema, response parsing, OpenAI structured path, retro parsing; TF-IDF index
- **internal/storage/sqlite/db_test.go** — V1 → V2 migration, week membership and `done_at`, Placement history and corrections, jobs, Taxonomy edits, Code Changes and Updates, Published Report replace
- **internal/cutover/cutover_test.go** — markdown parser on synthetic reports, import/promotion idempotency, backup-before-drop
- **internal/integrations/slack/filing_flow_test.go** — `/report` save-then-queue, replies via `response_url`, placement/Update/Undo/Done buttons, edit modal, publish warning
- **internal/fetch/auto_fetch_test.go** — Code Change linking, merge → in testing, closed skipped
- **internal/publish/publish_test.go**, **internal/report/report_builder_test.go** — Published Report snapshot, view building and rendering
- **internal/web/handlers/*_test.go** — live report, permissions, edit/move, Taxonomy editor, publish

Manual testing for Slack integration:

1. Set up a test Slack workspace with the app installed
2. Configure `config.yaml` with test tokens and a single-user `team_members` list
3. Run `/report Test item (done)` → verify the "Saved" reply is replaced by the filed reply
4. Run `/list` → verify item appears
5. Run `/fetch` → verify GitLab API call and MR import
6. Run `/generate-report team` and `/generate-report boss` → check the Undetermined warning and both outputs
7. Run `/check` → verify missing members list with nudge buttons, click Nudge → verify confirmation modal and DM

## Common Issues

- **`anthropic.Model` type error**: Cast string model name with `anthropic.Model(model)`
- **SQLite CGO disabled**: Must build with `CGO_ENABLED=1`
- **Slash commands not visible**: Reinstall Slack app after creating commands
- **Permission denied on `/fetch`**: Add user's Slack user ID to `manager_slack_ids` in config
- **Scheduled nudge not firing**: Check logs for "Next nudge at..." message, verify `team_members` is not empty
- **GitLab 401**: Verify `gitlab_token` has `read_api` scope and `gitlab_group_id` is accessible
- **Decision model 422 "instructions must be text"**: questions must keep plain-text `instructions`; put data in the request `state`
- **Everything lands in Undetermined with `no_taxonomy`**: run `reportbot migrate-v2` (or add Sections in the web Taxonomy editor)
