# Handoff 2026-10-24j — #34 tag gap: the writer-facing mode, and three popularity traps

## What shipped

`GET /api/v1/corpus-query/tag-gap?tag=hurt%2Fcomfort&limit=25` — the only mode in this
project that answers an AUTHOR's question: what do the works around this tag carry that its
own works rarely do?

Shape (two hops, and the second one is the query):

```
seed T → its neighbourhood N: the 30 tags with the strongest PAIRWISE ASSOCIATION to T
       → the tags C carried by N's works that do NOT carry T
       → rank by (enrichment − 1) × (1 − lift)
```

- `lift` = (own / works(T)) ÷ (works(C) / corpusWorks) — how often T's works carry C
  against the archive's rate. `< 1` means T lacks it.
- `enrichment` = (support / neighbourhoodWorks) ÷ (works(C) / corpusWorks) — how much more
  often the neighbourhood carries C than the archive does, shrunk toward 1 by
  `support/(support+100)`.
- `support` is the **gate** (≥ the mode's co-work floor), not a factor.
- Every row prints `own`, `corpus works`, `lift` and both rates, so a reader can check any
  row by hand.

`Options.MinCorpusWorks` is new (0 = default 50, negative = disabled) so a small fixture
can test the logic rather than the floor.

## The three popularity traps, in the order they were found

1. **support as the score.** The first ranking was `support × (1 − lift)`: on the live
   mirror hurt/comfort's gaps were `underage sex, vaginal sex, oral sex` — and every seed's
   list opened the same way. Support is dominated by how MANY works carry a tag.
2. **the neighbourhood as "the archive's front page".** Taking the seed's 30 heaviest
   co-tags put `m/m`, `f/m`, `Explicit`, `No Archive Warnings Apply` at the centre — tags on
   a third of the archive. Every candidate then had enrichment ≈ 1.1x ("the whole archive is
   like this") and the score fell back to support. Ranked by association the same seed's
   neighbourhood is `whump, sickfic, chronic pain, seizures, "tim drake needs a hug"`.
3. **candidates chosen by support.** The top 150 candidates were the 150 largest, so a tag
   carried by 4,000 neighbourhood works at the archive's own rate beat one carried by 60 at
   six times the rate, and the specific ones never reached the enrichment test. Candidates
   are now selected by `support / works(C)` — P(tag | neighbourhood) — and their own sizes
   come back in the same row.

## The bug the live output caught, and the lesson

The first "fixed" version returned, for hurt/comfort:

```
implied/referenced child abuse   own 937/2990   2.94x the corpus rate
```

A 2.94x rate is the tag's works carrying it MORE than the archive — the opposite of a gap.
`gap = support × (enrichShrunk−1) × (1−lift)` is a product of two signed factors, and
**two negatives multiply to a positive**. One `gap <= 0` check cannot police both.

I had also looked at the previous run and pronounced it good because the names sounded like
gaps ("panic attacks, suicidal thoughts, trauma") — while the numbers printed beside each row
said its own works carried them 2-4x the archive rate. **Read the numbers a row prints
before believing the row.**

Guards are now explicit and separate (`lift >= 1` out, `enrichShrunk <= 1` out), and the
earlier claim in the file that the enrichment guard was dead code was true only while the
lift factor was positive.

## Verification

- `internal/corpusquery/taggap_test.go`, 6 tests on hand-countable fixtures: the top gap,
  the lift exclusion, the neighbourhood-not-over-carrying exclusion, both-factors-negative,
  a seed too small to describe, an unknown tag.
- **Five mutants, four of which changed the FIXTURE rather than the code** — that is the
  real content of the unit:
  - deleting the `NOT EXISTS` survived two fixtures (20 works fell under the corpus floor;
    30-inside/40-outside was killed by the lift clamp instead). The dedicated small fixture
    now has `own 12 of 40` against a corpus rate of 0.49 so only the NOT EXISTS explains the
    absence. Kills with `support 12`.
  - the both-negatives case needed 60 neighbourhood works, not 40: at 40 the tag never
    cleared the 50-support candidate gate and the test passed without touching the guards.
    Kills with `score 3.13, lift 1.19`.
  - removing `gap <= 0` survived because `Limit 20` sorted the bad row off the end.
  - a 30-tag fixture puts every candidate inside the seed's top-30 neighbourhood, where the
    candidate query excludes it by name — both fixtures now carry 30 FILLER tags.
- **One real bug found by printing the SQL arguments**: they were built "hood, seed, min,
  limit, hood" while the placeholders want "hood, seed, hood, min, limit". The minimum landed
  where the exclusion list starts; the candidate query returned nothing, which read as "no
  gaps found".
- **Performance, measured**: 34.5 s → 14 s → and the phases named via `KINDRED_TAG_GAP_TRACE=1`.
  The `NOT EXISTS` beside the join ran per JOINED row (21M lookups); as a materialised CTE it
  is per work (600k). 600 → 150 candidates cut the count queries from 6.5 s to 1.6 s. Modernc
  is ~2.6x the sqlite3 CLI on the same query, which is the remaining gap.
- Gate: **125 checks**, tag-gap fetched ONCE via the new `checkall` (five fetches of a 14 s
  query made the last checks time out).

## Known limitation, stated rather than hidden

The top rows on this archive are rating/meta attributes (`dead dove: do not eat`,
`no category`, `general audiences`) rather than the genre tags a writer would hope for. The
reason is structural: the neighbourhood is a UNION of 30 tags' works, so it mixes genre tags
with archive-wide ones, and a candidate's neighbourhood rate stays close to its archive rate
when the neighbourhood is half the archive. A ceiling on corpus share was written and
**removed again** — it is an unmeasured heuristic, and the measurement that would fix this
is a fandom-anchored neighbourhood (compare the seed's works with the works of its closest
FANDOMS, not with a 30-tag union). Until then the rows are checkable, which was the bar.

## Next

The remaining OPEN rows with data already present: #35 (author-author co-readership: the same
interaction table, a different question — `SimilarAuthors` is tag-profile overlap today),
#52 (TF-IDF summary similarity, which also unblocks #47), #44/#80 (seasonal context, now that
`dayExpr` normalises the mirror's two date formats), #48 (reader velocity from
`user_work_interactions.timestamp`).
