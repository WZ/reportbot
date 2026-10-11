---
status: accepted
---

# File Work Items on arrival: the decision model picks the Section

Work Items are filed when they arrive (`/report`, fetch), not when the Weekly Report is generated, so the team sees a live report and the reporter can fix a doubtful Placement while the context is fresh. One decision-model request (System One protocol, internal gateway) per Work Item picks the Section — a Choice over the active Sections, given the most similar past Work Items and where they were filed — and in the same request answers the narrow questions code cannot decide: the Status, which ticket ID is meant, and whether the report is an Update to an existing Work Item. The Choice's calibrated confidence decides whether to file without asking. An LLM is used only to draft Section descriptions for `/retrospect`.

## Considered Options

Measured on the same 592 past Work Items (answer key: the published reports):

| Option | Section accuracy | Auto-filed at ≥ 90% accuracy | Calls per Work Item |
|---|---|---|---|
| **A1: decision model picks, with 30 similar items** (chosen) | 67.1% | 31% | 1 |
| B: LLM picks, decision model checks the pick | 66.9% | 10% | 2 |

- **B, an LLM picks and the decision model checks it** — built first and rejected: Section accuracy was the same, but the check's probability separated right from wrong picks poorly (about 81% correct even at p ≥ 0.95), so far fewer Work Items could be filed without asking, at twice the calls.
- **The LLM does everything, as in V1** — rejected: its self-reported confidence cannot gate anything, so every doubtful Placement would either be filed silently or asked about blindly.

The answer key is V1's output as edited by people, which favours V1's own labels; no option reached V1's 73% on it.

## Consequences

- Every arrival costs one decision-model request, off the Slack ack path; the reporter sees "Saved" first and the Placement seconds later.
- The decision model being unreachable degrades the Work Item to Undetermined with a retry sweep; it is never lost.
- The decision model must be given the most similar past Work Items and where they were filed; measured accuracy depends on that context far more than on wording.
- The gateway rejects structured `instructions`; supporting data goes in `state` and questions reference it by path.
- The LLM is optional; without it only `/retrospect` is unavailable.
