# Handoff — #55: one work, two sentences, one vote

2026-10-25. `internal/explain`, `internal/store/explainduels.go`, `internal/corpus/readers.go`,
`internal/web/algo_explain.go`, `/algo?mode=explain`.

## What the row asked and what shipped

The row said: `/algo` compares two tunes' **result lists** and records a preference; two
**explanations** are never shown side by side. It now has a third mode that shows one work with
two explanations and records which one the reader finds convincing.

The two kinds are the only two this deployment can back with a **number**:

    tags     shares 8 tags with "The Guild of Gamers: The Supervillain":
             underage sex, rape/non-con, major character death and 5 more
    readers  2 readers bookmarked both this and "The Guild of Gamers: The Supervillain"

There is deliberately no third kind. A runtime model would write a nicer sentence and nobody
could check it; the duel would then measure prose style rather than evidence.

## Three refusals, each with a test that dies when the rule is removed

1. **An item with only one sentence is not offered.** A sentence alone in a column is not a
   comparison — the defect /algo already refuses for two identical tunes.
2. **A single shared reader is not a pattern** (`explain.MinSharedReaders = 2`). "1 reader
   bookmarked both" as a competing explanation is a sentence with no evidence in it.
3. **A nameless vote is refused by the store** (`ErrNoIdentifier`). It could not be
   de-duplicated, so one reader reloading could move the standings. The POST mints a session
   (`d.arenaSession`) before recording, so the refusal is a guard rather than the normal path.

One vote per reader per work, by unique index + upsert: changing your mind replaces your row.
The standings reuse `stats.CompareVotes`, so **no leader is named under 10 decisive duels** and
the record is printed either way. `neither` is a real answer and counts as a tie, which the sign
test already excludes from decisiveness.

## The vacuous test, and how it was caught

`TestAnItemWithOneSharedReaderIsNotADuel` first asserted that the page never contains
"1 reader bookmarked both this". It passed **with the floor deleted**, because the fixture never
produced such a candidate: the one-reader cohort ranked nowhere near the top 20.

It now measures its own premise: it calls the engine, finds a ranked work that shares a tag with
the seed and has *exactly* one shared reader, fails with "premise: no ranked work shares exactly
ONE reader" if the fixture stops producing one, and then asserts that work's id is absent from
the rendered duels. With the floor removed it now fails by name: `work 25 has ONE shared reader
and was offered as a duel`.

The fixture needed a third cohort for this: five readers on works 1-20, one reader on the seed
plus works 21-120. Three attempts were needed — the first two ranked none of the one-reader
works, and the second also forgot to include the seed in the single reader's bookmarks, so the
count was 0 rather than 1.

## What the live page showed, and the sentence's real limit

Seed `30788489` (the mirror's most-bookmarked work, 69 readers): **4 of 10 ranked works are
duelable**. The other six share a tag but have fewer than 2 shared readers — the interaction
graph is 180,677 rows, and one neighbour of the usual gate seed shares only 2 readers.

The four duels all print the same three tags first, and that is **correct**: measured on the
mirror, the shared tags between two works of one fandom are archive-wide warnings —
"underage sex" 9,507 works, "rape/non-con" 9,835, "major character death" 11,205 of 112,937.
Rarity ordering (the only lever the tag sentence has) puts them first *because* they are the
rarest tags the pair shares. So the tag sentence on this corpus often reads as a content-warning
list, and that is the honest input to the duel rather than a bug: whether a reader finds
"shares these 8 tags" or "2 readers took both" more convincing is exactly the unmeasured
question.

## Verified

- 33 packages green, `gofmt` clean.
- Mutants killed: the readers floor (via the page test above), the rarity order reversed
  (`TestSharedTagsAreOrderedRarestFirst`), the standings floor and the one-vote upsert (store
  tests).
- **151 deploy checks** (was 145). The gate uses seed `30788489` rather than the shared gate
  seed, because a thin seed honestly produces no duel and the block would assert nothing — that
  is why the first deploy reported 3 failures against `17249318`.
- `check_absent` does not exist in this script; the absence helper is `check_not`.

## Files

    internal/explain/explain.go              the two sentences, and when a duel is refused
    internal/store/explainduels.go           explanation_duels, standings, power floor
    internal/corpus/readers.go               SharedReaders, SharedReadersMany, TagSupport
    internal/web/algo_explain.go             the mode, the vote, the sample line
    internal/web/assets/algo.html            the duel block and its standings
