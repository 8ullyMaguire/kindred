# Handoff 2026-10-23i — the byline is a page, and the shelf says how much is left

Two rows that were both PARTIAL for the same reason: the data and the engine
were there, and no page reached them.

## #66 Author recs from a single work — BUILT

The row's own words: "`/api/v1/authors/similar` exists; the work page does not
link to it and no template renders it."

It was worse than that. The work page did not name its author **anywhere** — no
byline at all — so the endpoint had no entry point and the author was a string a
reader had to retype into `/author`.

Built:

- `data-testid="work-byline"`: the byline, split with `arena.SplitAuthors`, each
  name an `<a rel="author" href="/author?q=…">`.
- "Authors who write like this one": the same `SimilarAuthors` the endpoint
  serves, with the shared-work count and the evidence line, plus its own error
  and empty states.
- `corpus.Byline(ctx, id)` — because `corpus.Entity` deliberately does **not**
  carry the byline: every ranking path holds tens of thousands of entities and
  nothing in the ranking reads an author string. The one surface that needs it
  asks for it by id.

Live on `/work/17249318`: one byline testid, `href="/author?q=Titus621"` plus the
split co-authors, and a populated author list naming Paranormal_Knight and
others.

## #50 "Complete your collection" — BUILT

`/author?q=` now prints, for a reader with a session:

```
You have read 2 of 4 works whose byline matches this name — 2 to go.
Show only the ones I have not read
```

and `?unread=1` answers `Showing 2 of 2` with exactly the unread ids.

**Two databases that cannot be joined.** The corpus knows an author's works, the
state database knows what a reader read, and there is no ATTACH between them.
`store.ReadAmong` reads the read set for a bounded id list, 400 ids per
statement, and the page computes the count from it.

**Three exclusions, each a claim that would otherwise be false:**

| exclusion | why |
|---|---|
| only SHOWN is not read | `seen_works` records both; a "to go" count that counts a recommendation as progress flatters the reader |
| another reader's reads are not yours | the count is about a person |
| no session, no claim | an anonymous reader must not be told "0 of 4" — they get no line at all |

**The count is over the SHELF, not the page.** "2 of 20" where 20 is the page
size would be a number about pagination. Capped at 5,000 ids, and the page says
"counted over the first 5,000, so this shelf may be longer" when it hits the cap.

**`OwnerForSession` is not `TouchSession`.** Rendering a list is not activity, and
"last seen" is a number other parts of the site report.

Live, signed in, with two of Titus621's four works marked read:
`data-read="2" data-total="4"`, "2 to go", and the filtered list at `2 of 2`.

## A gate defect this deploy found

`'/author?q='` as a gate pattern can never match: in an ERE, `?` makes the
preceding character optional, so the pattern means `/autho` + optional `r` +
`q=`. The check reported a working link as missing on the first deploy and cost a
retry. The pattern is now `'href="/author'`, and the comment in the script says
why — this is the second time this gate has been wrong about its own regex.

## Mutations killed

| mutant | killed by |
|---|---|
| a shown work counted as read | `TestTheAuthorPageSaysHowMuchOfTheShelfIsRead` |
| `?unread=1` made inert | `TestTheUnreadFilterShowsOnlyWhatIsLeft` |
| the filtered list not announced | same |
| the shelf counted as one page | `TestTheAuthorPageSaysHowMuchOfTheShelfIsRead` |
| the byline dropped from the page | `TestTheWorkPageNamesItsAuthorAndLinksTheByline` |
| the author list dropped | `TestTheWorkPageShowsAuthorsWhenTheEngineFindsThem` |
| a byline rendered but not linked | `TestTheWorkPageNamesItsAuthorAndLinksTheByline` |

## State

- 27/27 packages green; deployed; provenance IN SYNC; gate **87 checks**.
- Roadmap: **37 built, 10 partial, 49 open, 4 without a premise**.

## Next

The remaining PARTIAL rows are the shortest path to more product per unit of
work, and each has a named gap:

- **#54 rich tag profile** — `/tag/{id}` has the count, the works and a
  neighbours sidebar; it lacks median kudos/words, the most-bookmarked work and a
  trend line. All three are corpus aggregates, no model.
- **#45 tag canonical form** — `families.json` + `fandom.base()` exist; there is
  no alias table and the index is never merged.
- **#79 diversity metric** — `diversify` enforces MMR and reports what it
  dropped; `eval_runs` does not record intra-list diversity, so the effect of
  the cap is unmeasured.

## Instrument notes

- Running SQL through `ssh '…'` + `sudo sqlite3 "…"` + Go string quoting is three
  layers of escaping and it **fails silently**: an INSERT that looked fine left
  no row (the page then honestly said "read 0"). Write the SQL to a file on the
  host and feed it with `< file`, then `SELECT COUNT(*)` to confirm — the count is
  the only evidence the write happened.
- `seen_works` is `WITHOUT ROWID`, so `ORDER BY rowid` is a prepare error.
- The deployed binary is `/usr/local/bin/kindred`; `~/.local/bin/kindred` is stale.
