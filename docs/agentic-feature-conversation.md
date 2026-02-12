# Agentic AI Features — Design Conversation

**Date**: 2026-02-11
**Branch**: `fix/hide-empty-undetermined-category`
**Participants**: Wilson Li, Claude Opus 4.6

---

## Context

Wilson asked: how to enrich the agentic AI features in ReportBot? Specifically:
- **Reflection**: when generating a new report, learn from manual edits made to previous reports (moved items, added categories/sub-categories)
- **Memory**: persistent learning across weeks so the system improves over time

---

## Codebase Analysis

### What Already Exists (Proto-Agentic)

| Pattern | What exists | Where |
|---------|------------|-------|
| Few-shot learning | Last report's items fed as examples to LLM | `llm.go:150-168` |
| Template continuity | Last week's report structure carries forward | `report_builder.go:98-124` |
| Confidence gating | Items below `LLMConfidence` (0.70) go to "Undetermined" | `report_builder.go` merge logic |
| Knowledge base | Glossary overrides LLM decisions for known phrases | `glossary.go` + `llm_glossary.yaml` |
| Duplicate detection | LLM can flag items as duplicates of existing ones | `llm.go:183` |

### Critical Gap

**No feedback loop.** The system is open-loop: LLM classifies → human corrects → corrections are lost forever. The system makes the same mistakes every week.

### Key Files

- **`llm.go`** — Core classification: prompt building, LLM calls (Anthropic SDK / OpenAI HTTP), response parsing, glossary overrides
- **`report_builder.go`** — Pipeline: load last report as template → classify new items → merge → render markdown
- **`slack.go`** — Command handlers (`/report`, `/fetch-mrs`, `/generate-report`, `/list-items`), edit/delete modals
- **`db.go`** — SQLite schema (`work_items` table), CRUD operations
- **`glossary.go`** — Manual phrase→section YAML mappings, applied post-classification
- **`config.go`** — Config struct with LLM settings: provider, model, batch size, confidence threshold, example count

### Current LLM Prompt Structure

**System prompt**: Lists valid section IDs (e.g., `S0_2: Product Alpha > Observability`), instructions for classification, status normalization, ticket extraction, duplicate detection, confidence scoring.

**User prompt**: Examples from last report (`EX|S0_2|Fix API metrics`), existing items for dedup (`K1|S0_2|in progress|Add heartbeat`), new items to classify (`ID:142 - Fix log forwarding`).

**Response format**: JSON array `[{"id": 142, "section_id": "S0_2", "normalized_status": "in progress", "ticket_ids": "1234", "duplicate_of": "", "confidence": 0.91}]`

---

## Proposed Agentic Design Patterns

### 1. Reflection (Learning from Corrections)

**The feedback loop:**

```
Week N:   LLM classifies "ClickHouse upgrade" → Support (wrong)
          Manager moves it → Query Service (correct)
          System records: {item, old_section, new_section, corrector}

Week N+1: LLM sees in prompt:
          "CORRECTION: 'ClickHouse upgrade' was misclassified as Support,
           correct section is Query Service"
          → LLM gets it right this time
```

**Three implementation pieces:**

1. **Correction capture** — New DB table `classification_corrections` + hook into edit modal (`slack.go:695`) to track "before" state
2. **Correction injection** — New prompt section in `llm.go:buildSectionPrompts()` with recent corrections as negative examples
3. **Report diff detection** — When manager edits the generated `.md` and re-uploads, diff original vs edited to auto-extract corrections

### 2. Memory (Persistent Cross-Session Learning)

**Tier 1 — Auto-growing glossary** (low effort, high impact)
- When the same correction happens 2+ times, auto-add to `llm_glossary.yaml`
- New function: `glossary.go:AppendGlossaryTerm()`
- The glossary already overrides LLM with confidence 0.99 — just automate population

**Tier 2 — Classification history** (medium effort)
- New table persisting every LLM decision: `{item_id, section_id, confidence, model, timestamp}`
- Currently decisions are in-memory only and discarded after rendering
- Enables: "what did the LLM decide last time for similar items?"

**Tier 3 — Semantic example selection / RAG** (high effort, biggest quality jump)
- Today: blindly picks last 20 items as examples
- Better: embed all historical items, find top-K most similar past items with their correct sections as few-shot examples
- RAG applied to classification

### 3. Self-Evaluation (Meta-Cognition)

**Uncertainty sampling** — For items below confidence threshold, instead of silently dumping to "Undetermined", ask the manager via Slack interactive buttons:

```
Uncertain: "[12345] Fix API server metrics collection"
LLM suggests: Observability (62% confidence)
[Observability] [Infrastructure] [Query Service] [Other...]
```

Button press = free correction record.

**Weekly retrospective** — New `/retrospective` command:

```
3 items were misclassified as Support → Query Service
Pattern: items mentioning "ClickHouse" belong in Query Service
Suggestion: Add glossary rule "clickhouse" → Query Service
[Apply] [Dismiss]
```

The LLM reasons about its own mistakes (reflection at the meta level).

### 4. Planning (Generator-Critic Loop)

Currently report generation is single-pass. More agentic approach:

1. **Draft** — LLM classifies all items (current behavior)
2. **Critique** — Second LLM call reviews the draft: "Are any items in wrong sections? Duplicates? Items in Undetermined that clearly belong elsewhere?"
3. **Revise** — Apply critique, re-classify flagged items
4. **Present** — Show manager draft with annotations

Classic generator-critic loop. Cost: 2-3x LLM calls. Benefit: catches mistakes before human review.

### 5. Tool Use (Agent with Actions)

Turn the classifier into a ReAct agent that can:
- Search GitLab for ticket context before classifying (MR labels, linked issues)
- Query past reports to find how similar items were categorized historically
- Look up team member roles to infer category from author

---

## New Database Tables

```sql
-- Track every LLM classification decision
CREATE TABLE classification_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    work_item_id INTEGER NOT NULL,
    section_id TEXT,
    confidence REAL,
    llm_provider TEXT,
    llm_model TEXT,
    classified_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY(work_item_id) REFERENCES work_items(id)
);

-- Track human corrections to LLM decisions
CREATE TABLE classification_corrections (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    work_item_id INTEGER NOT NULL,
    original_section_id TEXT,
    corrected_section_id TEXT,
    original_status TEXT,
    corrected_status TEXT,
    corrected_by TEXT,
    corrected_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY(work_item_id) REFERENCES work_items(id)
);

-- Auto-learned patterns from repeated corrections
CREATE TABLE learned_patterns (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    description_pattern TEXT,
    section_label TEXT,
    confidence_boost REAL,
    occurrence_count INTEGER DEFAULT 1,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    last_updated DATETIME DEFAULT CURRENT_TIMESTAMP
);
```

---

## Implementation Phases

| Phase | Pattern | What was built | Files changed | Status |
|-------|---------|---------------|---------------|--------|
| **1** | Reflection | Correction capture table + track category changes in edit modal | `db.go`, `slack.go` | Done |
| **2** | Reflection | Inject corrections into LLM prompt | `llm.go` | Done |
| **3** | Memory | Auto-grow glossary from repeated corrections | `glossary.go` | Done |
| **4** | Self-Eval | Uncertainty sampling via Slack buttons | `slack.go`, `report_builder.go` | Done |
| **5** | Memory | Classification history persistence | `db.go`, `report_builder.go` | Done |
| **6** | Self-Eval | `/retrospective` command with meta-analysis | `llm.go`, `slack.go` | Done |
| **7** | Planning | Generator-critic loop for draft review | `llm.go`, `config.go` | Done |
| **8** | Memory | TF-IDF example selection from classification history | new `llm_examples.go` | Done |
| **9** | Infra | Prompt caching for Anthropic system prompts | `llm.go` | Done |
| **10** | Observability | `/report-stats` accuracy dashboard | `db.go`, `slack.go` | Done |

### File Changes Summary

**Modified existing:**
- `db.go` — New tables, migration, CRUD for corrections/history/patterns, stats queries
- `llm.go` — Correction injection, prompt caching, TF-IDF integration, critic pass, confidence tracking
- `report_builder.go` — Classification pipeline, history persistence, historical items threading
- `slack.go` — Uncertainty buttons, `/retrospective`, `/report-stats`, `/check` nudge UI, correction capture in edit modal
- `glossary.go` — Auto-append terms, frequency analysis
- `config.go` — New config fields for agentic features (`llm_critic_enabled`)

**Created new:**
- `llm_examples.go` — TF-IDF index for relevance-based few-shot example selection

---

## Architecture Diagrams

Full Mermaid diagrams saved separately in [`docs/agentic-architecture.md`](./agentic-architecture.md):

1. **Open-Loop Problem** — Original flow where corrections were lost
2. **Closed-Loop Solution** — Full pipeline with TF-IDF, parallel classification, critic, and feedback
3. **Feedback Cycle** — Sequence diagram of week-to-week learning with TF-IDF and critic
4. **Database Schema** — work_items, classification_history, classification_corrections
5. **Memory Tiers** — Glossary (rules) → Corrections (statistical) → Guide (semantic) → History (TF-IDF similarity)

---

## Status

All 10 phases are complete. The system now has a full closed-loop classification pipeline with:
- Self-improving corrections and auto-growing glossary (Phases 1-3)
- Uncertainty sampling and retrospective analysis (Phases 4-6)
- Generator-critic loop and TF-IDF example selection (Phases 7-8)
- Prompt caching and accuracy dashboard (Phases 9-10)

See [`docs/agentic-features-overview.md`](./agentic-features-overview.md) for a detailed overview of all features.
