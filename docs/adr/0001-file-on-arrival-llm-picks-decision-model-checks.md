---
status: accepted
---

# File Work Items on arrival: an LLM picks the Section, a decision model checks it

Work Items are filed when they arrive (`/report`, fetch), not when the Weekly Report is generated, so the team sees a live report and the reporter can fix a doubtful Placement while the context is fresh. A generative LLM picks the Section, because it was the most accurate option measured. A decision model (System One protocol, internal gateway) then answers narrow typed questions whose probabilities drive behaviour: whether the LLM's Section is right enough to file without asking, the Status when code cannot map it, which ticket ID is meant, and whether the report is an Update to an existing Work Item.

## Considered Options

- **Decision model does everything** — rejected: on 592 production items its best Section accuracy was 69.6% against the LLM's 74.5%, and only about a third of items reached 90% accuracy, so most reports would need a manual pick.
- **LLM does everything, as before** — rejected: its self-reported confidence cannot gate anything, so every doubtful Placement would either be filed silently or asked about blindly.

## Consequences

- Every arrival costs one LLM call and then one decision-model request, in that order, off the Slack ack path; the reporter sees "Saved" first and the Placement 20–60 s later.
- Either model being unreachable degrades the Work Item to Undetermined with a retry sweep; it is never lost.
- The decision model must be given the most similar past Work Items and where they were filed; measured accuracy depends on that context far more than on wording.
- The gateway rejects structured `instructions`; supporting data goes in `state` and questions reference it by path.
