# ReportBot: Closed-Loop Agentic Classification

## The Problem

ReportBot classifies 50-100+ work items per week into report sections using an LLM. The original system was **open-loop** — manager corrections to the generated report were lost, and the LLM repeated the same mistakes every week.

```mermaid
flowchart LR
    Items["Work Items"] --> LLM["LLM\nClassifier"]
    LLM --> Report["Report"]
    Report --> Manager["Manager\nReview"]
    Manager -..->|"corrections LOST"| Void["void"]

    style Void fill:#ccc,stroke:#999,color:#666
    style Manager fill:#f96,stroke:#333,color:#000
```

## The Solution: Closed-Loop Feedback

We added a **self-improving feedback loop** where every manager correction trains the next generation run. The system gets smarter each week — without any model fine-tuning.

```mermaid
flowchart TB
    subgraph Input["Data Sources"]
        S1["/report\n(Slack)"]
        S2["/fetch\n(GitLab)"]
    end

    subgraph DB["Persistent Storage"]
        WI[(work_items)]
        CH[(classification\nhistory)]
        CR[(corrections)]
    end

    subgraph Pipeline["Classification Pipeline"]
        direction LR
        TFIDF["TF-IDF\nExample Selector"]
        BATCH["Parallel\nBatch Splitter"]
        GEN["LLM Classifier\n(cached prompts)"]
        MERGE["Result\nMerger"]
        CRITIC["Critic Pass\n(optional)"]
        TFIDF --> BATCH --> GEN --> MERGE --> CRITIC
    end

    subgraph Memory["Memory Layer"]
        GL["Auto-Growing\nGlossary"]
        CG["Classification\nGuide"]
        CORR["Past Corrections\n(negative examples)"]
    end

    subgraph Output["Output & Feedback"]
        RPT["Draft Report\n(.md + .eml)"]
        UNC["Uncertainty\nSampling"]
        RETRO["/retrospect\nAnalysis"]
    end

    subgraph Feedback["Manager Feedback"]
        MGR["Manager Review"]
        EDIT["Edit Modal\n(category dropdown)"]
        BTN["Uncertainty\nButtons"]
    end

    S1 --> WI
    S2 --> WI
    WI --> TFIDF
    CH -->|"12 weeks\nhistory"| TFIDF

    Memory -->|"inject into\nprompt"| GEN
    CRITIC --> RPT
    CRITIC -->|"persist decisions"| CH
    CRITIC -->|"low confidence"| UNC

    RPT --> MGR
    MGR --> EDIT
    UNC --> BTN

    EDIT -->|"record"| CR
    BTN -->|"record"| CR

    CR -->|"next run"| CORR
    CR -->|"2+ same correction"| GL
    CR --> RETRO
    RETRO -->|"suggest rules"| GL
    RETRO -->|"suggest updates"| CG

    style Pipeline fill:#1a1a2e,stroke:#69f,color:#fff
    style Memory fill:#0f3460,stroke:#69f,color:#fff
    style Feedback fill:#533a1e,stroke:#f96,color:#fff
    style MGR fill:#f96,stroke:#333,color:#000
```

---

## Key Features

### 1. Parallel Batch Classification

LLM calls are the bottleneck. We split items into batches and classify them **concurrently** via goroutines with `sync.WaitGroup`.

- Each goroutine writes to its own index in a pre-allocated results slice (no mutex)
- All inputs (glossary, options, corrections) are read-only — safe for concurrent access
- ~3x speedup for teams with 50+ items per week

```
Before:  [Batch 1: 800ms] [Batch 2: 750ms] [Batch 3: 820ms] = 2,370ms
After:   [Batch 1: 800ms]                                     =   820ms
         [Batch 2: 750ms]
         [Batch 3: 820ms]
```

### 1a. Prompt Caching

Anthropic system prompts are marked with `CacheControl: ephemeral`. Since all parallel batches share the same system prompt (sections, rules, glossary, corrections), only the first batch pays full input token cost. Subsequent batches hit the cache.

- Tracked via `CacheCreationInputTokens` and `CacheReadInputTokens` in `LLMUsage`
- ~40% reduction in input token costs for multi-batch runs
- OpenAI path is unchanged (no cache control API)

### 1b. TF-IDF Example Selection

Instead of using the first N items from the previous report as few-shot examples, we select the most relevant historical items via TF-IDF cosine similarity.

- `internal/integrations/llm/llm_examples.go` implements a pure Go TF-IDF index (no external deps)
- Up to 500 classified items from the last 12 weeks (confidence >= 0.70) are loaded from `classification_history`
- For each batch, `topKForBatch` finds the union of per-query top-K results, deduplicated
- Falls back to existing-item examples when no history is available
- Pure in-memory computation — zero additional LLM calls

### 1c. Generator-Critic Loop

An optional second LLM pass reviews all classification assignments after batches are merged:

- Enabled via `llm_critic_enabled: true` in config
- The critic sees the full assignment list (all items + their assigned sections) in a single call
- Returns only items it believes are misclassified, with a suggested alternative section
- Valid suggestions are applied; invalid section IDs are ignored
- Non-fatal: if the critic call fails, original assignments are preserved
- Token usage is tracked and logged alongside the main classification

### 2. Classification History

Every LLM decision is persisted to a `classification_history` table with full context:

| Field | Purpose |
|-------|---------|
| `section_id` / `section_label` | What the LLM chose |
| `confidence` | How sure it was (0-1) |
| `normalized_status` | done / in progress / in testing |
| `duplicate_of` | Cross-item dedup reference |
| `llm_provider` / `llm_model` | Reproducibility |

This enables accuracy tracking over time and powers the correction system.

### 3. Correction Capture

Three correction sources feed into a single `classification_corrections` table:

| Source | Trigger | UX |
|--------|---------|-----|
| **Edit Modal** | Manager changes category in `/list` | Dropdown with all report sections |
| **Uncertainty Buttons** | Manager clicks correct section | Inline Slack buttons |
| **Retrospective** | LLM suggests pattern fixes | Apply/Dismiss buttons |

Each correction records: original section, corrected section, item description, who corrected, when.

### 4. Corrections in LLM Prompts

Past corrections (last 4 weeks, up to 20) are injected into the **user prompt** as negative examples:

```
Past corrections (learn from these — avoid repeating these mistakes):
- "Fix TimescaleDB lag" was classified as S1_2 (Support), corrected to S1_3 (Query Service)
- "Tenant pending check" was classified as S0_0 (Infrastructure), corrected to S1_0 (Query Service)
```

The system prompt also gets a one-liner: *"A 'Past corrections' section shows previous misclassifications. Avoid repeating them."*

Result: the LLM avoids repeating known mistakes without any fine-tuning.

### 5. Auto-Growing Glossary

When the **same correction appears 2+ times**, the system automatically appends a term to the glossary YAML:

```
Correction: "tenant pending" → Query Service  (1st time: recorded)
Correction: "tenant pending" → Query Service  (2nd time: auto-glossary triggered)

# glossary.yaml gets:
terms:
  - phrase: "tenant pending"
    section: "Query Service"
```

Glossary terms override LLM decisions with 0.99 confidence — deterministic, zero-latency.

### 6. Uncertainty Sampling

After report generation, items with confidence between 0 and the threshold (default 0.70) get sent to the manager as ephemeral Slack messages with interactive buttons:

```
┌─────────────────────────────────────────────────┐
│ Uncertain classification (45% confidence)       │
│ Item ID: 142                                    │
│ Best guess: Query Service                       │
│                                                 │
│ [Infrastructure] [Query Service] [Support] [Other...] │
└─────────────────────────────────────────────────┘
```

One tap records a correction, updates the item, and feeds the feedback loop. Capped at 10 items to avoid notification fatigue.

### 7. `/retrospect` Command

Manager-only command that loads all corrections from the last 4 weeks and sends them to the LLM for pattern analysis:

```
/retrospect
→ "Analyzing 23 corrections from the last 4 weeks..."
→ Suggestion 1: "TimescaleDB items always go to Query Service"
   Action: Add glossary term "timescaledb" → S1_0
   [Apply] [Dismiss]
→ Suggestion 2: "Support tickets with [ticket] prefix..."
   Action: Add guide rule
   [Apply] [Dismiss]
```

- Max 5 suggestions, only patterns appearing 2+ times
- "Apply" writes directly to glossary YAML or classification guide
- "Dismiss" is a no-op (the correction data is still retained)

---

## The Feedback Cycle

```mermaid
sequenceDiagram
    participant M as Manager
    participant Bot as ReportBot
    participant TFIDF as TF-IDF Index
    participant LLM as LLM
    participant DB as Database
    participant Mem as Memory

    Note over Bot,LLM: Week N
    Bot->>DB: Load items + 12 weeks history
    Bot->>TFIDF: Build index from history
    Bot->>Mem: Load corrections + glossary
    Bot->>TFIDF: Select examples per batch
    TFIDF-->>Bot: Relevant few-shot examples
    Bot->>LLM: Classify (parallel batches, cached prompt)
    LLM-->>Bot: Decisions + confidence
    opt Critic enabled
        Bot->>LLM: Critic reviews all assignments
        LLM-->>Bot: Flagged misclassifications
    end
    Bot->>DB: Persist to classification_history

    alt Low confidence items
        Bot->>M: Uncertainty buttons
        M-->>Bot: Clicks correct section
        Bot->>DB: Record correction
    end

    Bot->>M: Upload report
    M->>Bot: Edit item category in /list
    Bot->>DB: Record correction

    alt 2+ same correction
        Bot->>Mem: Auto-append glossary term
    end

    Note over Bot,LLM: End of Week
    M->>Bot: /retrospect
    Bot->>LLM: Analyze correction patterns
    LLM-->>Bot: Suggestions
    Bot->>M: Apply/Dismiss buttons
    M-->>Bot: Apply
    Bot->>Mem: Update glossary + guide

    Note over Bot,LLM: Week N+1 — fewer mistakes
```

---

## Database Schema

```mermaid
erDiagram
    work_items {
        int id PK
        text description
        text author
        text source
        text category
        text status
        datetime reported_at
    }

    classification_history {
        int id PK
        int work_item_id FK
        text section_id
        text section_label
        real confidence
        text llm_provider
        text llm_model
        datetime classified_at
    }

    classification_corrections {
        int id PK
        int work_item_id FK
        text original_section_id
        text corrected_section_id
        text description
        text corrected_by
        datetime corrected_at
    }

    work_items ||--o{ classification_history : "has"
    work_items ||--o{ classification_corrections : "has"
```

---

## Memory Tiers

```mermaid
flowchart TB
    subgraph T1["Tier 1: Glossary (Deterministic)"]
        direction LR
        G1["Phrase → Section mapping"]
        G2["Confidence override: 0.99"]
        G3["Auto-populated from\nrepeated corrections"]
    end

    subgraph T2["Tier 2: Corrections (Statistical)"]
        direction LR
        H1["Last 4 weeks of\nmanager corrections"]
        H2["Injected as negative\nexamples in prompt"]
        H3["'Don't classify X\nas Y again'"]
    end

    subgraph T3["Tier 3: Guide (Semantic)"]
        direction LR
        R1["Free-text rules in\nclassification guide"]
        R2["Updated via\n/retrospect"]
        R3["Domain-specific\nhints for the LLM"]
    end

    subgraph T4["Tier 4: History (Similarity)"]
        direction LR
        S1["12 weeks of classified\nitems from DB"]
        S2["TF-IDF cosine\nsimilarity search"]
        S3["Top-K relevant\nexamples per batch"]
    end

    subgraph Prompt["Assembled LLM Prompt"]
        P["System: sections + guide + glossary rules (cached)\nUser: TF-IDF examples + corrections + items"]
    end

    T1 -->|"post-LLM override"| Prompt
    T2 -->|"negative examples"| Prompt
    T3 -->|"semantic hints"| Prompt
    T4 -->|"few-shot examples"| Prompt

    style T1 fill:#2d6a4f,stroke:#333,color:#fff
    style T2 fill:#1a5276,stroke:#333,color:#fff
    style T3 fill:#6c3483,stroke:#333,color:#fff
    style T4 fill:#1a5276,stroke:#333,color:#fff
    style Prompt fill:#1a1a2e,stroke:#69f,color:#fff
```

---

## What's Next (Deferred)

| Feature | Description | Impact |
|---------|-------------|--------|
| **Structured Output** | Use `anthropic.Tool` for critic response schema | Eliminates JSON parse failures |
| **Structured Logging** | Replace `log.Printf` with `slog` + duration tracking | Observability for LLM call latency |
| **Semantic Embeddings (RAG)** | Replace TF-IDF with vector embeddings for higher-quality example selection | Better cold-start for new item types |
| **ReAct Agent** | Turn classifier into an agent that queries GitLab, past reports, team context before classifying | Context-aware classification |
