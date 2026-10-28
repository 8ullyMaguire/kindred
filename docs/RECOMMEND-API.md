# Recommendation interface — the estate's one recommender

**Decision 2026-10-28:** kindred is the estate's recommender. Projects call it; they do not grow their
own. This document is the contract, so that decision is implementable rather than aspirational.

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
POST /v1/recommend
{
  "namespace": "stash-box",          // required: which entity space
  "seeds":     ["<entity-id>", ...], // 1..50
  "limit":     50,                   // 1..200
  "exclude":   ["<entity-id>", ...], // optional, already-seen
  "min_score": 0.0                   // optional
}
200 {
  "namespace": "stash-box",
  "algorithm": "cooccurrence-v1",    // report it, so a caller can pin behaviour
  "results": [ {"id": "<entity-id>", "score": 0.83} ]
}
```

## Rules

1. **`exclude` is honoured, not advisory.** A deck that shows a work twice is broken in a way users
   notice; a recommender that ignores `exclude` is not a recommender here.
2. **Empty is a valid answer.** `200 {"results": []}` beats a padded list. Cold start returns few, not
   random.
3. **`algorithm` is part of the response.** Callers pin it in tests so a model swap is visible rather
   than silent.
4. **No personal data in, no personal data out.** Seeds are entity ids, never a member id. Per-member
   history stays in the caller; kindred does not learn who asked.
5. **Scores are comparable within one response only.** Never across namespaces, never as a percentage
   shown to a user.

## Migration order for callers

Cheapest first, because the payoff is per-caller: stash-box discovery → tessera discovery → darkhold
continue-reading → blackrep deck ordering (last: its policy constraints are the strictest, so it needs
the most review).

## Known limits

`recall@1000 = 0.0284` on the current AO3 candidate set is the real blocker, and it is kindred's problem,
not its callers'. An interface in front of weak candidates is still weak candidates — which is why
`docs/what-is-left.md` ranks candidate generation above every integration here.
