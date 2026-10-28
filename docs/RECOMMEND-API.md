# Recommendation interface — the estate's one recommender

**Decision 2026-10-28:** kindred is the estate's recommender. Projects call it; they do not grow their
own. This document is the contract, so that decision is implementable rather than aspirational.

**Correction 2026-10-28:** the first version of this document described an endpoint kindred does not
serve — `POST /v1/recommend` taking `namespace`, `limit`, `exclude` and `min_score` in a JSON body.
The server reads the **query string** (`parseRecommend`, `internal/api/server.go`), there is no
`namespace` concept, and no `exclude` or `min_score` parameter exists at all. `POST` decodes seeds and
the ranking knobs and nothing else, so this is not a half-implemented body API to be finished later:
the document has been corrected to what the handler actually serves, and the body-shaped version is
recorded here as a design that was never built.

## Why kindred

It already exists, it is a general entity recommender over a read-only local mirror, and it is sized to
run on modest hardware — so adding a caller costs an HTTP route, not a system. Four other projects
contain a recommender-shaped hole (stash-box discovery, tessera discovery, blackrep deck ordering,
darkhold continue-reading) and each would otherwise invent one.

## The split that makes this safe

**Shared: candidate generation.** Given seeds, return ranked entity ids.
**Not shared: ranking policy.** Each caller applies its own constraints to the candidate list, because
the constraints are product rules, not algorithm choices — blackrep's standing rule is that nothing
sexual may contribute, display or sort. A generic scorer dropped in would violate it silently.

So: kindred answers "what is near these seeds"; the caller decides what may be *shown*.

## Endpoint

```
GET /api/v1/recommend?seed=ao3_work:1234&n=50
```

`POST /api/v1/recommend` also exists and takes a JSON body, but it is a **read**: it changes no row,
and it is deliberately not behind the write token that guards kindred's mutating routes. POST carries
the seeds and the ranking knobs; the filters stay query parameters on both methods.

```json
{"seeds": ["ao3_work:1234"], "kind": "ao3_work", "n": 50}
```

### Query parameters

| Parameter | Meaning | Default |
|---|---|---|
| `seed` | `kind:id`, e.g. `ao3_work:1234`. Repeat it or comma-separate several. | **required** (`400` without one) |
| `kind` | Entity kind to rank. `ao3_work` is the only kind this mirror holds; anything else is a `400` naming the kind. | the first seed's kind |
| `n` | How many results. | `10`, clamped to `200` |
| `pool` | Candidate-pool size before ranking. Bounds memory, not quality. | `0` = the deployment's default (200 in lite mode, 1000 in full) |
| `tune` | Named signal-weight vector. | `default` |
| `group_by` | `fandom`, `tag`, `author`, or empty for no grouping. | `""` |
| `max_per_group` | Cap on results per group. | `0` = no cap |
| `exclude_seeds` | Drop the seeds from the result. | **`true`** — a seed returned at the top of its own list is worse than useless |
| `personal` | `personal=0` withholds the deployment's owner-cohort prior, so the caller sees the mirror's own opinion. Named for the opt-out because the prior is on by default. | owner prior on |
| `pool_mode` | `tags`, `collab`, `tags+collab`, `tags_or_collab`. | the engine's default (`tags`) |
| `min_words`, `max_words`, `min_kudos`, `not_updated_within_days` | Numeric filters. | unset |
| `complete` | `any`/`all`, `complete`/`only`, `in-progress`/`wip`. | any |
| `rating`, `lang`/`language` | Multi-valued; repeat or comma-separate. | unset |
| `blocked_tag`, `exclude_tag` | Tag names (lowercased) or numeric ids. Unioned with the deployment's site-wide block list — a caller can add blocks, never remove the site's. | unset |

The deployment's **standing filters** (`KINDRED_STANDING_FILTERS`) fill only the keys the request left
unset.

### Response

```json
{
  "kind": "ao3_work",
  "seeds": ["1234"],
  "items": [
    {
      "id": 5678,
      "kind": "ao3_work",
      "title": "…",
      "url": "https://archiveofourown.org/works/5678",
      "tags": ["…"],
      "score": 0.83,
      "evidence": [{"signal": "tag_overlap", "value": 0.4}]
    }
  ],
  "meta": {
    "seeds": 1, "candidates": 200, "pool_size": 200, "tune": "default",
    "degraded": ["…"], "truncated": false
  },
  "tune": {"tag_overlap": 0.22, "neighbourhood": 0.30, "quality": 0.09}
}
```

`meta.degraded[]` names every signal that could not contribute, and `meta.shortfall` explains a list
shorter than `n`. Read them: a short list that says why is a different answer from a short list that
does not.

## Rules

1. **The seeds are excluded by default, and there is no `exclude` list.** `exclude_seeds` defaults to
   `true`, and a caller that wants "more like this, including this" has to ask for it. A caller that
   needs to suppress already-seen entities filters the response itself — which is where that product
   rule belongs anyway, and nothing on the server implements it.
2. **Empty is a valid answer.** `200 {"items": []}` beats a padded list. Cold start returns few, not
   random.
3. **`meta.tune` and the `tune` map are part of the response.** Callers pin them in tests so a model
   swap is visible rather than silent.
4. **Seeds are entity ids, never a member id.** A caller that presents kindred's `kindred_arena`
   session cookie also gets that session's learned taste folded into the ranking — that is the
   reader's own model, not an identity kindred stores about them, and a caller with no cookie is
   served the deployment's ranking instead.
5. **Scores are comparable within one response only.** Never across requests, never as a percentage
   shown to a user.

## Migration order for callers

Cheapest first, because the payoff is per-caller: stash-box discovery → tessera discovery → darkhold
continue-reading → blackrep deck ordering (last: its policy constraints are the strictest, so it needs
the most review).

## Known limits

`recall@1000 = 0.0284` on the current AO3 candidate set is the real blocker, and it is kindred's problem,
not its callers'. An interface in front of weak candidates is still weak candidates — which is why
`docs/what-is-left.md` ranks candidate generation above every integration here.

The interface is also **AO3 works only**: `kind` has exactly one legal value today. A caller in another
entity space (stash-box scenes, tessera documents, blackrep decks) cannot use it as written — it can
only use it as a design, and the multi-namespace shape this document originally claimed would have to
be built before those callers could point at kindred directly. That is a decision for whoever needs it,
not a gap in the documentation.
