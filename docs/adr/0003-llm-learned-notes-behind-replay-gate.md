---
status: accepted
---

# LLM-written context is applied automatically only behind a replay gate

People keep correcting the decision model between neighbouring Sections, and a better Section description is the cheapest fix (ADR 0001: accuracy depends on context far more than on wording). An LLM now drafts **Learned Notes**: a short note per Section, separate from the manager-written description, saying what belongs there and what goes elsewhere. Every night, for up to three Sections with new corrections or mis-ranked "Where does this go?" picks from at least two people, the LLM drafts notes. The decision model then re-runs its Section Choice on the last 8 weeks of person-decided Work Items (at least 60), once with the current and once with the candidate notes, using the request builder live filing uses. The notes are applied only if overall accuracy rises by at least 2 points and no Section with 5 or more replay items loses accuracy. Managers get a digest with the full note text and Undo.

## Considered Options

- **Replay gate on person decisions, automatic apply** (chosen). The decision is measured on the same kind of request production sends, against what people actually chose. A bad note costs one replay, not a week of misfiling.
- **Manager approval of each note**: rejected. Managers already ignored suggested descriptions (`/retrospect` until now), and a manager cannot tell by reading whether a note helps the decision model. The gate measures it.
- **Silent apply**: rejected. LLM-written text in the decision model's criteria would change filing for everyone with no evidence that it helps and no way to see why.

## Consequences

- The LLM is now part of the improvement loop. Without it, the loops are off and filing works as before.
- LLM output reaches the decision model's criteria, and reporter-written text feeds the prompt. So Work Item text goes to the LLM only as delimited JSON data, and a draft is rejected before the replay unless it is plain text of at most 300 characters. It must not address the model, must name only existing Sections, and must name no author. Notes only ever sit in their own Section's criteria value, never in the question's instructions.
- Replays cost decision-model requests. Identical requests are answered from a replay cache, and similar-item ties keep a stable order so the "current notes" replay is mostly cached.
- The same LLM also gives a **Second Opinion**: it files Work Items that nobody placed within 24 hours, choosing only among the decision model's top 3. It also makes weekly **Structure Proposals**, which a manager approves. Publishing may now call the LLM: `/generate-report` and web Generate first give a Second Opinion on still-Undetermined items, for at most about 60 seconds. This replaces ADR 0001's "publishing calls no model".
- The confidence threshold stays a fixed configuration value; nothing tunes it automatically.
