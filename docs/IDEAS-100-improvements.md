# kindred: 100 ideas, triaged

A working backlog, not a wish list. Every row is priced against what this deployment
actually holds, using the four verdicts `docs/PLAN-recsurfaces-31-100.md` already uses:

| tag | meaning |
|---|---|
| **BUILD** | the data exists and the metric can move. Code, then a measurement. |
| **STREAM** | blocked by a per-item statistic with no judgements behind it: a bandit with no rewards, per-tag variance over ~150 comparisons. Needs reader traffic first. |
| **DATA** | blocked by missing data: no wrangling graph, no second instance, a snapshot where the row needs a time series, a table with zero rows. |
| **MODEL** | blocked by a model this deployment does not have: pure Go, no GPU, no runtime LLM. |

Effort: **S** ≈ an hour, **M** ≈ a day, **L** ≈ a week or an eval campaign.

Measured facts that must not be re-argued from memory live in the handoffs under
`docs/` and in the recommender skill. Rows below that depend on one of them say which.

---

## A. Ranking signals and weights (1–24)

1. **Coordinate-ascent the tune.** The eval is deterministic (split seeded by reader
   id), so 13 weights can be hill-climbed one at a time against `recall@10` with no
   sampler noise to fight. Start from the default, one pass, ±0.05 per weight, keep a
   weight only if the interval says BETTER. **BUILD, M.**
2. **Two-stage tune search.** Coarse grid on the four heaviest dimensions
   (`neighbourhood`, `tag_overlap`, `collab`, `session_taste` = 0.82 of 1.91), then
   coordinate ascent on the rest. Cheaper than a full search and honest about which
   weights actually matter. **BUILD, M.**
3. **Feed the bootstrap back into the loop.** `kindred evalcmp` exists now; make the
   ascent step refuse any move whose CI straddles zero. Without that, hill-climbing
   fits the cohort's noise over dozens of steps. **BUILD, S.**
4. **Cross-validate the tune.** Split the readers into two halves, tune on A, report
   on B. A tune that wins on its own cohort and loses on the other is fitted. **BUILD, S.**
5. **Cap IDF for singleton tags.** 491,414 of 634,188 freeform tags sit on one work;
   under `log(N/df)` they take the maximum weight in the corpus and can never relate
   two works. Floor the document frequency, as `/authors/similar` already had to
   (`AuthorMinDF`). **BUILD, S.**
6. **nPMI-pruned adjacency.** Today the CSR keeps each node's top-N edges by raw
   co-occurrence *count*, which re-imposes the frequency bias nPMI removes. Rebuild it
   from `cooccurrence_edges` (7,750,334 rows) ranked by nPMI instead. **BUILD, M.**
7. **Per-fandom tunes.** `eval_slices` already stores per-fandom counts. Score each
   fandom's slice, pick the tune per fandom, and only ship it where the slice has
   enough held-out works to say anything. **BUILD, L.**
8. **Reader-chosen weights.** `/algo` already compares tunes; add three sliders
   (familiar ↔ surprising, short ↔ long, safe ↔ risky) that map onto tune vectors and
   persist per owner key. **BUILD, M.**
9. **Length as a hard filter when the reader says so.** `length_fit` is a soft signal
   (0.713 with vs 0.575 without) but a reader who wants 100k+ words wants a filter, not
   a preference. **BUILD, S.**
10. **Per-reader MMR lambda.** 0.75 beats 1.0 on both axes; the open question is
    whether the reader who reads one fandom at a time wants a different point. Store a
    lambda per owner key and measure. **STREAM** (needs a second arm with enough
    readers to separate).
11. **Session decay.** `session_taste` holds a reader's arena weights; add a half-life
    so last week's mood does not outvote tonight's. **BUILD, S.**
12. **Consensus across seeds.** `seed_agreement` averages; add "at least two seeds
    must share this tag" as a variant and let the eval choose. **BUILD, S.**
13. **Series identity as a signal.** `works.series_id` exists and nothing ranks on it:
    readers of book 1 want book 2. **BUILD, M.**
14. **Completion status as a signal.** Abandoned WIPs are a known reader complaint;
    the chapter fields are in the mirror. **BUILD, S.**
15. **Language as a hard filter.** The mirror is overwhelmingly English but not
    entirely; a reader's language preference is not a taste vector. **BUILD, S.**
16. **Negative evidence.** A "not for me" tap on an item should subtract from its tag
    vector, not just hide the work. Distinct from the block table, which is a hard
    exclusion. **BUILD, M.**
17. **Tag-gap injection.** `corpusquery` computes tag gaps with the correct gates
    (support as gate, lift as score, `lift >= 1` and `enrich <= 1` separately). Let a
    small share of the list come from a gap so a reader sees something they would
    never search for. **BUILD, M.**
18. **Bandit over tunes with real feedback.** The interleaving infrastructure exists
    and answers "which list did the reader prefer" without propensities. A bandit needs
    rewards per arm; today the arena holds ~150 comparisons. **STREAM**, and the power
    floor in `internal/stats` will refuse before the bandit does.
19. **Calibrate scores per tune.** Rank order is what the reader sees, but
    `/api/v1/calibration` publishes bands; a tune change moves them and nothing
    recomputes. Recompute per stored run. **BUILD, S.**
20. **Quality normalised by age.** A 2015 work with 5,000 kudos and a 2025 work with
    500 are not comparable on raw kudos. Use kudos/age with a floor. **BUILD, S.**
21. **Recency anchored on the mirror.** A 90-day window held 2 works; anchored on the
    mirror's newest date and widened to 180 days it held 16,153. Any new windowed
    signal must do the same. **BUILD, S.**
22. **Embedding signals for works, not just tags.** Every one of the 634k vectors is
    `kind='tag'`; a work's vector is a derived mean. A per-work embedding would cost a
    build but would stop two fluffs from looking identical. **BUILD, L.**
23. **Contrastive tag vectors.** Mean-subtract then L2-normalise took tag-family
    coherence from 0.194 to 0.888; the same correction is not applied to the work-side
    mean. Try centring the work vector too, and measure. **BUILD, M.**
24. **A cheap nPMI arm for `seed_agreement`.** It shares `idfOf` with `idf_overlap`;
    if nPMI wins for one it should be tried for the other. One change per measurement,
    which is why it is a row and not part of the first experiment. **BUILD, S.**

## B. Candidate generation and pools (25–34)

25. **Pool recall on every run.** `--pool-depth` exists and is off by default. A work
    the pool never held cannot be ranked, so a recall number without its ceiling is
    half a number. Make the ceiling print by default. **BUILD, S.**
26. **Union the walk pool by default.** `walk` needed `internal/engine/walkpool.go`
    before it could change anything: a signal whose candidates come from one hop cannot
    surface two-hop finds. Same shape as `collabpool`. **BUILD, S.**
27. **Budget the pool by latency, not by count.** Through the API a 27 s query became
    an HTTP 500 at 20 s; caps are a latency knob whose cost is roughly linear. Choose
    the cap from the budget. **BUILD, M.**
28. **Cache the pool per seed-tag set.** Two readers with three overlapping tags share
    most of their candidates; the tag pool query is the expensive part. **BUILD, M.**
29. **Materialise `top works per tag`.** 634k tags × 24 works is a small table and
    turns the pool query into a lookup. **BUILD, M.**
30. **A seen-set Bloom filter per reader.** `SeenIDs` is a map built per request; for a
    reader with 2,000 bookmarks that is 2,000 entries before any work is scored.
    **BUILD, S.**
31. **Deduplicate near-identical works in the pool.** The shelf code drops ≥0.9
    Jaccard duplicates before filling a page; the ranking path does not. **BUILD, S.**
32. **Second-instance candidates.** "Readers of this instance also liked" needs a
    second deployment's interactions. **DATA** (no wrangling graph, no second instance).
33. **Cross-fandom bridging as a pool source.** `bridges.go` computes them and the
    ranking ignores them. **BUILD, M.**
34. **Cold-start pool from the seed's fandom, not its tags.** A seed with two obscure
    tags produces a pool of two works. **BUILD, S.**

## C. Evaluation and measurement (35–50)

35. **Paired dump by default.** `--dump-users` landed; make every recorded run write
    one, so no future comparison is untestable. **BUILD, S.**
36. **Slice-level intervals.** Per-fandom and per-tag slices are stored as counts;
    with a dump they can carry intervals too, which is what tells you a change helped
    *somewhere* rather than everywhere a little. **BUILD, M.**
37. **Power calculator.** "How many readers to detect +5% recall at 80% power" as a
    subcommand, so the next campaign is sized before it runs. **BUILD, S.**
38. **Temporal split as a second evaluator.** The current split holds out bookmarks at
    random, which lets "future" bookmarks into the profile (risk 3 in the nPMI plan).
    A split by date answers the product question instead. **BUILD, M.**
39. **Offline↔online correlation.** For every past tune change, plot the offline delta
    against the interleave delta. If the two disagree, the offline evaluator is
    measuring the wrong thing and the whole campaign is mispriced. **DATA** (needs a
    history of paired changes; the stored runs do not carry interleave outcomes).
40. **Golden-set fingerprint.** 20 fixed seeds, expected top-10 ids, committed. A
    refactor that changes ranking silently fails loudly. **BUILD, S.**
41. **Synthetic readers with known taste.** Build 100 readers whose taste is a
    generated tag vector, then check the ranker finds their works. A recall ceiling
    that real data cannot give. **BUILD, M.**
42. **Evaluate at k=5 and k=20 too.** `recall@10` over a 12,515-work universe is a
    measurement, not the product; the top three carry 25% of hits and the tail 7%.
    Different k answers different questions. **BUILD, S.**
43. **Store the weights, not the name.** `eval_runs.tune` names a tune that can be
    edited later; store the weight vector beside it. A rename or an overwrite currently
    makes an old run unattributable. **BUILD, S.**
44. **Refuse a run whose signal set changed.** `meta.degraded` is recorded; nothing
    compares it to the previous run before printing a delta. **BUILD, S.**
45. **Diversity and length intervals.** The dump carries recall and NDCG; overlap and
    length-fit are aggregated elsewhere. Same instrument, three more columns.
    **BUILD, S.**
46. **An eval page that reads.** `/eval` shows standings; add the per-arm table with
    intervals so a human can see "no verdict" without a terminal. **BUILD, M.**
47. **Ablation registry.** Every stored tune is an ablation; a page listing
    tune → run → metric would stop the next session re-running `no_walk`.
    **BUILD, S.**
48. **Regression gate in CI.** `go test ./...` is green; nothing runs an eval. A 40-
    reader night run on a schedule, compared with `evalcmp`, would catch a ranking
    regression before a deploy. **BUILD, M.**
49. **Explain the abstention.** When the engine returns nothing, it knows why (pool
    empty, all filtered, all skipped). Record the reason in the report. **BUILD, S.**
50. **Shadow evaluation of the deployed binary.** Run a second binary against the same
    seeds on the host and diff the ids, so a deploy that changes ranking is detected by
    behaviour rather than by provenance. **BUILD, M.**

## D. Corpus and data quality (51–62)

51. **Backfill `work_tags.tag_type`.** 3.89M rows say `'freeforms'` for essentially
    every pre-existing row; the listing pages carry the real category. The backfill
    query is already documented and resumes with no state file. **BUILD, L** (paced
    by AO3, ~60–105 s/page in a storm).
52. **Merge duplicate works.** The mirror was crawled twice over months; a title+author
    key would find re-ingests and stop them splitting a work's readers.
    **BUILD, M.**
53. **Refresh kudos/hits.** They are a snapshot from whenever the work was fetched;
    a "rising" signal built on stale counts measures the crawl. **BUILD, M.**
54. **Summaries for the 113k works that lack one.** `summary_style` skips without
    them, and the item evidence says so — which is honest, but it means one signal is
    dark for the whole corpus. **BUILD, L** (paced by AO3).
55. **Chapter and completion backfill.** Same shape as the summary gap. **BUILD, M.**
56. **Date normalisation at ingest.** 112,893 rows are `05 Sep 2007` and two are ISO;
    a lexical comparison silently counts every work written on the 30th or 31st.
    Normalise on write, not in every query. **BUILD, S.**
57. **Fandom identity by name join.** `tag_type` finds ~110 typed rows against 5,336
    fandoms a name join finds. Anything fandom-shaped must not wait on row 51.
    **BUILD, S.**
58. **Tag spelling cluster review.** `families.json` is thin: only three spellings of
    each universe. Report "too few to measure" rather than a purity over three tags.
    **BUILD, M.**
59. **Language distribution report.** Which languages the mirror holds, and how many
    works per language — before building any language filter (row 15).
    **BUILD, S.**
60. **A deduplicated tag vocabulary for display.** `tags.name` is `UNIQUE COLLATE
    NOCASE`, so case variants are already merged; punctuation variants are not.
    **BUILD, S.**
61. **Interaction provenance.** `user_work_interactions` has no timestamp: a bookmark
    is when the crawl saw it, not when the reader made it. A temporal split (row 38) is
    limited by this. **DATA** (the column does not exist and cannot be reconstructed).
62. **Reader history for cold-start.** The state database holds ~11 marked works from
    8 owners. Anything per-reader is a demo, not a measurement. **DATA.**

## E. Performance and operations (63–74)

63. **An in-memory tag index at boot.** `modernc.org/sqlite` (pure Go) runs the same
    query ~2.6x slower than the CLI; 3.9M work_tags rows fit in a few hundred MB and
    would make tag lookups free. **BUILD, L.**
64. **Cache the tag vectors for the pool.** `tagVectors` loads 81 MiB once; the
    per-work mean is recomputed per request. **BUILD, M.**
65. **A warm-up request after restart.** The unit needs 10–20 s to load its index and
    curls in that window return `000`, which reads like a crash. Serve a readiness
    endpoint that reports the load state. **BUILD, S.**
66. **Metrics endpoint.** Latency per signal, pool size, degraded list, cache hit rate
    — Prometheus text on `/metrics`. Right now the only instrument is a curl and a
    profile. **BUILD, M.**
67. **Slow-query log with a threshold.** `KINDRED_TAG_GAP_TRACE=1` exists for one
    surface; make it general and always-on above 1 s. **BUILD, S.**
68. **Freeze the mirror during an eval.** The corpus has a 600 MB WAL from crawling
    while reading. An eval against a moving mirror measures a different corpus per arm.
    **BUILD, S.**
69. **Snapshot the corpus per eval campaign.** Copy the DB (or `VACUUM INTO`) and run
    every arm against the snapshot, so a campaign is reproducible. **BUILD, S.**
70. **A deploy gate for the ranking.** `scripts/deploy.sh` checks behaviour and
    provenance; add two eval arms and require "no WORSE" from `evalcmp`. **BUILD, M.**
71. **Cap the WAL.** 600 MB of WAL beside a 1.9 GB corpus will eventually fill the
    disk; a checkpoint on a timer. **BUILD, S.**
72. **Move the state DB off root ownership.** Every CLI call needs `sudo` and explicit
    flags, and env vars are ignored — the most repeated friction in this repo.
    **BUILD, S.**
73. **A single `make eval` that does the right thing.** Arms, snapshot, dump, compare,
    report — the sequence exists in shell scripts that only the last session knew.
    **BUILD, S.**
74. **Profile the top-3 fandom queries** the way tag-gap was profiled; the `NOT EXISTS`
    beside a join was 21M lookups and 10 s → 2.6 s as a materialised CTE. Probably
    more of those. **BUILD, M.**

## F. Reader-facing product (75–92)

75. **"Why not this one."** The explain surface says why an item is there; the
    interesting question is why a work the reader expected is absent. Answer from the
    pool and the filters. **BUILD, M.**
76. **A shareable shelf link.** `kindred shelf` writes pages; a stable URL with a
    k-anonymity floor (row 93) makes it a product. **BUILD, M.**
77. **RSS per saved search.** Feeds exist per tag and per user; a saved query with a
    feed is the cheapest retention feature here. **BUILD, S.**
78. **"More like this" on every work page.** The engine already answers it; the page
    does not ask. **BUILD, S.**
79. **Author page: readers also read.** `internal/authorcf` computes co-readership and
    is served at `/authors/similar`; the work-level page does not surface it.
    **BUILD, S.**
80. **Onboarding by three works.** Paste three AO3 URLs, get a taste vector and a
    first list. The import path exists. **BUILD, M.**
81. **Bookmark import by URL.** The bookmarks importer exists and writes page by page;
    wire it to a form. **BUILD, M.**
82. **A tag's rise and fall.** `tagtrend` computes it; the page shows a table, not a
    sparkline. **BUILD, S.**
83. **A calendar view for seasons.** `tagseason` finds them with `MinYears >= 3`;
    twelve months of a tag's year-share is one chart. **BUILD, M.**
84. **Theme browser with search.** `tagtree` builds families with coherence 0.888 and
    stability 0.63; a search box over 200 themes is the missing half. **BUILD, M.**
85. **Keyboard shortcuts on the comparison pages.** `/algo` and `/arena` are
    left/right decisions; make them arrow keys. **BUILD, S.**
86. **A mobile layout for the recommend page.** The lists are tables; tables do not
    work on a phone. **BUILD, M.**
87. **Explain diffs between two lists.** When two tunes disagree, show which items
    moved and which signal moved them — the fastest way to debug a weight.
    **BUILD, M.**
88. **A permalink for a recommendation.** Same seeds, same tune, same list, shareable.
    **BUILD, S.**
89. **A weekly digest.** "What rose in your fandoms" — `tagtrend` + a feed.
    **BUILD, M.**
90. **An explicit "surprise me" that is honest.** Sample from the tag-gap surface, not
    from popularity, or it is the same list as everything else. **BUILD, S.**
91. **Show the price of diversity.** The list's overlap and fandom spread are measured;
    a reader who wants twenty works of one fandom should be able to say so, and the
    number should change. **BUILD, S.**
92. **Accessibility pass on the two most-used pages.** Focus order, contrast, and the
    `<h1>` duplication the layout helper already guards against. **BUILD, S.**

## G. Governance, privacy, trust (93–100)

93. **K-anonymity floor on anything shared.** `tag_affinity` already requires 20
    bookmarks for a row; a shared shelf or digest needs the same rule, stated on the
    page. **BUILD, S.**
94. **Erasure across the schema.** Deleting a reader must reach interactions, arena
    votes, taste rows, owner keys and any dump. One command, tested against every
    table. **BUILD, M.**
95. **An audit log for tune changes.** `tune --set` overwrites silently today; who
    changed which weight, when, and what the run after it measured. **BUILD, S.**
96. **A machine-readable provenance endpoint.** `PROVENANCE.md` and the deploy gate
    already compare the binary's revision with HEAD; serve that comparison.
    **BUILD, S.**
97. **Rate limit the public pages.** The API has a shared token for writes; reads are
    unbounded and a crawler can make the mirror unreachable. **BUILD, S.**
98. **State what the recommender does NOT use.** No reading history beyond bookmarks,
    no dwell time, no click tracking — printed on the page, so the absence is a
    feature rather than an omission. **BUILD, S.**
99. **A second-instance privacy story before a second instance.** Rows 32 and 39 both
    want one; neither should ship before the differential-privacy question is answered
    in writing. **DATA / decision.**
100. **Publish the evaluation method.** The split, the metric, the power floor and the
    "no verdict" rule are unusual and defensible; a page that explains them turns the
    numbers from claims into evidence. **BUILD, S.**

---

## How to use this list

- Rows **3, 5, 6, 35, 43, 44, 48, 70** compound: each one makes the next measurement
  cheaper or more trustworthy. The instrument before the experiment.
- Rows tagged **STREAM** and **DATA** are not failures. A backlog where half the rows
  are unbuildable is a fact worth writing once, not rediscovering per row.
- Every BUILD row that changes ranking needs the same closing sequence: implement,
  `go test ./...`, two arms with `--dump-users`, `kindred evalcmp`, then a handoff
  naming the interval — never a point estimate.
