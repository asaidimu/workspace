# Store: owner-scope the by-ID Gets

Status: `[ ]` planned. Created 2026-10-29.

## Why this exists

Store specification section 10, rule 10 says no repository method returns or
modifies another owner's data (system scope excepted). The current
implementations break that rule for by-ID reads:

- `store/memory/repos.go:250` (`conversationRepo.Get`) looks the row up by ID
  with no owner check, and the turn/content/blob repos do the same.
- `store/anansistore/conversations.go` deliberately mirrors that quirk; see the
  comment at `store/anansistore/conversations.go:14`.

This was discovered while wiring `blobResolver` (see
`done/blocks-hybrid.md`). I scoped the memory `Get` to the repository's owner
and then reverted it, because the two backends must stay byte-for-byte
behavioural twins and anansistore cannot express the check the same way
without a filter on `_id_`. As a stopgap, `blobresolver.go` re-checks
`row.Owner == owner` after the read, so blob resolution is safe; every other
by-ID caller is not.

## What to do

- **Context:** owner-scoped repositories are constructed with `scope.Owner`
  (see `store/memory/repos.go` `ready`/`scope` usage). The check belongs in the
  repository, not in each caller.
- **Details:** in every by-ID `Get`, return `store.ErrNotFound` (not a
  permission error) when the row's owner differs from the repository scope and
  the scope is not system. System scope keeps reading across owners.
- **Both backends:** anansistore must do the same, either by adding
  `eq("owner", scope.Owner)` to the by-ID query or by reading and then
  comparing. Keep the memory/anansi parity comment in sync.
- **Tests:** add cases to `store/storetest` that create an owner A row, read it
  with an owner B repository, and expect `ErrNotFound`; run them against both
  memory and anansistore. Cover conversations, turns, content and blobs.
- **Then:** remove the now-redundant owner check in `blobresolver.go` (or keep
  it as defence in depth and say so in a comment).

## Files

- `store/memory/repos.go`
- `store/anansistore/conversations.go` and the other anansi repos
- `store/storetest/` (contract cases)
- `blobresolver.go`
