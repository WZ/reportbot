---
status: accepted
---

# Keep the Taxonomy in the database with stable Section IDs

The Taxonomy (Area → Category → Section) lives in database rows with stable IDs, edited by managers in the web UI, instead of being re-parsed from the previous Weekly Report's markdown. Positional IDs derived from the markdown shifted whenever a heading was added or reordered, which silently broke classification history, corrections, and any per-Section learning; real reports rename, split, and merge Sections most weeks.

## Considered Options

- **Keep parsing the last report, match Sections by label** — rejected: renames still sever history, and there is nowhere to store a Section description.
- **Versioned YAML file** — rejected: edits need a deploy, and managers already edit reports in the web UI.

## Consequences

- The markdown report is output only; editing a published `.md` by hand no longer changes the next week's structure.
- The Taxonomy is seeded once from the latest published report at cut-over.
- Taxonomy edits trigger re-classification of Open Work Items; confident ones move automatically, and a person whose choice was overridden is notified with Undo.
