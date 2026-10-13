---
status: accepted
---

# author_ids accepts code-hosting username keys

Fetched Code Changes get an owner only through `author_ids`. Besides numeric account keys (`gitlab:24`, `github:5151`), it also accepts username keys (`gitlab:yfma`, `github:octo-cat`). Fetch looks up the numeric-ID key first, then the username key. Username matching is exact and case-insensitive; display names never bind. If both keys match one Code Change but map to different Slack user IDs, the ID key wins and a warning names both keys.

## Considered Options

- **Numeric IDs only** — rejected: IDs are hard to find and review, so `author_ids` was hard to keep up to date.
- **Display names or fuzzy matching** — rejected: display names are free text, not unique, and anyone can change their own, so a match could grant someone else's edit rights.

## Consequences

- `author_ids` is easier to write and review, and `--suggest-author-ids` prints username keys with the numeric ID in a comment.
- **A renamed or reused username must be updated in `author_ids`.** Until then, the renamed account's new Code Changes stay unowned (unless a numeric key also maps it), and a reused username binds its new holder. Numeric IDs never change. The owner accepted this because usernames are centrally managed; use a numeric key for any account where that does not hold.
- Existing Work Items keep their `author_id`; a key change affects only Code Changes fetched afterwards.
