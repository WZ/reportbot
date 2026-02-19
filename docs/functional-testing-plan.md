# Functional Testing Plan (Slack + GitHub)

## Goal
Validate end-to-end behavior for command handling and PR ingestion with realistic API interactions while keeping tests deterministic.

## Local Functional Suite (CI-safe)

### 1. Slack command flow: `/report`
- Use a mock Slack Web API server for:
  - `users.info`
  - `chat.postEphemeral`
- Execute `handleReport(...)` with a real `slack.Client` pointing to the mock API URL.
- Assert:
  - item is persisted in SQLite
  - author is resolved from `users.info`
  - ephemeral feedback is posted

### 2. GitHub fetch flow: `/fetch`
- Redirect `api.github.com` requests to an `httptest` GitHub mock.
- Mock both queries:
  - `is:merged`
  - `is:open`
- Execute:
  - `FetchAndImportMRs(...)` directly
  - `handleFetchMRs(...)` (manager path) with mock Slack API
- Assert:
  - imports new PRs
  - repeat fetch deduplicates
  - Slack feedback is posted (start + summary)

## Optional Live Smoke Suite (manual gate)

Run only when all env vars are provided:
- `SLACK_BOT_TOKEN`
- `SLACK_APP_TOKEN`
- `TEST_SLACK_CHANNEL_ID`
- `GITHUB_TOKEN`
- `TEST_GITHUB_ORG` or `TEST_GITHUB_REPOS`

Scenarios:
1. Post a report via command handler and verify Slack response + DB insert.
2. Fetch real PRs from test repo/org and verify import counts and dedup on second run.

## Non-Goals
- No real Socket Mode websocket integration in CI.
- No destructive calls to production Slack channels or org-wide GitHub search in automated runs.
