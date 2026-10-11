# ReportBot

Collects a team's work items from Slack and code-hosting sources, files each one into the team's weekly report as it arrives, and publishes that report.

## Language

### Report structure

**Taxonomy**:
The ordered tree of Areas, Categories and Sections a team's report is organised by, owned by managers.
_Avoid_: template, report template

**Area**:
The broadest grouping of a report, e.g. a product line; contains Categories.
_Avoid_: marker, product section

**Category**:
A heading within an Area that groups related Sections, or is itself a Section when it has none.
_Avoid_: parent section, group

**Section**:
The deepest heading a Work Item can be filed under; its identity is stable across renames and reorders. Any owner or status text on it is just part of its name.
_Avoid_: subcategory, option, bucket, project

**Undetermined**:
The holding place for Work Items that could not be confidently filed into a Section, awaiting a human decision.
_Avoid_: UND, uncategorized

**Placement**:
The Section a Work Item is filed under, together with who decided it (the decision model, the assistant's Second Opinion, or a person) and how likely the decision model judged that Section to be right.
_Avoid_: classification, category (for the item's filing)

**Learned Notes**:
A short note on a Section, at most about two sentences, saying what belongs there and what goes to a neighbouring Section instead. It is drafted by the LLM from people's disagreements with the decision model and applied only when a replay of recent person decisions shows it helps. It is kept apart from the manager-written description; managers can edit, clear or undo it.
_Avoid_: glossary, hints, auto-description

**Structure Proposal**:
A change to the Taxonomy (a new Section, a split, or a retirement) that the LLM suggests from the week's Work Items and that a manager approves or rejects. A rejected change is not proposed again for 8 weeks.
_Avoid_: suggestion, auto-taxonomy

**Second Opinion**:
The assistant (the LLM) choosing one of the decision model's three most likely Sections for an unsure Work Item that nobody placed within 24 hours, or that is still Undetermined when the report is published. The reporter is told and can change it.
_Avoid_: fallback classification, LLM pick

### Work

**Work Item**:
One unit of a developer's work — reported in Slack or fetched from a merge/pull request — that appears on weekly reports.
_Avoid_: item, task, entry

**Open Work Item**:
A Work Item whose status is not done; it appears on every Weekly Report until it is done, then once more as done.
_Avoid_: carry-over item, existing item

**Code Change**:
A merge request or pull request fetched from code hosting; it either becomes its own Work Item or is linked to an existing one.
_Avoid_: MR, PR (when meaning the concept rather than the platform object)

**Update**:
A new report about an existing Work Item that changes its status or text instead of creating a new Work Item; always confirmed by a person.
_Avoid_: duplicate, merge

**Status**:
Where a Work Item is in its lifecycle: in progress, in testing, blocked, or done (terminal). The reporter's own wording may be kept alongside as a Status Note.
_Avoid_: state, stage

**Weekly Report**:
The rendering of all Work Items relevant to one Monday–Sunday week, organised by the Taxonomy.
_Avoid_: report draft, template

**Published Report**:
The immutable snapshot of a Weekly Report as it was sent; re-publishing the same week replaces it.
_Avoid_: generated report, final report
