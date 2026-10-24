# Handoff 2026-10-24h — session end, state, and a triage of every remaining row

Read this first. It is written for a session that knows nothing about the last one.

## Where things stand

- **Repo:** `/home/alvaro/code-local/go/kindred`, branch `main`, clean at `5158928`.
- **Suite:** 28 packages green, `gofmt -l internal/ cmd/` empty.
- **Deploy gate:** `bash scripts/deploy.sh` → **117/117 checks** against
  `http://127.0.0.1:8019` on thinkcentre, provenance IN SYNC.
- **Roadmap:** `docs/PLAN-recsurfaces-31-100.md` → **28 built, 6 partial, 32 open, 4
  without a premise** (70 rows). Counts are produced by `scripts/check-roadmap.py`,
  never remembered; run it after any status edit.
- **Live data sizes:** corpus 112,937 works / 634,249 tags / 3,894,394 work-tag edges /
  108,195 summaries >80 chars / 18 languages. Mirror: 634,232 tag embeddings, one width
  (32), `kind='tag'` only. State DB: 180,677 `user_work_interactions` over 6,261 users,
  149 arena comparisons, **0 algo comparisons**, 108 `reader_visits`, 0 blocked tags.

## What shipped this session

| row | unit | commit |
|---|---|---|
| #74 | `signal.Walk` + `walkpool.go` (personalised PageRank; pool source so 2-hop finds are reachable) | `e3a668e` |
| — | `eval_runs.tune`: a run says which weights it measured | `a09e17a`, `9693641` |
| #39 | shrunk kudos on the tag shelf `(Σ + 10·prior)/(n+10)`, prior printed | `23bff15` |
| #53 | `arena.FitBT` + Spearman cross-check on `/leaderboard` | `6133b4f` |
| #31 | `ModeTagTrend` — window vs the corpus's own growth | `066427b`…`5158928` |

Handoffs for each: `HANDOFF-2026-10-24d-walk.md`, `…e-tagquality.md`, `…f-bradleyterry.md`,
`…g-tagtrend.md`.

## The triage: 38 rows remaining, and which of them are worth building

Verdicts are grounded in the data sizes above, not in how good the idea sounds.

### DO — data is in hand (ordered by value ÷ effort)

| row | why it is worth it |
|---|---|
| **89** ALS | `user_work_interactions` is 180,677 rows over 6,261 users — the one table holding what readers CHOSE, and `collab` only counts pairs from it. A 32-factor ALS at this size is seconds of CPU in Go, and it is a dimension that is not a function of tag co-occurrence. Rejected once for training cost; the measured size says that objection is stale. |
| **34** tag gap | 3.89M co-occurrence edges and no new data needed. "Tags this tag's neighbours carry that its own works rarely do" is writer-facing, and the roadmap has no writer surface at all. |
| **55** explanation-quality eval | Needs no new data: perturb the top evidence signal, recompute, and see whether the rank moves the way the explanation claims. Turns "explanations render" into "explanations are faithful". |
| **45** tag canonical forms | `families.json` is 83 entries — a seed, not a mapping. An alias table improves recall on every page and is the prerequisite for #75. |
| **35** author co-readership | Same interaction table, a different question: `SimilarAuthors` is tag-profile overlap today, which cannot see co-reading. |
| **37** tag hierarchy | 634,232 tag vectors already exist at dim 32; clustering them is cheaper than any of the model-shaped rows and gives tag pages something to navigate. |
| **93** fairness/exposure | Fandom and author exposure are computable from data the pages already serve; no personal attributes, so it does not conflict with the privacy design (the row's stated blocker). |
| **38** (PARTIAL) quality-adjusted co-occurrence | Edges are shrunk by evidence only; shrinking by kudos is a small change to `collab.go` with a measurable claim. |
| **32** (PARTIAL) completion weight | Completion is a filter; making it a tune dimension is a weight and an ablation. |
| **80 + 44** seasonal | `corpusquery.dayExpr` now normalises the mirror's two date formats, which is the missing piece; a per-tag month distribution is one aggregate. |
| **48** reader velocity | `user_work_interactions.timestamp` exists, so cadence is computable — `reader_visits` alone is 108 rows and would be noise. |
| **52** summary-style similarity | 108,195 summaries >80 chars. A TF-IDF pass in Go needs no model and writes work-level vectors, which is also what unblocks #47. |
| **75** cross-fandom trope transfer | Only after #45: it is "same non-fandom tags, different fandom", which needs canonical tags to be meaningful. |
| **47** work cluster labels | Only after #52 (needs work-level vectors). |
| **36** taste export/import | Cheap, and it makes #81 a format someone could implement later. |
| **77** tag suggestions | After #34; same query, a different sort. |
| **72** (PARTIAL) embedding dim search | `embed --dim` exists; three re-embeds answer "is 32 right". Expensive in wall-clock, cheap in thought — do it once, not for every row. |

### NOT WORTH IT — and the reason, which is the part worth keeping

| row | verdict |
|---|---|
| 68 tag temperature, 69 controversy, 82 exploration, 86 bandit weights | All four need a **judgement stream**. Live: 149 arena comparisons and **0 algo comparisons** — the tune-vote page has never received a single vote. Per-tag variance over that is noise, and a bandit whose reward stream is empty is not a policy. The significance work (#84) already states what this volume can support. |
| 85 causal inference | No instrument and no counterfactual log. IPS was tried here and rejected as algebraically vacuous on a deterministic policy (propensity 1/0, estimator = identity). |
| 49 tag polarity | `arena_user_blocked_tags` has **0 rows**. Nothing to infer polarity from; revisit if blocks ever exist. |
| 81 federated taste | No second instance exists. `dump`/`fetch` move mirror snapshots; a taste exchange with no peer is a format nobody consumes. |
| 83 wrangling-graph transfer | The mirror carries no AO3 wrangling or synonym data. |
| 87 GNN, 88 attention fusion, 90 RL sequencing, 91 cross-lingual, 92 LLM explanations, 99 NL query | Each needs a training pipeline or a runtime model the project deliberately does not have (pure Go, no GPU, no runtime LLM). They would produce code nobody can measure on a 37-list eval — which is the same objection that killed the first ALS estimate, applied where it still holds. |
| 94 social layer | No cross-instance identity graph. |
| 95 lifecycle prediction | The mirror is a snapshot; `first_seen` + `update_date` is two observations, not a time series. |
| 96 per-tag embedding width | Fit and storage cost for a precision win nothing here can measure. |
| 59 t-SNE tag map | A projection is a picture, not a ranking, and 634k points need downsampling plus a library the repo does not have. |
| 73 Pareto ranking | Needs a second objective a reader actually weights; with one scalar the "front" is decoration. |
| 58 productivity/quality scatter | A chart of two numbers the page already prints. |

That is 18 worth building, 20 not, and the reasons for the 20 are the useful output:
most of them are blocked by **data the project does not have**, not by effort.

## If the next session only does three things

1. **#89 ALS** — biggest gap in what the recommender can see, and the data is already
   there.
2. **#34 tag gap** — the only writer-facing surface in the backlog.
3. **#55 explanation quality** — the project's own standard applied to the surface that
   currently gets the least scrutiny.

## Rules a new session must not relearn the hard way

- **The deploy gate is the instrument, and it must not generate the data it verifies.**
  It reads `/eval`, never `/algo?mode=interleave` (that records impressions by
  rendering).
- **Every eval run names its tune.** `--tune <name>`; `''` means "predates the column",
  `default` means the default. Four unattributable rows accumulated before this column.
- **The mirror stores dates as `05 Sep 2007`** (112,893 rows) and ISO on two. Compare
  them as dates via `corpusquery.dayExpr`; `"31 Oct 1999" >= "2020-12-12"` is TRUE
  lexically.
- **Windowed queries anchor on the mirror's newest date, not today**, and widen until
  the window holds a sample (90→180 days, 2→16,153 works live).
- **API items are keyed `id`, not `work_id`.** Comparing `work_id` yields two lists of
  `None` that compare equal — this produced a committed claim that the walk changed
  nothing.
- **A two-hop signal needs its own pool source** (`walkpool.go`, `collabpool`); scoring
  a one-hop candidate set can only re-rank what `tag_overlap` found.
- **`gofmt -l` and the test count must be read from the same output**, or a formatting
  flag hides behind a green count.
- **A shrunk estimate sharpens with more data**; pure MLE is scale-invariant and the
  shrunk fit is not. Both are testable (`FitBTWithPrior(…, 0)`).
- **A correlation over a thin fit is a number about the prior.** `data-bt-thin` and the
  `<3 per item` rule say so on the page.

## Verification commands

```
cd /home/alvaro/code-local/go/kindred
gofmt -l internal/ cmd/; go test ./... -count=1 | grep -cE '^ok'   # 0 and 28
python3 scripts/check-roadmap.py                                    # 28/6/32/4
bash scripts/deploy.sh                                              # 117 checks
```
