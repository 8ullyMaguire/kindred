# kindred §11 — the `/block` page

Status: **implemented 2026-09-30.** Written after the code, not before, which is
the one process failure worth recording: the feature was built on 2026-09-29
without ever appearing in SPEC.md or PLAN.md, so the only description of intent
was the test file's doc comment. This document is that description, recovered
and made durable.

## 11.1 The problem

The arena learns a negative weight per tag from the reader's comparisons: when
they pick work A over work B, every tag they differ on moves. That machinery
is blind to a specific and common case.

**A tag shared by every work the reader rejected teaches nothing.**
`LearnTags` only updates tags that *differ* between the two works shown, so a
tag carried by the whole rejected set never appears in a comparison at all. A
reader with strong, consistent evidence against a tag — a whole fandom, a
trope they always skip — accumulates no weight for it, no matter how long they
play. So a page built only from the weight table is empty for precisely the
reader with the most evidence.

## 11.2 Two sources, and why both are needed

`renderBlock` assembles candidates from two independent sources, because each
catches what the other structurally cannot.

**Source 1 — learned-negative weights.** `TagWeights(ctx, ownerKey, 40)`, tag
weight below zero. Gated on `tw.N >= 2`: a weight from a single comparison is a
coincidence, and offering it as "you probably do not want this" invites a block
the reader does not want. A block is a *statement*, and statements get
believed, so this page must not manufacture one. Sorted by weight ascending
(strongest dislike first), ties broken by evidence descending.

**Source 2 — tags common to works passed over.** `passedOverTags`, the tags
shared across works this reader skipped. Invisible to the weight table by
construction; that is the whole point. It contributes only when it can produce
at least `minEvidence` co-occurrences, for the same reason.

Both sources are deduplicated into one `seen` set, and a tag already in
`Blocked` is never offered as a candidate — there is no point recommending the
unblock of something just blocked.

## 11.3 The block itself is a statement, not an inference

A block lives in `arena_user_blocked_tags`, **not** as a very negative weight.
The two are separate so that:

- **unblocking restores what the arena actually inferred.** Removing a block
  does not pretend the reader ever liked the tag, and it does not silently
  re-teach a weight that outlived the block.
- **a weight batch can never overwrite a block.** Blocks are rules; weights are
  inferences. Collapsing them would let a re-run of `learn` silently unblock
  something the reader decided on.

`BlockedTags` orders by `at DESC, tag_id ASC` so the most recent decision is
first and ordering is total (two blocks written in the same second by
`datetime('now')` still have a defined order, which matters for the test that
counts controls).

## 11.4 Idempotent, explicitly not a toggle

`postBlock` dispatches on an explicit `action` field: `block` or `unblock`.
**A toggle was rejected deliberately** — a toggle makes the outcome depend on
state the request does not carry, so a double-submit, a retried POST, or a
browser's back-then-forward flips a block back off. A block the reader cannot
reliably keep is worse than no block at all.

Both operations are idempotent at the store level:

- `BlockTag` — `ON CONFLICT(owner_key, tag_id) DO UPDATE SET at = excluded.at`.
  Re-blocking refreshes the timestamp. A double-click is harmless rather than a
  500.
- `UnblockTag` — `DELETE ... WHERE owner_key = ? AND tag_id = ?`. Unblocking a
  tag that was never blocked is a no-op, not a 400 the reader sees for clicking
  the wrong button.

Both handlers redirect `303` to `/block` after success.

## 11.5 Every block must be visible and reversible

**This is the load-bearing constraint of the feature, and it is what the tests
were failing on.** A block that does not appear on the page with a working
unblock control is a decision made *for* the reader, with no way to reverse it.
The template therefore renders `page.Blocked` first, above both candidate
sources, with the tag name, a link to `/tag/:id`, and an explicit `unblock`
form.

The prose in the template says why unblocking does not restore a positive
opinion: *"a block is a rule, and removing it does not pretend you ever liked
the tag."*

## 11.6 The empty state is an explanation, not a blank page

When there are no candidates, the page states *why* — a reader who has just
compared two works and seen no suggestions otherwise cannot tell "nothing to
block yet" from "this page is broken". When the reader has no evidence at all,
that is the honest answer and it says so.

## 11.7 The bug that cost the most time

The feature shipped with two tests failing, and neither the handler, the store,
nor the template was at fault. All three were correct. The fixture was not.

`newTestServer` returns `httptest.NewServer(...)`, and **`ts.Client()` on a
plain `httptest.Server` has `Jar == nil`.** The `kindred_arena` cookie set by
the first response was therefore never sent back, so every request minted a
fresh session, `OwnerKey` returned a different HMAC each time, and any test
that wrote with one request and read with another was looking at two different
owners.

The symptom was a *silent wrong answer* in the worst direction: the write
provably landed — a row in the table, correct `owner_key` — while the page
listed nothing. That reads as a store bug and is not one. It is the fixture,
and production is correct, because a real browser sends the cookie.

The codebase already knew: `arena_resume_test.go` has a `jarClient` helper
whose doc comment says *"returns a client that persists cookies, so repeated
requests share one arena session the way a browser's do."* The `/block` tests
simply never used it. The fix is `jarClient` at four call sites, not a change
to any production code.

**`TestBlockDoesNotOpenOtherPagePOSTs` still uses `ts.Client()` on purpose.**
It asserts that other pages answer 405 to a POST, and carries no session state
across requests, so the jar is irrelevant there. Changing it would be
cargo-culting the fix onto a test that does not have the bug.

**Recorded because the wrong lesson is available and attractive:** this looks
like "the session handling is broken, add a fallback" and it is not. Adding a
fallback that makes the write and the read agree on an identity would make the
test pass while leaving the actual invariant untested — that a block is
recorded for, and reversible by, *the same reader*.

## 11.8 What is deliberately not here

- **No global/instance blocks.** Blocking is per reader, keyed on the arena
  session. There is no moderation role in kindred.
- **No block reasons.** The page states where a candidate came from
  (`"you have chosen against N works carrying it"`); a block stores no note.
  The reader's reason is their own and the data model does not presume to
  record it.
- **No bulk block.** One click per tag, by design: §11.2's whole argument is
  that blocks are statements and must not be made carelessly.
- **Not in the API surface as a typed route.** The page is server-rendered
  HTML; `/block` is not part of the JSON API and does not appear in the OpenAPI
  document.
