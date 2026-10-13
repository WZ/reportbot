# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

ReportBot is a Slack bot that collects Work Items from developers and files each one into the weekly report **as it arrives**, then publishes the report. Sources: `/report` slash commands, and fetched GitLab MRs, GitLab branch commits and/or GitHub PRs (Code Changes). A decision model (System One protocol) picks the Section and answers Status, ticket and Update questions with calibrated probabilities, in one request per Work Item; unanswered unsure Work Items fall back to its first choice after 24 h; an optional LLM only makes Structure Proposals that managers approve. Domain terms are defined in `CONTEXT.md`; decisions in `docs/adr/`.

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
- **GitLab**: `gitlab_sources` (YAML-only list; env override removed): each entry has `name` (unique, `^[a-z0-9_-]+$`), `url`, `token` (may be `"${ENV_VAR}"`), `ref_ticket_label`, and either `group_id` (fetch all MRs in the group) or `project` + `branch` (fetch commits from that branch; commits become Work Items with Status done, owned via the plain name key, tickets parsed from `<label> <id>` in the message; merge/revert commits skipped). Exactly one source has `default: true` — the source unqualified `gitlab:` author_ids keys resolve against. One failing source does not abort the others.
- **LLM (optional, Structure Proposals only)**: `llm_provider` ("anthropic" or "openai"; empty = no LLM), `llm_model`, `anthropic_api_key` or `openai_api_key`, `openai_base_url`, `llm_reasoning_effort` (low/medium/high for OpenAI-compatible reasoning models; empty sends none)
- **Decision model**: `decision_model_url` (requests go to `{url}/v1/systemone`), `decision_model_api_key`, `decision_model_name`, `decision_model_confidence` (Section Choice confidence needed to file without asking; default 0.85), `similar_item_count` (similar past Work Items it sees; default 30); env `DECISION_MODEL_URL`, `DECISION_MODEL_API_KEY`, `DECISION_MODEL_NAME`, `DECISION_MODEL_CONFIDENCE`, `SIMILAR_ITEM_COUNT`
- **Permissions**: `manager_slack_ids` (list of Slack user IDs) — controls access to `/fetch`, `/generate-report`, `/check`, `/retrospect`, `/stats`
- **Nudge**: `team_members` (list of Slack full names or user IDs; used by `/check`, scheduled nudge and the fetch team filter, which matches ID entries via `author_ids` account keys and the users' Slack profile names), `nudge_day` (Monday-Sunday), `nudge_time` (HH:MM 24h format)
- **Author binding**: `author_ids` (env `AUTHOR_IDS` as JSON; values must be Slack user IDs) is the only way fetched or imported Work Items get an owner (`author_id` grants edit rights and receives DMs). Plain keys are report author names, used by the `migrate-v2` import and by branch commits (exact after trim + case-fold); code-hosting account keys are used only by fetch, in two forms: numeric ID (`"gitlab:24": "U0123ABCD"`) or username/login (`"github:octo-cat": "U0123ABCD"`; letters, digits, `_`, `-`, `.`). GitLab keys may be server-qualified (`gitlab@<source name>:<id|username>`); an unqualified `gitlab:` key resolves against the default source only. Fetch looks up the ID key first, then the username key (exact, case-insensitive), never the display name; if both match with different Slack IDs the ID key wins and a warning is logged. A renamed or reused username must be updated in `author_ids` (ADR 0004). Delegated `/report {Name}` uses the plain keys. Unmapped items are manager-only. `migrate-v2 --suggest-author-ids` prints an unverified suggestion from roster names, with code-hosting accounts as username keys and the numeric ID in a comment (fails closed); `/report` items use the command's own user ID
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
- **internal/storage/sqlite/** — `db.go` versioned additive migrations (`schema_migrations`); `work_items.go` (week membership, status/done_at, Updates), `placements.go` (Placement + `placement_history`, `model_judgments`, corrections, fallback, `/stats`), `edits.go` (edit history, Draft Snapshots), `proposals.go` (Structure Proposals), `taxonomy.go`, `jobs.go` (durable filing queue), `code_changes.go`, `published.go`
- **internal/integrations/llm/** — optional LLM for Structure Proposals only: `proposals.go` prompt and parsing; `providers.go` `Complete()` over Anthropic SDK / OpenAI Responses API
- **internal/proposals/** — Structure Proposals: evidence (Undetermined Work Items, picks outside the model's top 3 including moved Fallback Placements, large and unused Sections), `validate.go` plain-text and no-names rules, 8-week cooldown (same key, or a new Section with mostly the same evidence), 2-week expiry, retire rechecked at approval, Approve/Reject once through the Taxonomy edit paths
- **internal/similar/similar.go** — TF-IDF `Index` of placed Work Items, for the similar items the decision model sees and for Update candidates
- **internal/integrations/decision/decision.go** — decision-model client: one request per Work Item, text-only instructions, retries on 429/5xx/529 with `Retry-After`, 8 in flight
- **internal/filing/** — `filing.go` one filing pass (one decision-model request: Section Choice + Status/ticket/Update questions → Placement/Status/tickets/judgments → notify); `questions.go` exact request state and questions; `text.go` ticket rules; `worker.go` job workers, retries, sweep, re-filing after Taxonomy edits; `fallback.go` Fallback Placement
- **internal/integrations/slack/** — `slack.go` Socket Mode bot and slash commands; `notify.go` filing replies, "Where does this go?", Update/Link/Undo/Done buttons, edit modal; `retro.go` `/retrospect` (generates Structure Proposals on demand), proposal DMs with Approve/Reject, and `/stats`; `slack_users.go` user resolution
- **internal/integrations/gitlab/**, **internal/integrations/github/** — MR/PR clients; GitLab also fetches branch commits (`FetchBranchCommits`, merge/revert filtered, tickets parsed from the commit message)
- **internal/fetch/auto_fetch.go** — records MRs/PRs/commits from every `gitlab_sources` entry plus GitHub as Code Changes (exact-ticket link, merge → in testing, own Work Item otherwise; commits → done Work Items owned by the plain name key; per-source counters, errors isolated per source), cron scheduler
- **internal/publish/publish.go** — live Weekly Report for a week; Published Report snapshot + `.md`/`.eml` files; Friday Draft Snapshot
- **internal/report/** — `report_builder.go` view building and team/boss markdown rendering; `report.go` file/EML writers
- **internal/cutover/** — `migrate-v2`: markdown parser (seed only), import, promotion, backup-then-drop
- **internal/nudge/nudge.go** — scheduled/on-demand DMs listing Open Work Items with Done buttons
- **internal/web/** — chi + Templ + HTMX UI: live report, item edit/move, Taxonomy editor, publish, Published Reports; Slack OAuth sessions; `deps.go` function-var seams

## Key Flows

### Arrival (`/report`, edit with new text, fetched Code Change)

1. `slack.go:handleReport()` parses lines; code maps obvious Status words and leading `[tickets]`, otherwise leaves Status to the model and keeps the wording as the Status Note. `sqlite.InsertWorkItemsForFiling()` saves the item(s) and a `file` job in one transaction, *then* it replies "Saved — filing it…" (or replies with the error if the save failed).
2. `filing.Queue` workers claim the job: `Filer.FileItem()` sends one decision-model request (`filing.BuildRequest`): a `section` Choice over the active Sections (criteria = path + description) with the 30 most similar placed Work Items in `state`, plus Status/ticket/Update questions only when code cannot decide.
3. Choice confidence ≥ `decision_model_confidence` → Placement `model`; else Undetermined and the reporter gets "Where does this go?" (the model's top 3 Sections by probability + Other…). The reply (via `response_url`, else ephemeral, else DM) shows the Section path, the line `**Author** - [tickets] description (status)`, Status/Note and tickets. Raw answers are kept in `model_judgments` (kind `section_choice` with the probability map).
4. Failures retry with backoff (5 attempts); then the item is Undetermined with `model_unreachable` (the author is told once per Work Item, `failure_notified_at`) and the 15-minute sweep re-queues it. Update/Link questions are asked on a Work Item's first successful filing only (arrival, fetch, or a retry of a never-filed item).
5. An edit whose re-filing is unsure keeps the old Section and sends "Where does this go?" buttons.

### Taxonomy edits

Every edit in the web Taxonomy editor (and an approved Structure Proposal) queues one coalesced `reclassify` job: one Section Choice request per Open Work Item (bounded by the decision client's concurrency); confident changes move (person-placed ones too, with an Undo DM).

### Weekly Report and publishing

`sqlite.GetWeekItems()` = open items reported before the week ends + items done during the week. `/generate-report` and web Generate warn about Undetermined items first; once the manager confirms, they file unanswered unsure Work Items by fallback, render the live view (no model calls), store the Published Report and write `.md`/`.eml`; with the LLM configured, `proposals.Service.AfterPublish()` then generates Structure Proposals in the background.

### Fallback, edit history and Draft Snapshots

- **Fallback Placement** — `filing.FileByFallback()`: an item Undetermined with `low_confidence` whose reporter has not picked within 24 h (checked by the 15-minute sweep), or any such item at publish, is filed under the most likely active Section of its stored `section_choice` answer, `placed_by = 'fallback'`, without a model call; the reporter gets "Filed under X automatically" with Change. An answer older than the last Taxonomy change is skipped; the `reclassify` job re-asks such items (subject `reask`, which does not restart the 24 h).
- **Edit history** — `UpdateWorkItemFields`, `SetWorkItemStatus`, `DeleteWorkItemByID`, `ApplyUpdate` and `ConfirmCodeChangeLink` take a `sqlite.Actor` (Slack user ID + a `sqlite.Via*` path; cut-over uses `SnapshotItemTx`/`RecordEditTx`) and append to `work_item_edits` in the same transaction (before/after snapshots; no-ops skipped).
- **Draft Snapshot** — `publish.StartDraftSnapshotScheduler()` stores the rendered Weekly Report in `draft_snapshots` every Friday 09:00 (`timezone`).

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
- Never call the decision model or the LLM on the ack path; filing runs in `internal/filing` workers
- Permission check: `internal/config/config.go:IsManagerID()` checks Slack user ID against `manager_slack_ids`

## GitLab Integration Notes

- Group sources call `/api/v4/groups/:id/merge_requests?state=all&updated_after=<monday>`; branch sources call `/api/v4/projects/:path%2Fescaped/repository/commits?ref_name=<branch>&since=<monday>`
- Paginates through all pages (follows page number)
- Fetches both merged and open MRs; filters client-side by state (`merged` by `merged_at`, `opened` by `updated_at`) within the date range. Commits filter by `committed_date` (fallback `authored_date`) and skip merge/revert titles
- Deduplicates by `code_changes(source, source_ref)` (commit web URL for branch sources)
- Uses `net/http` with `PRIVATE-TOKEN: <source token>` header; sources fetch sequentially in config order, one failure does not abort the rest

## Testing

Automated tests exist for core logic. Run with:

```bash
CGO_ENABLED=1 go test -v ./...
```

Test files (unit tests never make live model calls; the gateway and the LLM are `httptest` fakes):
- **internal/filing/filing_test.go** — arrival filing pass (Section Choice, criteria, de-duplicated similar items, stored `section_choice`), threshold routing with top-3 buttons, Status/ticket/Update questions in one request, retries and "unreachable → Undetermined" (told once), sweep, re-filing after Taxonomy edits with Undo notification, retired Sections, unsure edits keep their Section
- **internal/filing/text_test.go** — status/ticket rules, request building only asks what code cannot decide
- **internal/integrations/decision/decision_test.go** — text-only instructions, answer parsing, 429/5xx/529 retries with `Retry-After`, no retry on 422
- **internal/integrations/llm/*_test.go** — proposal prompt and parsing, reasoning effort, context cancel; **internal/similar/similar_test.go** — TF-IDF index
- **internal/proposals/*_test.go** — Structure Proposals (store and notify, invalid dropped, approve once, cooldown incl. reworded proposals, expiry, stale retire, moved fallbacks as evidence), text validation rules
- **internal/filing/fallback_test.go** — 24 h fallback to the model's #1, retired #1 skipped, publish-time fallback without model calls
- **internal/storage/sqlite/edits_test.go** — edit history (edit, status, delete, Update merge), draft snapshots; **internal/publish/draft_test.go** — Friday 09:00 schedule and draft rendering
- **internal/storage/sqlite/db_test.go** — V1 → V2 migration, week membership and `done_at`, Placement history and corrections, jobs, Taxonomy edits, Code Changes and Updates, Published Report replace
- **internal/cutover/cutover_test.go** — markdown parser on synthetic reports, import/promotion idempotency, backup-before-drop
- **internal/integrations/slack/filing_flow_test.go** — `/report` save-then-queue, replies via `response_url`, placement/Update/Undo/Done buttons, edit modal, publish warning
- **internal/fetch/auto_fetch_test.go** — Code Change linking, merge → in testing, closed skipped, owner binding by ID or username key; branch commits → done Work Items with tickets; a failing source does not abort the others
- **internal/fetch/team_filter_test.go** — team filter: name entries, Slack-ID entries via `author_ids` and profile names, lookup-failure fallback and warning
- **internal/integrations/gitlab/commit_fetch_test.go** — branch commit fetching (escaped project path, week filter, merge/revert skip) and commit ticket parsing
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
- **GitLab 401**: Verify the source's `token` has `read_api` scope and its `group_id`/`project` is accessible
- **Decision model 422 "instructions must be text"**: questions must keep plain-text `instructions`; put data in the request `state`
- **Everything lands in Undetermined with `no_taxonomy`**: run `reportbot migrate-v2` (or add Sections in the web Taxonomy editor)
