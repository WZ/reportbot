---
status: rejected
---

# Auto-applied LLM learned notes for the decision model, considered and rejected

People correct the decision model mostly between neighbouring Sections. We considered letting an LLM draft **learned notes** for the most confused Sections every night: a short contrastive text per Section, kept apart from the manager-written description and shown to the decision model in the Section's Choice criteria. A replay of recent person decisions would gate each draft: apply only if accuracy rose by at least 2 points with no Section of 5 or more items losing. We also considered having the LLM give a **second opinion** on unsure Work Items nobody placed. Both were built and measured, then removed.

## Why rejected

- **The measured gain was noise.** A historical simulation over the last 8 published weeks used real Work Items, the production prompt, the production replay gate and the decision model on the internal gateway. It improved Section accuracy by **+0.9 points on 116 Work Items over 7 weeks** (58.6% → 59.5%). The auto-filed share was unchanged, and accuracy among auto-filed Work Items fell from 91.1% to 88.9%. The gate discarded 12 of 15 drafted notes, and the 3 it applied changed one Work Item's outcome.
- **Generated Section text did not help the decision model before either.** Earlier evaluations of LLM-drafted Section descriptions showed no reliable gain; ADR 0001 found that accuracy depends on the similar past Work Items far more than on wording.
- **The LLM is no better than the decision model on unsure Work Items.** On Work Items the decision model was unsure about, the LLM chose the right Section 52% of the time, against 53% for the decision model's own first choice. A second opinion adds a model call and a dependency without adding accuracy.

The cost was real: LLM-written text in the decision model's criteria, a prompt fed by reporter-written text (needing injection defences and output validation), a nightly replay of a few hundred requests, and more for managers to review and undo.

## What we do instead

- An unsure Work Item that nobody placed within 24 hours, or that is still Undetermined at publish, is filed under the decision model's most likely Section from its stored answer (**Fallback Placement**). No model is called, and the reporter can change it.
- The LLM is used only for **Structure Proposals** (new, split or retired Sections), which a manager approves.
- Every Work Item edit, delete and Update is recorded, and a Friday **Draft Snapshot** is kept beside the Published Report. Together they show what people change by hand: the signal a future LLM editor would be trained and evaluated on.

## Consequences

- Publishing calls no model.
- The confidence threshold stays a fixed configuration value.
- Migration 7 created a learned-notes column and tables (`learned_notes`, `learned_notes_history`, `learning_runs`, `replay_answers`). Migrations are additive, so they stay, unused.
