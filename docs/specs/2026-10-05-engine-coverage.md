# What internal/engine covers now, and what it still does not

`internal/engine/engine_test.go` was added on 2026-10-05. Before it, the package
had **no test file at all** — 617 lines that turn a request into a ranked list,
with nothing exercising them. The `goal-check` clause `no untested packages` had
been failing on exactly this.

## What the tests cover

| test | what it pins |
|---|---|
| `TestParseSeed` | the `kind:id` grammar: 4 valid forms, 6 rejected |
| `TestParseSeedRejectsTrailingGarbage` | the `Sscanf` bug — see below |
| `TestRenormaliseRestoresTheOriginalTotal` | dropped weight is spread, total preserved |
| `TestRenormaliseMutatesItsInput` | it deletes from the map it was given |
| `TestRenormaliseWithNothingToSpread` | absent key and zero-weight key are both no-ops |
| `TestRenormaliseWithZeroSurvivorsDoesNotProduceNaN` | the `total == 0` guard, 3 shapes |
| `TestDefaultTuneIsUsable` | all 7 signal weights present and positive |
| `TestPoolSizesAreOrdered` | `DefaultPoolSize < FullPoolSize` |

## The bug this found

`ParseSeed` used `fmt.Sscanf(id, "%d", &n)`. Sscanf stops at the first non-digit
and **reports no error**, so:

    ?seed=ao3_work:12abc   ->  work 12
    ?seed=ao3_work:1.5     ->  work 1
    ?seed=ao3_work:%205    ->  work 5

A seed arrives from a public query parameter (`internal/web/handlers.go`,
`?seed=ao3_work:1&seed=ao3_work:2`), so the ranking was being computed for an
entity the request never named, and the response looked entirely normal. Now
`strconv.ParseInt(id, 10, 64)`, which rejects anything that is not entirely digits.

Mutation-checked, not asserted. Restoring the Sscanf version fails exactly one
test:

    --- FAIL: TestParseSeedRejectsTrailingGarbage
        ParseSeed("ao3_work:12abc") = {Kind:ao3_work ID:12}, want an error

## What is still untested, and why it matters more

The tests above all cover pure functions. **The parts that actually decide what a
user sees are untested**, because each needs a store, a corpus and a graph:

| function | what it does |
|---|---|
| `Recommend` | the whole request path: request → ranked list |
| `poolFor` | candidate selection, incl. the clamp against `FullPoolSize` |
| `signals` | turns candidates into signals plus a tune |
| `seedVector` | embedding vector from the seeds |
| `attachSummaries` | metadata for the returned items |
| `Embedding`, `PeerRatings` | fetch paths, including `float32FromLe` |

That is the honest state. Testing them properly means a fixture corpus and a live
database — which is the same prerequisite that made threadlight's suite unrunnable
on this host until a provisioning script existed. It is a real piece of work, not
a gap that can be closed by writing assertions.

What the new tests do buy is that the two pure functions now fail loudly when
someone changes them, and that `internal/web`'s dependency on `ParseSeed`
(input validation) is covered at all.

## `internal/web` — the other untested package

Also flagged by `no untested packages`, and 2029 lines across `arena.go`,
`handlers.go`, `render.go`, `embed.go`. **Not started.** Same obstacle: it is
handler code that needs a running server and a fixture corpus, and there is no
existing harness for that in this repo. Recorded here so the predicate's second
failure is documented rather than forgotten.