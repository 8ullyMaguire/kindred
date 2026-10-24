# Handoff 2026-10-24k — #35: authors by readers, and the empty list nobody had noticed

## What shipped

`GET /api/v1/authors/similar?name=<author>` now ranks by **co-readership** when the mirror
has an interaction table, with the tag profile behind it:

- `?by=readers` (default) — merge of both arms, ordered by the share of the seed author's
  readers who read the candidate; a candidate only the tag arm can see ranks after every
  candidate a reader placed.
- `?by=tags` — the pre-#35 ordering, untouched, and the only arm on a mirror with no
  interaction table.
- `?by=<anything else>` — 400 naming the two arms.
- `ranked_by` in the response says which arm actually answered. On a mirror with no index
  the default reports `tag_profile` rather than claiming an ordering nothing computed.
- Every row carries `readers`, `read_share`, and one evidence line per arm that placed it:
  `"33 of "Masterweaver"'s 61 readers also read this author (54%)"`.

## The measurement, and its one bias

`kindred author-eval` (new): reader holdout on author pairs. For a seed author, split its
readers 80/20 (seeded by the author key, so runs repeat), take candidates from the 80% only,
and score both arms against the candidates the **held-out 20%** also read.

Live mirror, 40 seed authors, 5,686 held-out truth pairs, k=10:

| arm | recall@10 | hits |
|---|---|---|
| tag profile (as shipped) | **0.0146** | 83 |
| co-readership | **0.0346** | 197 |

Co-readership was better on 34 seeds, worse on **0**, tied on 6 (sign test z = 5.83). It
placed 216 of 8,000 candidates against a tag arm that can place at most 50 per seed.

**The bias, stated rather than adjusted away:** the candidate set is defined BY co-readership
(what the evidence readers read), which advantages that arm — it can only rank authors its own
evidence named. The tag arm competes within the same set, so it is not disadvantaged *inside*
the comparison, but an author the tag profile would have found and no reader has read is not
in the experiment at all. The honest summary is: **on the question "which author will these
readers pick up next", co-readership is 2.4x better; the tag arm answers a different question
("who writes alike"), which the endpoint was never asked.**

## The bug the evaluation exposed before it could report anything

The first `author-eval` run scored **0 for both arms** — the proxy returned nothing for every
one of the 40 seeds. Not the evaluator: the endpoint itself.

`Masterweaver` has 26 works, 196 distinct tags, and **37 of those tags are on a single work**.
The profile keeps the 24 *rarest* tags, so all 24 were singletons; the candidate query
returned one author — the seed itself — and `/authors/similar` answered
`{"items":[],"returned":0}` for an author with 61 readers. Sampled 25 authors at random from
the interaction table: **2 of 25** had no neighbours at all, and they were the multi-work
authors a reader is most likely to click.

The fix is a document-frequency floor (`df >= 3`): a tag on one or two works cannot relate two
authors, because it can connect at most one other work. Rarest-first is right; it has to be
rarest-first *among tags that are shared*. After the floor: **25 of 25** sampled authors have
neighbours, and every one of the 40 eval seeds produced a ranking.

Three things the floor had to get right, each now a test:

- **No table, no floor.** Without document frequencies every tag reads df=0, and a floor on
  that number deletes every profile: a mirror that has never been ingested would answer "no
  author shares these tags" for every author. The floor is a statement about a table.
- **A fixture is not an archive.** `AuthorMinDF` is an `Engine` field (0 = default 3) for the
  same reason `MinCorpusWorks` is an option in corpus-query: twelve works must be able to test
  the ranking without every tag failing a floor written for 113k.
- **An author who exists is not a 404.** When every tag is below the floor the engine returns
  `ErrAuthorProfileEmpty` and the handler answers **200** with a `note`; a 404 would tell a
  reader the person is not in the mirror, which is false.

## `orphan_account` is not an author

1,889 works read by 1,585 people, plus `Anonymous` with 1,650. They satisfy every data rule
and would rank near the top of almost any list ordered by readers. Excluded as **candidates**,
allowed as **seeds** (asking who reads the orphan account is a fair question; answering "the
orphan account" is not). The measurement excludes them too — a list the endpoint would never
serve must not be the list being scored.

## The mutation round: 6 mutants, 6 killed, 2 of them initially survived

Both survivors were **weak tests, not weak code**, and both are the same shape as earlier
sessions' findings:

1. **The vote-once fixture had no reader spanning both seeds.** `CoReadersFrom` counts each
   reader once however many seeds they read; the fixture's only multi-seed reader (9) read A
   and C but not B, so the dedupe was never exercised and deleting it changed nothing. Fixed by
   giving reader 1 a bookmark on all three.
2. **The merge fixture put its only real contender outside the sort.** Tag-only rows are
   appended *after* `sort.SliceStable`, by design, so they never compete — and the two
   remaining candidates happened to order identically under both comparators. Fixed by giving
   a both-arms candidate the stronger tag score and the weaker reader count, so only a
   read-share comparator can put them in the measured order.

Killed in the end: orphan-as-candidate, vote-once, blind-proxy credit, df floor, no-table
floor, merge order.

## Deployment

`CoReaders` is built at boot from the same interaction table the collab index uses: 63,245
authors, ~1 s. Memory: 180 MB RSS immediately after a restart, **573 MB once the index, the
graph, the collab index and the doc-frequency table are all resident and serving** — the
ceiling is 768 MB `MemoryHigh` / 1 GB `MemoryMax`, so the headroom is real but this index is
the largest single addition to it. A mirror with no interaction table builds nothing. The gate is **130 checks**, three of them new:

- the live default ranks by readers (`ranked_by: co_readers`, a `readers` count, and the
  evidence sentence),
- `?by=tags` still answers,
- an unknown arm is refused by name.

`check-deploy.sh` ends in a summary and an `exit`; the first version of these three checks was
appended **after** that exit and silently never ran (the gate still said 125). A block added to
the end of a script with a verdict at the end is dead code — read where the file stops printing
before adding to it.

## Known limits

- Two people sharing a byline collide: `arena.NormalizeAuthorName` is the rule `/author`,
  `SimilarAuthors` and this index all use, and the collision is documented there.
- Co-readership cannot relate two authors nobody has read together — a new author with one
  reader has no neighbours, and the tag arm ranks after *any* co-read candidate rather than
  competing with it. That ordering is a deliberate choice from the measurement, not an
  oversight.
- The endpoint still caps its tag arm at 50 rows internally; the merge asks for that cap and
  interleaves up to `limit`.

## Next

OPEN rows with data present: **#52** TF-IDF summary similarity (also unblocks #47), **#44/#80**
seasonal context (with `dayExpr` normalising the mirror's two date formats), **#48** reader
velocity from `user_work_interactions.timestamp`, **#36/#37** are the remaining corpus-query
surfaces. **#35's own follow-up**, if anyone wants it: the co-readership index rebuilt on a
timer rather than at boot, since it is the only thing in the service whose staleness is
measured in bookmarks rather than in corpus rows.
