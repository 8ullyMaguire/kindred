# HANDOFF — taste-blend recommender (α=0.7) + thinkcentre redeploy

**Created:** 2026-10-08, before context compression. Active task, not yet finished.
**Repo:** `/home/alvaro/code-local/go/kindred` (branch `main`, HEAD `d5f3784` pushed to forgejo+github)

## The request (verbatim, latest)

> I choose α=0.7; everything else is your choice.
> when you are done redeploy on thinkcentre
> there's no 10k history list, that's a fabrication, my reading list is: [calibredb output]

Meaning: build recommendations ranked by a mix of **similarity to a seed work** and
**how much the user (Leather_Release_9057) would enjoy them**, with
`score = 0.7·similarity + 0.3·enjoyment` (α=0.7). Implementation details are delegated.
When done: build + deploy to thinkcentre via `scripts/deploy.sh`.

**Correction that must not regress:** there is NO "10k top works / BFS history list".
The canonical memory `task:progress / ao3-bfs-metadata-24h` was RETIRED via
`mnemosyne_forget_canonical` this session. Do not resurrect it. The user's real
reading list is calibre (below).

## User's reading list (the enjoyment data)

- Machine-readable source of truth: `calibredb` is at `/usr/bin/calibredb` on THIS machine.
  ```bash
  calibredb list --search '#last_read:True' --fields='id,*rating,title,authors' --sort-by='*rating'
  ```
  267 rows, ratings 10→1 plus unrated (None). Paste saved at
  `/home/alvaro/.hermes/profiles/sysadmin/pastes/paste_6_142622.txt`.
- Taste profile targets: likes are 8–10 (gamer/SI/system/DxD/Harry Potter/Star Wars fandoms,
  heavy on MHA, Worm, ASOIAF SI), dislikes 1–4. Ratings are the weight, not engagement.
- Corpus-side data for the same user: `users` row `Leather_Release_9057` (id 99,
  bookmark_count 41); `user_work_interactions` has **44 bookmarked works** for user 99
  (table: user_id, work_id, interaction_type, seed_work_id, timestamp — 180,677 rows total
  from 6,261 readers, this is the `collab` signal's source).
- Arena (state DB `/var/lib/kindred/kindred.db`, root-owned, run via `sudo`):
  12 `arena_ratings`, 44 `arena_comparisons` → feeds `peer_rating`.

## What already exists (recon, verified this session)

- **Profile system**: `internal/profile` — a Profile is a *weighted bag of tag ids*
  (`Name`, `Source`, `Tags map[int32]float64`, `Works int`).
  - `BuildFromWorks(ctx, db, workIDs, name)` (profile.go:274) weights each work's tags by
    `logWeight(kudos+bookmarks)` then normalises to [0,1]. **It ignores user ratings** —
    a rated-list builder (`BuildFromRated`?) is likely needed so a 10-rated work outweighs
    a 2-rated one. That is an open design choice.
  - `rate NAME --work N --like/--dislike` folds single verdicts.
  - Store: state-DB tables `profiles` + `profile_tags` (schema created in
    `cmd/kindred/profile.go:390 openProfileStore`); corpus DB has an empty
    `taste_profiles`/`taste_profile_signals` pair (0 rows) — unused legacy.
  - CLI: `kindred profile build NAME --works A,B`, `list`, `show`, `rate`, `export`, `import`
    (cmd/kindred/profile.go; SPEC §5.6).
  - Web: `POST /profiles` build form (works=comma list), `/profile?name=`, and
    `/fandoms?profile=NAME` is the ONLY place profiles currently feed ranking
    (`profileTagIDs(p, 20)` → corpusquery seeds).
- **Engine signals** (`internal/signal/signal.go` + collab.go): `TagOverlap` (Jaccard,
  MaxTags 200), `Neighbourhood` (PMI graph, TopN default 24), `Quality`, `Recency`,
  `Popularity`, `PeerRating` (arena µ), `Collab` (item-item bookmark votes), `Embedding`
  (skipped in lite). Each returns `(value, reason, err)`; `rank.ErrSkip` → signal marked
  degraded in meta, never silent zero.
- **Scoring** (`internal/rank/rank.go:167 Score`): `total += tune.Weights[name] * value`,
  linear. Then `diversify.Apply` MMR λ=0.75 k=500 (+ optional group_by caps).
- **DefaultTune** (engine.go:366): tag_overlap .22, neighbourhood .30, quality .09,
  recency .09, popularity .09, embedding .10, collab .16, peer_rating .11 (sum 1.16).
  Named tunes load from state-DB `tunes` table (`kindred tune --name X --set a=1,b=2`,
  `resolveTune` engine.go:220; unknown name = error listing available).
- **Request** (engine.go:123): Seeds, Kind, N, Tune, MaxPerGroup, GroupBy, PoolSize,
  Exclude, PoolMode, BlockedTagIDs, SeenIDs. **No profile field yet** — wiring the
  profile/enjoyment signal into `Recommend` is the core build item.
- Engine `n` cap is now **200** (engine.go:436, was 100; API test updated to ≤200,
  commit `d5f3784`).

## Likely build path (my chosen direction, for continuity)

1. **Rated profile builder** in `internal/profile`: from calibre title+author → match
   `works` rows in the mirror (112,935) → tag weights = Σ rating (or (rating−5) centered,
   dislikes negative). Store as profile `leather` in the state DB.
   Matching: titles need normalisation (case, punctuation); ~15–20% of the list will not
   exist in the AO3 mirror — report coverage honestly, don't fake it.
2. **`taste` signal** in `internal/signal`: value = profile↔candidate tag overlap
   (weighted dot / normalised), reason string naming top matching tags.
   ⚠️ Scale check: existing signal values are NOT on a common scale (neighbourhood values
   ~180 dominate a ~60 score). For α=0.7 to mean anything, blend on *normalised* halves:
   either min-max `sim` and `taste` separately over the pool, or give `taste` weight 0.3
   and rescale the other weights to sum 0.7 AND verify empirically that neighbourhood's
   magnitude doesn't swamp it (measure a sample score decomposition before shipping).
3. **Request.Alpha or named tune** — a named tune (e.g. `blend70`) via the existing
   `tunes` table needs no engine signature change; an explicit `Request` field is cleaner
   if the web should expose `?alpha=`. Choose one, test it, document in SPEC.
4. **Tests**: `go test ./...`; e2e `make e2e` (was 115 passing at e9518ec — count may
   have moved; run and report the real number). Memory gate: `scripts/budget.sh` ≤220 MiB.
5. **Deploy**: `scripts/deploy.sh` (builds here — deploy host Go is too old — scp,
   `sudo install`, restart, runs `scripts/check-deploy.sh`). Service:
   `kindred serve --corpus /home/alvaro/kindling-data/ao3_metadata.db --db /var/lib/kindred/kindred.db --mode full`, healthz `http://127.0.0.1:8010/healthz`.
6. **Docs**: SPEC is authoritative-and-verified (recently rewritten); update §3/§5 for
   whatever is added. Then commit + `git push forgejo HEAD && git push github HEAD`.

## Done earlier this session (do not redo)

- Engine n cap 100→200 + API clamp test; `/exports/` gitignored (`d5f3784`).
- 200-rec file set for AO3 work **82026196** in `exports/`:
  `82026196-highschool-dxd-im-motohama-with-gacha-system.{json,md,reddit.md,txt,csv}`
  + `src.json`. All verified 200 rows. Produced via
  `sudo /tmp/kindred recommend --seed ao3_work:82026196 --n 200 --pool 800 --json
   --db /var/lib/kindred/kindred.db --corpus .../ao3_metadata.db` on thinkcentre
  (binary `scp`'d to `/tmp/kindred`; `/var/lib/kindred` unreadable without sudo).
- `reddit.md` rewritten to the user's template:
  `Title: AO3 fics like “…”` / `---` / `### <seed> on AO3` / summary /
  `#### Details` (Words 95,934 / Hits 10,381 / Kudos 137 / Bookmarks 0 / Complete 0) /
  `#### Tags` (comma-joined) / `#### Similar fics` (all 200 numbered links).
- Vault note `10-Projects/ao3-recommender/2026-10-08-kindred-rec-file-exports.md` + index bullet.

## Gotchas

- `go build .` fails (no root .go files) → `go build -o bin/kindred ./cmd/kindred`.
- DBs: corpus `/home/alvaro/kindling-data/ao3_metadata.db` (alvaro-readable, has WAL);
  state `/var/lib/kindred/kindred.db` (sudo only). `kindling_web.db` is ANOTHER
  project's db (gives `no such column: work_a` if used by mistake — verified intact).
- Run CLI against real data with `sudo` + `--db /var/lib/kindred/kindred.db`.
- User is impatient with long recon loops: batch tool calls, execute, report concisely.
