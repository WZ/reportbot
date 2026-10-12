# ReportBot: Agentic AI Features Overview

## What Is ReportBot?

ReportBot is a Slack bot that collects Work Items from developers (`/report`) and from code hosting (GitLab MRs, GitHub PRs) and files each one into the team's Weekly Report **the moment it arrives**. The team watches a live report in the web UI; a manager publishes it at the end of the week. Terms used here (Work Item, Section, Placement, Taxonomy, Undetermined) are defined in [`CONTEXT.md`](../CONTEXT.md).

## The Problem We Solved

The first version classified everything in one batch at report time. Confidence was invented by code, corrections arrived too late to matter that week, and the Section list was re-parsed from last week's markdown, so renaming a heading broke history.

| Before | Now |
|--------|-----|
| Items classified in a batch when the report is generated | Each Work Item is filed when it arrives |
| Confidence assigned by code (0.90 by default) | Confidence is a calibrated probability from a decision model |
| Low-confidence items reviewed by the manager at report time | The reporter is asked "Where does this go?" right away; unanswered after 24 h, the model's first choice is used |
| Sections re-parsed from the last report, positional IDs | Taxonomy in the database with stable IDs, edited in the web UI |
| Open items carried over as copied text | Open Work Items stay on every report until they are done |
| Glossary and guide files maintained beside the code | Manager-written Section descriptions; structure changes proposed by the LLM, approved by a manager |
| Report visible only after generation | Live report in the web UI for the whole team |

---

## Agentic AI Features

### What Makes It "Agentic"?

1. **Memory** — every Placement, by the models or by a person, is kept and reused as context
2. **Self-evaluation** — every Section pick comes with a calibrated confidence that decides whether it is filed
3. **Knowing what it doesn't know** — below a confidence level it asks a person instead of guessing
4. **Self-improvement** — person decisions and corrections feed the next filing, the LLM proposes structure changes, and every hand edit is recorded for future learning

---

### Feature 1: Filing on Arrival

**What it does:** `/report` saves the Work Item, replies "Saved — filing it…" within Slack's 3-second limit, and queues a filing job. A background worker files it and updates the reply with the Section, the formatted report line, Status and tickets.

**Why it matters:** The reporter sees where the item landed while the context is fresh, and the team sees the report grow during the week.

**How it works:**
- The job is stored in the database together with the Work Item, so a crash or restart loses nothing
- Workers retry failures with backoff (5 attempts); after that the item is Undetermined and a 15-minute sweep re-queues it
- Fetched MRs/PRs go through the same queue; their questions go to the author by DM

---

### Feature 2: One Calibrated Decision per Work Item

**What it does:** A decision model (System One protocol) picks the Section as a Choice over the active Sections, and in the same request answers the narrow questions code cannot decide. The Choice's confidence is the Placement's confidence.

**Why it matters:** The decision model returns calibrated probabilities, so its confidence can gate what is filed without a person. On 592 past Work Items this filed about three times as many items at ≥ 90% accuracy as an LLM pick checked by the same model, with the same overall accuracy and half the calls.

**How it works:**
- One request per Work Item: the `section` Choice (criteria = each Section's path plus any manager-written description), with the 30 most similar past Work Items and where they were filed in `state`
- In the same request, only when code cannot decide: the Status, the ticket ID among several candidate numbers, and whether the report updates an Open Work Item
- Raw answers and probabilities are stored (`model_judgments`), so the threshold can be re-tuned without calling the model again

---

### Feature 3: Ask Instead of Guess

**What it does:** When the Section confidence is below `decision_model_confidence` (default 0.85), the Work Item waits in Undetermined and the reporter gets "Where does this go?" buttons for the decision model's three most likely Sections, plus Other….

**Why it matters:** The system acts alone only where it is reliable and hands the rest to the person who knows the answer. One click from the reporter is a person-decided Placement.

**How it works:**
- At 0.85, about 43% of items filed themselves at about 84% accuracy in the evaluation (0.91 reached 90% with 27% filed); for 88% of items the right Section is among the three buttons
- Likely Updates are always confirmed: "Looks like an update to …" [Update it] [It's new]
- A likely link between an MR and an Open Work Item is confirmed by the MR author by DM; only an exact ticket match links automatically

---

### Feature 4: Memory That Comes From People

**What it does:** Every Placement change is recorded in `placement_history` with who decided it — a model, the reporter, or a manager. Person decisions become the similar-item examples for future filing. Every hand edit of a Work Item's text, Status or tickets, every delete and every Update is also recorded with before, after and who, and a Friday Draft Snapshot of the report is kept beside the Published Report: together they show what people fix by hand.

**Why it matters:** The system learns the team's filing conventions from ordinary use — no retraining, no separate labelling step.

---

### Feature 5: Taxonomy as Living Data

**What it does:** Areas, Categories and Sections live in the database with stable IDs. Managers add, rename, reorder, split, merge and retire them in the web UI.

**Why it matters:** Renaming "7.4.5 release" to "7.4.6 release" no longer breaks history, and new Sections take effect immediately.

**How it works:**
- Every Taxonomy edit queues one re-filing of Open Work Items: the decision model picks again for each, and confident changes move
- If a person's choice is overridden, that person gets a DM with an Undo button

---

### Feature 6: Fallback When Nobody Answers

**What it does:** An unsure Work Item that nobody placed within 24 hours, or that is still Undetermined when a manager confirms publishing, is filed under the decision model's most likely Section, read from its stored answer. The reporter gets "Filed under *X* automatically (no answer within a day)" with a Change button.

**Why it matters:** Reports don't fill up with Undetermined items when people are busy, and no extra model call is made. An LLM second opinion was measured and chose no better than the decision model's own first choice (52% vs 53% on unsure items), so it was dropped (ADR 0003).

---

### Feature 7: Structure Proposals (optional LLM)

**What it does:** After each publish, and on `/retrospect`, the LLM proposes 0–3 Taxonomy changes: a new Section for a recurring theme among Undetermined and "Other…" Work Items, a split of a large Section with separate themes, or retiring a Section unused for 8 weeks. Managers get [Approve] [Reject] by DM. Approve applies the change once through the Taxonomy edit paths, which re-file Open Work Items. A rejected change is not proposed again for 8 weeks, even reworded; an undecided one expires after 2 weeks, and a retirement is checked again when approved.

**Why it matters:** The report's structure drifts as work changes. The LLM spots the drift, and a manager decides. LLM-written notes fed to the decision model were tried, measured at +0.9 points over 7 weeks (noise), and rejected (ADR 0003).

---

### Feature 8: Accuracy Dashboard (`/stats`)

**What it does:** Shows, for the week: items reported or fetched, how many were filed automatically, how many were asked, how many were filed automatically because the reporter didn't answer, filing failures, items still Undetermined, and corrections by Section.

**Why it matters:** The confidence threshold was set from historical data; `/stats` is how the team checks it against real corrections and tunes it.

---

## Agentic Design Considerations

### Human-in-the-Loop by Default

- Nothing is filed silently unless the decision model is confident
- Updates and MR links are confirmed by a person; only exact ticket matches link automatically
- `done` is always a person's decision — a merged MR moves a Work Item to `in testing`, not `done`
- Structure Proposals change the Taxonomy only when a manager approves them
- A fallback Placement uses only the decision model's own first choice, and the reporter can change it
- A person whose choice is overridden after a Taxonomy edit can undo it

### Graceful Degradation

- The Slack reply never waits for a model
- If the decision model is unreachable, the Work Item is Undetermined with a reason and retried; it is never lost
- Publishing calls no model; it warns about Undetermined items, and only after the manager confirms files unanswered unsure ones by fallback

### Trust and Identity

- Only a Work Item's author (by Slack user ID) or a manager can change it; display names are never trusted
- Fetched and imported Work Items get an owner only through the explicit `author_ids` map; code-hosting accounts are matched by numeric account ID
- Unmapped Work Items are manager-only

### Privacy and Cost

- The models are reached through configurable endpoints, so they can stay behind an internal gateway
- Similar-item retrieval is in-memory TF-IDF — no extra model calls
- Each Work Item costs one decision-model request; publishing costs none

---

## High-Level Architecture

```mermaid
flowchart TB
    subgraph Input["Input"]
        direction LR
        DEV["/report<br>(Slack)"]
        CC["GitLab MRs /<br>GitHub PRs"]
    end

    Q[("Filing queue<br>(saved first)")]

    subgraph File["File on arrival"]
        direction LR
        MEM["Memory<br>───<br>Taxonomy + descriptions<br>Similar past items"]
        DM["Decision model picks the Section<br>(+ Status, ticket, Update)"]
        MEM -.->|context| DM
    end

    subgraph Place["Placement"]
        direction LR
        AUTO["Filed<br>(confidence ≥ 0.85)"]
        ASK["Where does this go?<br>(reporter decides)"]
    end

    subgraph Deliver["Deliver"]
        direction LR
        LIVE["Live Weekly Report<br>(web UI)"]
        PUB["Published Report<br>(.md / .eml)"]
        STATS["/stats"]
    end

    MGR["Manager"]
    FB["Fallback after 24 h<br>(model's #1 Section)"]
    LLM["LLM (optional):<br>structure proposals"]

    DEV --> Q
    CC --> Q
    Q --> DM
    DM -->|"confident"| AUTO
    DM -->|"unsure"| ASK
    AUTO --> LIVE
    ASK -->|"person Placement"| LIVE
    ASK -->|"no answer in 24 h"| FB
    FB --> LIVE
    LIVE --> PUB
    LIVE --> STATS
    Deliver --> MGR
    ASK -.->|"person decisions"| MEM
    MGR -.->|"moves, Taxonomy edits"| MEM
    PUB -.->|"after publishing"| LLM
    LLM -->|"Approve / Reject"| MGR
    MGR -.->|"approved changes"| MEM

    style Input fill:#1a5276,stroke:#333,color:#fff
    style File fill:#1a1a2e,stroke:#69f,color:#fff
    style Place fill:#14532d,stroke:#4ade80,color:#fff
    style Deliver fill:#0f3460,stroke:#69f,color:#fff
    style MGR fill:#f96,stroke:#333,color:#000
    style FB fill:#14532d,stroke:#4ade80,color:#fff
    style LLM fill:#3b2f5c,stroke:#a78bfa,color:#fff,stroke-dasharray: 4 3
```

Two loops close the system. The **reporter loop**: whenever the decision model is unsure, the reporter decides, and that decision becomes memory for the next item. If nobody answers within 24 hours, the decision model's first choice is used and the reporter can still change it. The **manager loop**: moves, Taxonomy edits and approved Structure Proposals sharpen the context the decision model sees. Only confident answers, or unanswered ones, are filed without a person.

---

## How It Improves Over Time

```
Week 1:  Many items are asked ("Where does this go?") — reporters click once each
Week 2:  Those clicks are now similar-item examples → more confident picks → fewer questions
Week 3:  after publishing, Structure Proposals (e.g. a new Section for a recurring theme) → manager approves
Week 4:  /stats shows real auto-filed accuracy → threshold re-tuned from production data
```

---

## What's Next

| Item | Value | Effort |
|------|-------|--------|
| **Re-tune the threshold** from 2–3 weeks of `/stats` corrections | More items filed without a question | Low |
| **Manager-written descriptions** for the most confused Sections | Fewer neighbouring-Section mistakes | Low |
| **Merge suggestions:** nightly, the LLM suggests merging related Work Item lines; the manager accepts or rejects them in the web editor. A pilot caught all of the manager's real merges. | Fewer duplicate lines on the report | Medium |
| **Open-for-N-weeks badge:** computed, no LLM. | Long-running work stands out | Low |
| **Faster decision-model serving** (batching on the gateway) | Faster bulk re-filing | Medium |
| **Semantic embeddings** instead of TF-IDF for similar items | Better context for the decision model | Medium |
