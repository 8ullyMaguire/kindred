# Handoff — #47: work themes, and the two filters that make a label a label

2026-10-24. `internal/tagtree` (work side), `kindred worktree --write`, `/themes` +
`/themes/{id}`, `GET /api/v1/themes[/{id}]`.

## The derivation, and why the weighting is the unit

A work has no embedding of its own — the archive holds 32-dim vectors for TAGS, because a tag is
a thing thousands of works share and a work appears once. So a work's position is derived: the
**inverse-sqrt-weighted mean of its tags' vectors**, normalised to unit length.

    weight(tag) = 1 / sqrt(support(tag))

A flat mean makes every fluff-tagged work identical and hides the rare tag that identifies it:
"fluff" is on 60,000 works, "Alternate Universe - Time Loop" on 400. The test for this is shaped
so a flat mean cannot pass it — work A shares the common tag with B and a rare tag with C, and
under a flat mean the two similarities are equal BY SYMMETRY. Mutating `w = 1/√s` to `w = 1`
kills it.

`Center` is OFF here, and the reason is the opposite of #37's: for tags the shared popularity
direction was noise that had to be removed (uncentred, one family took 83% of the archive); for
works, "many works about the same popular thing" IS a theme.

## The label rule

Labels are the tags **over-represented inside** a theme, not its biggest tags:

* `share ≥ 8%` of the theme's works — a tag on one work of ten is a coincidence;
* `lift = share / global share ≥ 2` — a tag as common outside as inside says nothing.

Without the lift filter every theme was named "fluff · angst · alternate universe", which is the
archive's water. Both filters have killers, including a tag on exactly one work of ten (huge
lift, no evidence) that only the share floor keeps out.

## Measured, 60,000 works → 200 themes

    coherence  own 0.879   neighbouring 0.186   gap 0.693
    stability  0.63 adjusted Rand against a second seed
    largest theme 2% of works

**Stability 0.63 is reported, not celebrated**: a work vector is a mean of a handful of tags, so
about a third of works change theme between seeds. The themes are real and their boundaries are
soft — that is what the number says, and it is in the report line, not in a footnote.

What the themes look like (tags only; nothing told the clustering about fandoms):

    [24]  vision (marvel) · bruce banner · sam wilson (marvel)      1,281 works
    [ 6]  kurogiri (my hero academia) · tokoyami fumikage · iida    1,186 works
    [157] akimichi chouji · haruno sakura & uchiha sasuke & …        1,122 works
    [ 39] aphrodite (percy jackson) · hera (percy jackson) · …       1,114 works
    [ 28] taylor hebert | skitter | weaver · rachel lindt | bitch   989 works
    [  4] duke thomas · stephanie brown · cassandra cain             912 works

## Surfaces

    tag_family / tag_family_member      (#37, tags)     — unchanged
    work_family / work_family_member    (#47, works):   id, parent, label, size / work_id, family, dist
    GET /api/v1/themes[?limit]          biggest first, with a 3-work sample per theme
    GET /api/v1/themes/{id}[?limit]     the theme's works
    /themes, /themes/{id}               in the nav under "Find something to read"

An **unbuilt tree answers `built: false` with a note**, not an empty list: a reader cannot tell
"no themes exist" from "the build is missing", and they are different facts. The gate checks both
directions — `built:true` from the API, and 200+ families / 1,000+ members in the tables.

## Tests and mutants

Six tests, five mutants, **all five killed**: flat weighting, lift filter removed, share filter
removed, no normalisation, ShortTag not trimming. Two fixture lessons:

1. The first rare-tag fixture compared two pairs that each shared an IDENTICAL tag set, so both
   cosines were 1.0 and the assertion could never fail. Rebuilt around a triangle (A shares the
   common tag with B, a rare tag with C).
2. The normalisation mutant SURVIVED until the test asserted unit length directly: every other
   assertion was a COSINE, and cosine is scale-invariant, so dropping the normalisation was
   invisible to all of them. The contract "unit length, so Euclidean distance is cosine distance"
   needs its own assertion.

Also fixed on the way: both new pages rendered **two h1s** (the layout emits
`<h1>{{.Heading}}</h1>` when Heading is set, and the templates carry their own). One h1 per page
now, verified live.

## Files

    internal/tagtree/worktree.go        LoadWorks, LabelThemes, ThemeName, ShortTag
    internal/tagtree/worktree_test.go   the triangle fixture, labels, shortening
    cmd/kindred/worktree.go             the command, the write, the report
    internal/api/themes.go              the two routes
    internal/web/themes.go              the two pages
    internal/web/assets/{themes,theme}.html

Runtime: **~60 s** for 60,000 works including the second seed. Deterministic for a given seed.

## Not done

The themes are not a signal and do not change any ranking — the row asked for clustering and
labels, and the honest next step is an ablation (a theme-mate boost), not a quiet weight. The
catch-all problem #37 reported at the tag level does not appear here (largest theme is 2% of
works), because works are far more evenly spread than tags.
