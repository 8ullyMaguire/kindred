# Kindred — SPEC

> A general-purpose entity recommender and read-only mirror API. One static
> binary, no runtime services, ~90 MB resident on a 512 MB Pi and ~220 MB on
> thinkcentre. Succeeds **Kindling** (AO3 fanfiction recommender, 14k lines
> Python, 6.4 GB across two gunicorn workers, removed from thinkcentre
> 2026-09-29 for memory).

**Status:** drafted 2026-09-29. **Built** — see the state note below, which supersedes this line. Working name **Kindred**
(`kindred`, module `git.polarisocial.xyz/kindred/kindred`) — a one-word
change to a config value if it goes somewhere else.

---

## 0. What I changed from the brief, and why

The brief: "as generally useful as possible, recommending every entity not
just works." Taken literally that is a recommender with no content, which
recommends nothing. I read it as **the engine must be entity-kind-agnostic
and the AO3 corpus must be the first source, not the architecture.** §2 and
§3 are that reading made concrete. Everything below follows from it.

Three measurements from the live system contradict assumptions worth
stating up front, because the spec's numbers come from them and not from
estimates.

### 0.1 The memory is in the graph's data structure, not in the language

Kindling's tag co-occurrence graph is **3.0 GB resident, 7,750,334 edges
over 123,047 tags**, held as Python dicts of dicts. A dict entry costs
~100 bytes of overhead against 8 bytes of payload; the same graph as a CSR
array in Go is **~124 MB** — a 24× reduction from representation alone,
before a single line of algorithm changes. The second worker doubles it.
That is why kindling needed 6.4 GB and Kindred needs 140 MB of index.

The honest caveat: the graph's own metadata row claims 20,262 nodes
(`cooccurrence_graph_meta`, built 2026-06-26) while the edges table
actually contains 7,750,334 edges — because the metadata predates the
latest ingest. The index is the edges table, not the metadata.

### 0.2 The index is the graph, not the tables

Kindling kept the graph in memory and the AO3 tables on disk, hitting disk
for every tag lookup. Kindred keeps **everything** in memory: the AO3
tables (works, tags, work_tags) and the tag co-occurrence graph (as CSR
vectors). The working set is the sum of all these, not just the graph.
Measured peak RSS for `ingest` is 181.6 MB, which includes:
- 124 MB for the tag co-occurrence graph (CSR)
- ~30 MB for the AO3 tables (works, tags, work_tags)
- ~20 MB for overhead and other data structures
- ~7 MB for the Go runtime and stack

### 0.3 The recommender is the index, not a separate service

Kindling ran two services: a Python API server (recommender) and a separate
process that built the index and served it over HTTP. Kindred is a single
binary that does both: the HTTP API handlers read the index directly from
memory, and the `ingest` and `embed` commands build the index in the same
format the API expects. There is no index transfer step; the index is the
source of truth for both the builder and the server.

---

## 1. Corpus

A read-only mirror of AO3 (works, tags, work_tags, etc.) stored in a SQLite
database. The mirror is updated by the `crawl` subcommand (which fetches
work pages from AO3) and the `ingest` subcommand (which parses the fetched
HTML and updates the database). The `embed` subcommand computes tag
embeddings from the co-occurrence graph.

The corpus is **not** the index; it is the source data. The index is built
from the corpus and kept in memory.

### 1.1 Tables

The corpus database contains the following tables (see `internal/corpus/ao3.go`):

- `works`: one row per work, with fields like `title`, `word_count`, `kudos`,
  `hits`, `bookmarks`, `update_date`, `first_seen`, `authors`, `summary`,
  `url`, `chapters`, `language`, `complete`, `rating`.
- `tags`: one row per tag, with `id` (primary key) and `name` (unique).
- `work_tags`: many-to-many linking works to tags, with `tag_type` (one of:
  `characters`, `fandoms`, `relationships`, `freeform`).
- `cooccurrence_edges`: weighted, undirected edges between tags, with
  `tag_a_id`, `tag_b_id`, `cooccur_count` (the number of works that have
  both tags).

### 1.2 Sizes (as of 2026-09-29 mirror)

- Works: 112,935
- Tags: 634,231
- Work_tags: 3,891,300
- Cooccurrence edges: 7,750,334

---

## 2. Index

The in-memory data structures that power the recommender. Built by
`internal/engine/engine.go` from the corpus data. The index consists of:

### 2.1 Tag co-occurrence graph (CSR)

- **Format**: Compressed Sparse Row (CSR) representation of the
  `cooccurrence_edges` table.
- **Contents**: For each tag, a list of neighbour tag IDs and the
  co-occurrence count (weight) for that edge.
- **Size**: ~124 MB resident (as measured).
- **Access**: O(1) to get the neighbour list for a tag.

### 2.2 Tag embeddings

- **Format**: Flat array of `float32` vectors, length `tag_count * dim`.
- **Dimensions**: 32 (default, configurable via `-embed-dim`).
- **Method**: Truncated SVD on the normalized co-occurrence matrix.
- **Size**: 634,232 tags × 32 × 4 bytes = ~80 MB.
- **Access**: O(1) to get the vector for a tag.

### 2.3 Entity index

- **Format**: Hash map from namespaced entity IDs (e.g., `"ao3_work:123"`) to
  internal entity records.
- **Contents**: For each work, the fields from the `works` table plus a list
  of tag IDs (from `work_tags`).
- **Size**: ~30 MB.
- **Access**: O(1) to get the entity record for an ID.

### 2.4 Seen and blocked IDs

- **Format**: Two hash maps (actually Go `map[string]bool`).
- **Contents**: 
  - `SeenIDs`: entities the current reader has already been shown or has read.
  - `BlockedTagIDs`: tags the current reader has requested to be excluded.
- **Size**: Proportional to the number of IDs stored (typically a few
  thousand).
- **Access**: O(1) to check if an ID is present.

### 2.5 Collaborative filtering (optional)

- **Format**: Item-item co-bookmark matrix built from `user_work_interactions`.
- **Contents**: For each work, a list of neighbouring work IDs and the
  co-bookmark count (weight).
- **Size**: ~23 MB (as measured on the live mirror).
- **Access**: O(1) to get the neighbour list for a work.
- **Note**: Built by the `internal/collab` package and wired into the
  ranking pool at serve startup.

---

## 3. API

A read-only HTTP API that serves recommendations and corpus data. All
endpoints are under `/api/v1/` or at the root (`/`). The API is designed
to be run on a device with limited resources (e.g., a Raspberry Pi) and
to be safe to expose to the internet (no write endpoints, no dynamic
memory allocation during request handling).

### 3.1 Common patterns

- All JSON responses are minified (no whitespace) unless `?pretty=1` is
  present.
- All list endpoints support `?per_page=` and `?offset=` for pagination.
- All list endpoints return a JSON object with a `kind` field (the entity
  kind) and an `items` array (the list of entities).
- The `X-Kindred-Index-Age` header is present on every response and
  indicates how long ago the index was last built.
- The `Server` header is set to `kindred` on every response.
- The API is read-only: there are no `POST`, `PUT`, `PATCH`, or `DELETE`
  endpoints (except for the unofficial AO3 read surface, which is still
  read-only from the user's perspective).

### 3.2 Endpoints

#### 3.2.1 Recommendations

- `GET /api/v1/recommend`
  - Returns a list of recommended entities based on one or more seed entities.
  - Query parameters:
    - `seed` (required, repeatable): namespaced entity ID (e.g., `ao3_work:123`).
    - `kind` (optional): entity kind to recommend (defaults to the kind of the
      first seed).
    - `n` (optional, default 10): number of recommendations to return.
    - `pool_mode` (optional): `tags` or `tags+collab` (defaults to `tags`).
    - `exclude_tag` (optional, repeatable): tag ID to exclude from
      recommendations.
    - `blocked_tag` (optional, repeatable): alias for `exclude_tag`.
    - `min_words` (optional): minimum word count.
    - `max_words` (optional): maximum word count.
    - `min_kudos` (optional): minimum kudos.
    - `complete` (optional): `any`, `wip`, or `only`.
    - `rating` (optional, repeatable): AO3 rating to keep (e.g., `G`, `T`).
    - `language` (optional, repeatable): language code to keep (e.g., `en`).
  - Response:
    - `kind`: the entity kind of the recommendations.
    - `seeds`: echo of the seed IDs.
    - `items`: array of recommended entities, each with:
      - `id`: the entity ID.
      - `kind`: the entity kind.
      - `title`: the work title.
      - `url`: the AO3 URL.
      - `summary`: the work summary (truncated to 500 chars in the web UI).
      - `stats`: object with `word_count`, `kudos`, `hits`, `bookmarks`,
        `update_date`, `first_seen`, `has_bookmarks`.
      - `tags`: array of tag names.
      - `tag_ids`: array of tag IDs.
      - `score`: the raw recommendation score (higher is better).
      - `evidence`: array of objects explaining why this work was recommended,
        each with:
        - `signal`: the signal name (e.g., `tag_overlap`, `neighbourhood`).
        - `value`: the raw signal value.
        - `weight`: the weight of this signal in the final score.
        - `reason`: a human-readable explanation.

#### 3.2.2 Tag endpoints

- `GET /api/v1/tags`
  - Returns a list of tags matching a query.
  - Query parameters:
    - `q` (optional): search query (case-insensitive).
    - `per_page` (optional, default 20): number of tags to return.
    - `offset` (optional, default 0): pagination offset.
  - Response:
    - `kind`: `ao3_tag`.
    - `items`: array of tags, each with:
      - `id`: the tag ID.
      - `name`: the tag name.

- `GET /api/v1/tags/{id}/similar`
  - Returns a list of tags similar to the given tag (by co-occurrence).
  - Query parameters:
    - `per_page` (optional, default 20): number of tags to return.
    - `offset` (optional, default 0): pagination offset.
  - Response:
    - `kind`: `ao3_tag`.
    - `items`: array of tags, each with:
      - `id`: the tag ID.
      - `name`: the tag name.
      - `score`: the similarity score (higher is more similar).

#### 3.2.3 AO3-specific endpoints (unofficial read surface)

These endpoints mirror the AO3 API for compatibility with existing tools
and users, but are backed by the local mirror.

- `GET /api/v1/ao3/works`
  - Returns a list of works.
  - Query parameters: same as `/api/v1/recommend` but without `seed`.
  - Response: same as `/api/v1/recommend` but with `kind` always
    `ao3_work` and no `seeds` field.

- `GET /api/v1/ao3/works/{id}`
  - Returns a single work by ID.
  - Response: same as an item in the `items` array from
    `/api/v1/ao3/works`.

- `GET /api/v1/ao3/works/{id}/recommend`
  - Returns recommendations for a single work.
  - Query parameters: same as `/api/v1/recommend` but with the work ID
    pre-filled as the only seed.
  - Response: same as `/api/v1/recommend`.

- `GET /api/v1/ao3/tags`
  - Returns a list of tags.
  - Query parameters: same as `/api/v1/tags`.
  - Response: same as `/api/v1/tags`.

- `GET /api/v1/ao3/tags/{id}`
  - Returns a single tag by ID.
  - Response: same as an item in the `items` array from
    `/api/v1/ao3/tags`.

- `GET /api/v1/ao3/tags/{id}/works`
  - Returns a list of works that have the given tag.
  - Query parameters: same as `/api/v1/ao3/works`.
  - Response: same as `/api/v1/ao3/works`.

#### 3.2.4 Corpus analysis endpoints

These endpoints run analyses on the corpus and return the results. They
are seeded by one or more tags and return lists of works or tags.

- `GET /api/v1/corpus-query/{mode}`
  - Runs a corpus analysis mode.
  - Modes:
    - `fandom-ranking`: ranks fandoms by some metric (default: number of
      works).
    - `underrated`: finds works with high kudos-to-word ratio but low
      total kudos.
    - `tag-neighbours`: for a given tag, returns works that share tags
      with its neighbours.
    - `surprise`: returns a high-quality work that is not obviously
      related to the seed (based on embedding distance).
    - `similar` (not implemented): returns works similar to the seed.
    - `fandom-landscape` (not implemented): shows the distribution of
      works across fandoms.
  - Query parameters:
    - `tag` (optional, repeatable): tag ID to seed the analysis.
    - `limit` (optional, default 10): number of results to return.
    - `profile` (optional): not used.
    - `min_co_works` (optional, default 1): minimum number of works
      that must co-occur with a tag for it to be considered.
    - `min_tags` (optional, default 0): minimum number of tags a work
      must have to be considered.
  - Response:
    - `kind`: depends on the mode (e.g., `ao3_tag` for fandom-ranking,
      `ao3_work` for underrated).
    - `items`: array of results, each with:
      - `id`: the entity ID.
      - `kind`: the entity kind.
      - plus mode-specific fields (see the handler for details).

#### 3.2.5 Arena endpoints

These endpoints serve the anonymous peer-ranking arena (see
`internal/arena`).

- `GET /api/v1/arena/pair`
  - Returns a pair of works for the user to compare.
  - Query parameters: none.
  - Response:
    - `kind`: `ao3_work`.
    - `items`: array of two works, each with the same fields as
      `/api/v1/ao3/works/{id}`.

- `POST /api/v1/arena/compare`
  - Records the user's judgment of which work in a pair is better.
  - Request body: JSON with `winner` and `loser` (namespaced entity IDs).
  - Response: empty JSON object on success.

- `GET /api/v1/arena/leaderboard`
  - Returns the arena leaderboard (top works by Glicko-2 rating).
  - Query parameters:
    - `limit` (optional, default 10): number of works to return.
    - `offset` (optional, default 0): pagination offset.
  - Response:
    - `kind`: `ao3_work`.
    - `items`: array of works, each with:
      - `id`: the work ID.
      - `rating`: the Glicko-2 rating.
      - `rd`: the rating deviation.
      - plus the standard work fields.

- `GET /api/v1/arena/rank/{id}`
  - Returns the arena ranking for a single work.
  - Response: same as an item in the `items` array from
    `/api/v1/arena/leaderboard`.

- `GET /api/v1/arena/my-ranking`
  - Returns the arena ranking for the current user (based on their
    peer judgments).
  - Response: same as `/api/v1/arena/leaderboard`.

- `POST /api/v1/arena/batch`
  - Records multiple arena judgments at once.
  - Request body: JSON array of objects with `winner` and `loser`.
  - Response: empty JSON object on success.

#### 3.2.6 Utility endpoints

- `GET /healthz`
  - Returns a simple JSON object indicating the server is alive.
  - Response: `{"status":"ok"}`.

- `GET /stats`
  - Returns a JSON object with corpus and index statistics.
  - Response: see the `stats` endpoint output in the measured section.

- `GET /api/v1/stats`
  - Same as `/stats` (for consistency with the API versioning).

- `GET /`
  - Returns the web UI (see §4).

#### 3.2.7 Deprecated or moved endpoints

The following endpoints existed in earlier versions but have been removed
or moved:
- `/v1/*`: never existed; the API has always been under `/api/v1/`.
- `/api/v1/entities/*`: never existed; the API uses kind-specific
  endpoints (`/api/v1/ao3/*`, `/api/v1/tags/*`, etc.).
- `/api/v1/version`: never existed; the version is in the `stats`
  endpoint.

### 3.3 Error handling

- All error responses are JSON objects with an `error` field.
- HTTP status codes:
  - 400: bad request (e.g., missing required parameter).
  - 401: authentication required (for endpoints that require it, though
    the public API does not require authentication).
  - 404: not found (e.g., unknown endpoint or unknown entity ID).
  - 500: internal server error (should not happen; if it does, it is a
    bug).
  - 501: not implemented (e.g., for corpus-query modes that are
    declared but not implemented).

---

## 4. Web UI

A server-rendered HTML interface that provides a human-friendly way to
browse the mirror and get recommendations. The UI is built with Go's
`html/template` package and served by the same binary as the API.

### 4.1 Design

- The UI follows the AO3 theme: dark blue header, white content area,
  sans-serif font.
- The UI is responsive: it works on desktop and mobile browsers.
- The UI is accessible: it uses semantic HTML and ARIA labels where
  appropriate.
- The UI is privacy-preserving: it does not use cookies, localStorage,
  or any client-side tracking.

### 4.2 Pages

- `/`: the home page, with a search bar and a list of recommended works
  (based on a default seed or the most recent works).
- `/search`: a page to search for works by title, summary, or tag.
- `/work/{id}`: a page to view a single work.
- `/tag/{id}`: a page to view a single tag.
- `/recommend`: a page to get recommendations from one or more seed works.
- `/fandoms`: a page to browse fandoms (by tag type `fandoms`).
- `/underrated`: a page to browse underrated works.
- `/neighbours`: a page to browse works that are neighbours of a given
  tag (by co-occurrence).
- `/surprise`: a page to get a surprise recommendation.
- `/profiles`: a page to list taste profiles.
- `/profile/{id}`: a page to view a single taste profile.
- `/arena`: a page to browse the arena leaderboard.
- `/leaderboard`: same as `/arena`.
- `/my-ranking`: a page to view the user's arena ranking.
- `/block`: a page to block a tag (add it to the blocked list).
- `/rank/{id}`: a page to view the ranking of a single work (in the arena).
- `/static/*`: static assets (CSS, images, etc.).

### 4.3 Components

- **Header**: appears on every page, with the site logo and navigation
  links.
- **Footer**: appears on every page, with the index age and version.
- **Work card**: a reusable component that displays a work's title,
  summary, stats, and tags.
- **Tag badge**: a reusable component that displays a tag's name with
  appropriate colouring by tag type.
- **Pagination**: a reusable component for paginated lists.
- **Search form**: a reusable component for searching works.

### 4.4 JavaScript

The UI uses minimal JavaScript, only for:
- Enhancing the search bar with autocomplete (via `/api/v1/ao3/tags?q=`).
- Enabling keyboard navigation (j/k to move through results, Enter to open).
- Making the "More like this" button work (a one-click link to
  `/api/v1/recommend?seed=ao3_work:{id}`).
- Toggling the visibility of blocked tags in the UI.

No JavaScript is required for the core functionality; the UI works
without it.

### 4.5 Templates

All HTML templates are in `internal/web/templates/` and are parsed at
startup. The templates are:
- `layout.html`: the base layout (header, footer, container).
- `home.html`: the home page.
- `search.html`: the search page.
- `work.html`: the work page.
- `tag.html`: the tag page.
- `recommend.html`: the recommend page.
- `fandoms.html`: the fandoms page.
- `underrated.html`: the underrated page.
- `neighbours.html`: the neighbours page.
- `surprise.html`: the surprise page.
- `profiles.html`: the profiles page.
- `profile.html`: the profile page.
- `arena.html`: the arena leaderboard page.
- `my-ranking.html`: the my-ranking page.
- `block.html`: the block page.
- `rank.html`: the rank page.
- `error.html`: the error page.
- `notfound.html`: the 404 page.

### 4.6 Assets

All static assets are in `internal/web/assets/` and are served under
`/static/`. The assets are:
- `style.css`: the main stylesheet.
- `work.html`: a fallback for the work page (used if the template is
  missing).
- `notfound.html`: a fallback for the 404 page.
- `error.html`: a fallback for the error page.
- `leaderboard.html`: a fallback for the arena leaderboard page.
- `myranking.html`: a fallback for the my-ranking page.
- `arena.html`: a fallback for the arena page.
- `block.html`: a fallback for the block page.
- `fandoms.html`: a fallback for the fandoms page.
- `underrated.html`: a fallback for the underrated page.
- `neighbours.html`: a fallback for the neighbours page.
- `surprise.html`: a fallback for the surprise page.
- `profiles.html`: a fallback for the profiles page.
- `profile.html`: a fallback for the profile page.

---

## 5. Commands

The `kindred` binary provides several subcommands for building and
maintaining the mirror and the index.

### 5.1 serve

Runs the HTTP API and web UI.

- Flags:
  - `--corpus`: path to the corpus SQLite database (required).
  - `--db`: path to the kindred SQLite database (for storing tunes,
    profiles, arena data, etc.; required).
  - `--listen`: address and port to listen on (default: `127.0.0.1:8010`).
  - `--mode`: `full` or `lite` (defaults to `full`).
    - In `lite` mode, the embeddings are not loaded into memory, saving
      ~80 MB of RAM.
  - `--tune`: name of the tune to use (default: `default`).
  - `--pool-size`: size of the recommendation pool (default: 200 for
    `lite`, 1000 for `full`).

### 5.2 ingest

Measures the corpus and builds the index in memory (does not persist the
index; it is lost when the command exits). Used to measure the memory
footprint and build time of the index.

- Flags:
  - `--corpus`: path to the corpus SQLite database (required).
  - `--db`: path to the kindred SQLite database (required; used to store
    the collaborative filtering index).
  - `--no-collab`: skip building the collaborative filtering index.

### 5.3 embed

Computes the tag embeddings from the co-occurrence graph and stores them
in the kindred SQLite database (in the `embeddings` table).

- Flags:
  - `--corpus`: path to the corpus SQLite database (required).
  - `--db`: path to the kindred SQLite database (required).
  - `--dim`: embedding dimension (default: 32).
  - `--no-normalize`: skip L2-normalizing the embeddings.

### 5.4 recommend

Recommends from seeds, from the shell (useful for testing and scripting).

- Flags:
  - `--seed`: namespaced entity ID (required, repeatable).
  - `--kind`: entity kind to recommend (optional).
  - `--n`: number of recommendations to return (default: 10).
  - `--pool-mode`: `tags` or `tags+collab` (default: `tags`).
  - `--exclude-tag`: tag ID to exclude (optional, repeatable).
  - `--min-words`: minimum word count (optional).
  - `--max-words`: maximum word count (optional).
  - `--min-kudos`: minimum kudos (optional).
  - `--complete`: `any`, `wip`, or `only` (optional).
  - `--rating`: AO3 rating to keep (optional, repeatable).
  - `--language`: language code to keep (optional, repeatable).
  - `--format`: `json` (default) or `csv`.
  - `--print-evidence`: print the evidence for each recommendation.

### 5.5 crawl

Fetches AO3 work pages into the mirror (the only networked subcommand).
Used to update the mirror with new works.

- Flags:
  - `--corpus`: path to the corpus SQLite database (required).
  - `--work-id`: AO3 work ID to fetch (required, repeatable).
  - `--delay`: delay between requests in seconds (default: 1.0).
  - `--timeout`: request timeout in seconds (default: 10.0).

### 5.6 profile

Builds, lists, shows, and rates taste profiles.

- Subcommands:
  - `list`: list all profiles.
  - `show`: show a single profile.
  - `build`: build a profile from seeds.
  - `rate`: fold a like/dislike into a profile.
  - `export`: export a profile as JSON.
  - `import`: import a profile from JSON.

### 5.7 rate

Alias for `profile rate`.

### 5.8 corpus-query

Asks a question about the corpus (fandom-ranking, underrated, tag-neighbours).

- Flags:
  - `--mode`: one of `fandom-ranking`, `underrated`, `tag-neighbours`,
    `surprise` (required).
  - `--tag`: tag ID to seed the analysis (optional, repeatable).
  - `--limit`: number of results to return (default: 10).
  - `--min-co-works`: minimum number of works that must co-occur with a
    tag for it to be considered (default: 1).
  - `--min-tags`: minimum number of tags a work must have to be
    considered (default: 0).

### 5.9 stats

Prints corpus and index statistics.

- Flags:
  - `--corpus`: path to the corpus SQLite database (optional; if not
    provided, uses the same database as the `serve` command).
  - `--db`: path to the kindred SQLite database (optional; if not
    provided, uses the same database as the `serve` command).

### 5.10 dump

Writes an anonymised, signed snapshot for peers.

- Flags:
  - `--dir`: directory to write the snapshot to (required).
  - `--passphrase`: passphrase to encrypt the snapshot (optional).

### 5.11 verify

Verifies a snapshot's signature and shard hashes.

- Flags:
  - `--dir`: directory containing the snapshot to verify (required).
  - `--passphrase`: passphrase to decrypt the snapshot (if encrypted).

### 5.12 fetch

Pulls a snapshot from a peer's onion service.

- Flags:
  - `--onion`: onion service address (required).
  - `--dir`: directory to write the snapshot to (required).
  - `--passphrase`: passphrase to decrypt the snapshot (if encrypted).

### 5.13 tune

Inspects or sets signal weights.

- Flags:
  - `--name`: name of the tune to inspect or set (required).
  - `--list`: list the weights of the tune.
  - `--set`: set the weights (comma-separated list of `signal=value`).
  - `--reset`: reset the tune to the default weights.

### 5.14 help

Prints the help message.

---

## 6. Memory

The design goal is to fit within the memory budget of a Raspberry Pi with
512 MB of RAM (leaving 512 MB for the OS and other services) and a
workstation with twenty other services resident.

The memory usage is dominated by the index (see §2). The measured peak
RSS for the `serve` command is 35 MB in full mode and 23 MB in lite mode.

The `ingest` and `embed` commands are separate because their peak RSS
(181.6 MB and 203.2 MB, respectively) would exceed the budget if run in
the same process as the `serve` command.

---

## 7. Security

- The binary is statically linked and does not use CGO, reducing the
  attack surface.
- The API is read-only; there are no write endpoints.
- The only networked subcommand is `crawl`, which only fetches from AO3
  (and only if given a work ID).
- The `dump`, `verify`, and `fetch` commands handle anonymised, signed
  snapshots and do not expose private data.
- The API does not set cookies or use localStorage, so there is no
  client-side state to steal.
- The API does not log query parameters or request bodies, so there is
  no risk of leaking sensitive data in logs.

---

## 8. Licence

AGPL-3.0. See [LICENSE](LICENSE).

---

## 9. Acknowledgments

- The AO3 corpus is provided by the Archive of Our Own, a project of the
  Organization for Transformative Works.
- The collaborative filtering algorithm is inspired by the Netflix Prize
  and the work of Yehuda Koren, Robert Bell, and Chris Volinsky.
- The embedding algorithm is inspired by the word2vec paper by Tomas
  Mikolov et al.
- The arena system is inspired by the Glicko-2 rating system by Mark
  Glickman.
- The web UI is inspired by the AO3 theme and the work of the AO3
  design team.

---

brainstorm 100 ideas to make this site as useful as possible. ranked by score (impact over effort)

# 100 Ideas for Kindred — Ranked by Impact/Effort

## Tier 1: Absurdly High Impact/Effort (do these first)

1. **"More Like This" one-click button on every work card** — already specced, just make it prominent and obvious. Zero backend work, massive discovery.

2. **Fandom landing pages** — `/fandom/{name}` showing top works, related fandoms, popular tags within that fandom. All data already indexed.

3. **"I've read this" button** that adds to SeenIDs and removes from future recommendations — the single most important feedback loop.

4. **URL-based seed input** — let users paste an AO3 URL (`archiveofourown.org/works/12345`) and auto-extract the work ID. Pure frontend parsing.

5. **Tag type colour coding in all UI surfaces** — fandom=red, relationship=green, character=blue, freeform=grey. Already have tag_type, just CSS.

6. **Sort recommendations by different criteria** — kudos, word count, update date, score. All fields already in memory, just reorder.

7. **Shareable recommendation URLs** — `/recommend?seed=ao3_work:123&seed=ao3_work:456` already works via query params, just add a "Copy link" button.

8. **Reading list export** — dump recommendations as a plain list of AO3 URLs for opening in tabs. One template, no backend.

9. **"Exclude fandom" filter** — users reading crossovers want recs without the fandom they already know. Already have `exclude_tag`, just surface it for fandom tags.

10. **Random work button** — pick a random high-quality work from the corpus. One `rand.Intn()` call with a kudos floor.

---

## Tier 2: High Impact/Effort

11. **Multi-seed blending UI** — drag/drop or checkbox multiple works to create a blended recommendation. Backend exists, frontend needs a "basket."

12. **Taste profile onboarding wizard** — show 10 popular works, ask thumbs up/down, build a profile. Guided version of the existing profile system.

13. **Tag autocomplete on all search/filter inputs** — already have the endpoint (`/api/v1/ao3/tags?q=`), just wire it to every input field.

14. **"Why this recommendation?" expandable section** — render the evidence array as human-readable sentences. Data already returned, just display it.

15. **Relationship graph visualisation** — show the tag co-occurrence neighbourhood as an interactive force-directed graph. Client-side D3/canvas, data from `/api/v1/tags/{id}/similar`.

16. **Infinite scroll on work lists** — replace pagination with lazy loading. Pure frontend, uses existing `offset` param.

17. **Keyboard shortcuts help modal** — j/k/Enter already work, add `?` to show the shortcuts. Trivial JS.

18. **"Works like X but longer/shorter" slider** — combine recommendation with word_count filter. Filter already exists, just add a range slider.

19. **RSS/Atom feed for new works in a fandom** — generate feed from works sorted by `first_seen`. Template + content-type header.

20. **Dark mode toggle** — CSS custom properties, one media query for system preference, one toggle button.

---

## Tier 3: Medium-High Impact/Effort

21. **Fandom crossover explorer** — given two fandoms, show works tagged with both. Single SQL query on work_tags already possible.

22. **"Surprise me" with constraints** — the surprise endpoint exists but add fandom/rating/length filters to make it actually useful.

23. **Tag cloud for a work's neighbourhood** — show the most common tags among a work's recommendations as a weighted cloud.

24. **Bookmark import from AO3** — let users paste their AO3 bookmarks page URL, crawl it, use as seeds. Extends `crawl`.

25. **"Trending in fandom" section** — works with recent `first_seen` and high kudos velocity (kudos / days_since_first_seen).

26. **Work completion status indicator** — prominent badge: ✓ Complete, ⟳ WIP, ✗ Abandoned (no update > 1 year). All data available.

27. **Per-fandom recommendation tuning** — different signal weights for different fandoms (romance fandoms weight relationship tags higher).

28. **CSV export of recommendations** — already specced in CLI `--format csv`, just add a download button in the web UI.

29. **Embed-based "vibe search"** — let users describe a vibe in tags and find works near that point in embedding space. Requires summing tag embeddings.

30. **Static site generation mode** — `kindred generate --out-dir ./site` that pre-renders the top N fandom pages as static HTML. For CDN deployment.

---

## Tier 4: Medium Impact/Effort

31. **Author page** — `/author/{name}` showing all works by that author, their most-used tags, recommended co-authors. Data in `works.authors`.

32. **"Readers also liked" section on work pages** — collaborative filtering already built, just surface it as a UI section.

33. **Reading time estimate** — `word_count / 250` displayed as "~X hours" on every work card. One line of template math.

34. **Tag hierarchy browser** — show parent/child tag relationships (if available from AO3's wrangling). Requires crawling tag pages.

35. **Work series support** — group works by series, recommend entire series instead of individual works. Requires adding series to the crawl.

36. **Multi-language UI** — i18n the templates. High effort but high impact for non-English AO3 users.

37. **"What's popular right now" dashboard** — works added in the last 30 days, sorted by kudos. Simple query on `first_seen`.

38. **Recommendation diff** — "How would your recommendations change if you added/removed this seed?" Show the delta.

39. **Tag synonym resolution** — "Draco Malfoy" and "Draco Lucius Malfoy" should be the same tag. AO3 has canonical tags; crawl and merge.

40. **Embeddable widget** — `<iframe>` or `<script>` snippet that other sites can use to show "works similar to X." Subset of the API.

---

## Tier 5: Medium Impact, Medium Effort

41. **Arena "rapid fire" mode** — show 5 pairs in sequence without page reloads, batch submit. Uses existing `/api/v1/arena/batch`.

42. **Seasonal/holiday recommendations** — tag certain tags as seasonal (Christmas, Halloween) and surface them during relevant periods.

43. **"Hidden gems" page** — works with <100 kudos but high recommendation scores from multiple seed angles. Query already possible.

44. **Tag co-occurrence heatmap** — for a set of tags, show which pairs co-occur most. Matrix visualisation from the CSR graph.

45. **OpenSearch descriptor** — let browsers add Kindred as a search engine. One XML file.

46. **Recommendation explanation as natural language** — instead of `{"signal":"tag_overlap","value":0.83}`, say "83% of this work's tags match your seeds."

47. **"Anti-recommendation"** — given seeds, find the *least* similar high-quality works. Invert the score, keep a kudos floor.

48. **Bulk work lookup** — paste a list of AO3 work IDs, get back all their data. Useful for researchers.

49. **Tag frequency timeline** — when was a tag most popular? Plot `first_seen` distribution of works with that tag.

50. **Progressive Web App manifest** — add a manifest.json and service worker for offline browsing of cached pages. Moderate JS effort.

---

## Tier 6: Moderate Impact/Effort

51. **Recommendation confidence indicator** — use the score spread to show "high confidence" vs "speculative" recommendations.

52. **User-contributed tag descriptions** — let users add descriptions to tags (stored in kindred.db). Low-risk write endpoint.

53. **"Fandom DNA" profile** — show a user's taste profile as a radar chart of tag categories (angst, fluff, AU, canon-compliant, etc.).

54. **Smart defaults for filters** — if a user always filters to English and Complete, remember that (URL params, no cookies).

55. **Related fandoms graph** — show which fandoms share the most tags/readers. Already computable from co-occurrence.

56. **Work age indicator** — "Published 3 years ago, last updated 2 months ago." Relative dates from `update_date` and `first_seen`.

57. **Recommendation seeding from tag list** — instead of "works like X," let users say "I want: hurt/comfort + enemies to lovers + >50k words."

58. **Print stylesheet** — CSS `@media print` for clean printouts of work pages and recommendation lists.

59. **API rate limiting with informative headers** — `X-RateLimit-Remaining`, `Retry-After`. Protects the Pi.

60. **Healthcheck dashboard** — `/status` page showing uptime, request count, index age, memory usage. For the operator.

---

## Tier 7: Lower Impact but Still Worthwhile

61. **"Complete your triad" recommender** — given two tags, recommend a third that commonly co-occurs with both but not individually.

62. **Work word count distribution histogram** — for a fandom or tag, show the distribution of work lengths. Client-side chart.

63. **Kudos/hits ratio as quality signal** — surface the ratio as a "reader satisfaction" metric. Simple division, already have both fields.

64. **Recommendation caching with ETag** — hash the seed+filter params, return 304 if unchanged. Saves bandwidth on the Pi.

65. **"Fandom of the day" feature** — rotate a featured fandom daily based on a deterministic hash of the date.

66. **OPML export of followed fandoms** — for RSS reader integration.

67. **Structured data (JSON-LD)** — add Schema.org `CreativeWork` markup to work pages for SEO. Template-only change.

68. **Microformats on work pages** — h-entry, h-card for IndieWeb compatibility.

69. **"Pairs well with" for fandoms** — given a fandom, show the fandoms whose readers most overlap. Co-bookmark data.

70. **Tag negation in search** — "hurt/comfort -character death" using `-` prefix. Parse in the query handler.

---

## Tier 8: Niche but High Value for Specific Users

71. **Calibre integration** — export recommendations as a Calibre-compatible catalogue for offline reading.

72. **AO3 skin/theme compatibility** — let users paste their AO3 skin CSS and apply it to Kindred.

73. **Wayback Machine fallback links** — if a work is deleted from AO3, link to the Wayback Machine capture.

74. **Work diff detection** — if a work is updated, show what changed (new chapters, revised tags). Requires storing snapshots.

75. **Podfic/translation cross-linking** — if the mirror has both a work and its podfic/translation, link them.

76. **"Write this" prompt generator** — find tag combinations that have high co-occurrence but no works yet. Gap analysis on the graph.

77. **Gift exchange matching** — given two users' profiles, find works both would enjoy. Profile intersection.

78. **Reading challenge tracker** — "Read 5 works from 5 different fandoms this month." Client-side state, no cookies (URL-encoded).

79. **Collection/anthology builder** — let users curate and share themed lists of works.

80. **Accessibility audit report page** — run automated a11y checks on the UI and show the results publicly.

---

## Tier 9: Infrastructure & Developer Experience

81. **Prometheus metrics endpoint** — `/metrics` with request latency, recommendation computation time, memory usage.

82. **Structured logging (JSON)** — replace `log.Printf` with structured logger for easier grep/jq analysis.

83. **Graceful shutdown with in-flight request draining** — `SIGTERM` handler that stops accepting new connections and waits for active ones.

84. **Config file support (TOML/YAML)** — alternative to command-line flags for complex setups.

85. **Systemd socket activation** — let systemd manage the listen socket for zero-downtime restarts.

86. **Automated corpus freshness check** — `kindred check-freshness` that reports how stale the mirror is vs. AO3.

87. **Index build progress bar** — show a progress bar during `ingest` and `embed` for long-running builds.

88. **Test fixture generator** — `kindred generate-fixtures --n 1000` that creates a small synthetic corpus for testing.

89. **Benchmark suite** — `go test -bench` for recommendation latency, embedding lookup, CSR traversal.

90. **Fuzz testing for API inputs** — `go test -fuzz` on query parameter parsing to catch panics.

---

## Tier 10: Speculative / Future Directions

91. **Federation protocol** — let multiple Kindred instances share anonymised co-occurrence data to improve recommendations without sharing corpus.

92. **Browser extension** — when viewing a work on AO3, show Kindred recommendations in a sidebar.

93. **Matrix/Discord bot** — `!recommend <url>` in a chat room, bot replies with top 5 recommendations.

94. **Tor hidden service mode** — run the entire API as a `.onion` service for privacy. Already have `fetch`/`dump` for peer exchange.

95. **Voice interface** — "Hey Kindred, recommend something like The Untamed but with more angst." Speech-to-text → tag extraction → recommend.

96. **E-reader direct download** — generate EPUB from AO3 work pages and send to Kindle/Kobo. Legal minefield but technically straightforward.

97. **Recommendation explanation as narrative** — LLM-generated paragraph explaining why a work was recommended, using the evidence array as input.

98. **Cross-platform corpus** — add fanfiction.net, Wattpad, or other archives as additional sources. The architecture is entity-kind-agnostic; this is the payoff.

99. **Academic research API** — anonymised, aggregated endpoints for fandom studies researchers (tag popularity over time, genre evolution, etc.).

100. **Collaborative playlist** — multiple users contribute seeds, get a shared recommendation list. Real-time via SSE or WebSocket.

---

## Top 10 Summary

| Rank | Idea | Impact | Effort | Score |
|------|------|--------|--------|-------|
| 1 | "I've read this" feedback button | 10 | 1 | 10.0 |
| 2 | URL-based seed input (paste AO3 link) | 9 | 1 | 9.0 |
| 3 | Tag colour coding by type | 8 | 1 | 8.0 |
| 4 | Fandom landing pages | 9 | 2 | 4.5 |
| 5 | Taste profile onboarding wizard | 10 | 3 | 3.3 |
| 6 | Sort recommendations by criteria | 8 | 1 | 8.0 |
| 7 | Shareable recommendation URLs | 7 | 1 | 7.0 |
| 8 | "Why this rec?" evidence display | 8 | 2 | 4.0 |
| 9 | Bookmark import from AO3 | 9 | 3 | 3.0 |
| 10 | Dark mode | 6 | 1 | 6.0 |

# Kindred — Frontend Specification

**Status:** draft 2026-09-30. Supersedes §4 of the main SPEC where they conflict.
**Scope:** everything a browser renders. Server-side templates, CSS, the small amount of JS, the UX contract.
**Non-scope:** API internals (see main SPEC §3), index construction (§2), binary deployment (§5).

---

## 0. Principles, in order of precedence

When these conflict, earlier ones win.

1. **The site works with JavaScript disabled.** Every core flow — search, recommend, filter, paginate, read a work's page, record arena judgments — resolves to a plain HTML form that POSTs or a plain `<a>` that GETs. JS is enhancement.
2. **The site works on a Raspberry Pi over a slow link.** First meaningful paint on 3G from a Pi backend should be under 2 seconds. No webfont download blocks render. No framework.
3. **The site tells no one anything about the reader.** No cookies, no `localStorage` cross-page, no third-party anything, no analytics, no referrer leakage to AO3. State lives in the URL.
4. **The site is honest about what it knows.** Every recommendation displays its evidence. Every stale datum displays its age. Every "we don't know" is said out loud instead of being hidden.
5. **The site is boring to look at.** AO3-adjacent, dense, textual, high-contrast. No animations beyond `:hover` and `:focus`. No skeleton loaders — if we can't paint real content, we show nothing.
6. **The reader is a reader, not a user.** Reading flows (open in new tab, "I've read this", word-count filters, completion status) take precedence over engagement flows (never: notifications, streaks, badges, infinite scroll with autoplay).

---

## 1. State model

There are three places state can live. Each has a precise purpose.

### 1.1 The URL

**All reader-visible state lives here.** Filters, seeds, sort order, pagination, blocked tags, selected profile — all query parameters. This is non-negotiable because:
- It makes every page shareable.
- It makes the back button work.
- It makes "open in new tab" work.
- It removes the need for cookies.

Canonical parameter names (shared across all pages that accept them):

| Param | Type | Meaning |
|---|---|---|
| `seed` | repeated string | Namespaced entity ID, e.g. `ao3_work:12345`. |
| `n` | int, default 20 | Items to return. |
| `offset` | int, default 0 | Pagination offset. |
| `sort` | enum | `score` (default for rec), `kudos`, `hits`, `words`, `updated`, `added`, `random`. |
| `min_words`, `max_words` | int | Word-count filter. |
| `min_kudos` | int | Kudos floor. |
| `complete` | enum | `any` (default), `wip`, `only`. |
| `rating` | repeated enum | `G`, `T`, `M`, `E`, `NR`. If omitted, all. |
| `language` | repeated string | ISO code. If omitted, all. |
| `blocked_tag` | repeated int | Tag IDs to exclude. |
| `exclude_fandom` | repeated int | Fandom tag IDs to exclude (sugar for `blocked_tag`). |
| `pool_mode` | enum | `tags`, `tags+collab`. Default: `tags`. |
| `profile` | string | Profile ID to apply (loaded from kindred.db). |
| `pretty` | int | Debug: pretty-print JSON. |

Repeated parameters use the standard `?rating=G&rating=T` form, not comma-separated.

### 1.2 The kindred.db (server-side)

State the reader explicitly commits:
- Taste profiles (named, exportable).
- Arena judgments (anonymous, per-session; see §1.3).
- Tunes (signal weights).
- "I've read this" lists (attached to a profile, not to a browser).

Nothing is written without an explicit action. There is no implicit tracking.

### 1.3 The session cookie — *only* for arena

The arena needs continuity of judgment across requests, and it is the single feature that cannot be done statelessly without burdening the URL with cryptographic baggage. The arena uses one cookie:

- Name: `kindred_arena`
- Value: 128-bit random session ID (no reader identity, no fingerprint).
- Scope: `Path=/arena; SameSite=Strict; HttpOnly; Secure` (when served over HTTPS).
- Lifetime: 90 days of inactivity.
- Function: links a sequence of pairwise judgments so the Glicko-2 ranking can accumulate.
- Reader control: a prominent "Reset arena identity" button on `/arena` destroys the cookie.

**No other cookie exists.** Not for preferences, not for CSRF, not for anything. Preferences live in the URL. CSRF is handled by the form token pattern (§6.4).

### 1.4 Local storage, session storage, IndexedDB

**Not used.** If a feature seems to require them, the feature is wrong.

---

## 2. Page inventory

Every URL the browser can land on. Grouped by purpose.

### 2.1 Discovery

| Path | Purpose |
|---|---|
| `/` | Home. Featured fandom, recent additions, a search box, a random work button. |
| `/search` | Full-text search across work titles, summaries, and tag names. |
| `/recommend` | The primary recommendation surface. Accepts seeds via query params. |
| `/surprise` | A single high-quality work far from any specified seeds. |
| `/underrated` | Works with high kudos-per-word but low total kudos. |
| `/neighbours` | Works that share tags with a seed's tag neighbours (two hops out). |
| `/random` | Server-side 302 to a random work above a kudos floor. |

### 2.2 Entity pages

| Path | Purpose |
|---|---|
| `/work/{id}` | Everything we know about one work, plus "more like this." |
| `/tag/{id}` | A tag's metadata, its most-common co-occurring tags, and its top works. |
| `/fandom/{name}` | A fandom's top works, trending works, related fandoms. (Sugar for `/tag/{id}` where the tag type is `fandoms`.) |
| `/author/{name}` | All works by one author, their most-used tags. |

### 2.3 Profiles

| Path | Purpose |
|---|---|
| `/profiles` | List profiles stored in kindred.db. |
| `/profile/{id}` | View one profile: seeds, signal weights, recent recommendations. |
| `/profile/{id}/onboard` | A 10-work thumbs-up/thumbs-down wizard to seed a new profile. |

### 2.4 Arena

| Path | Purpose |
|---|---|
| `/arena` | The pair-comparison UI. One pair at a time, with "rapid fire" option. |
| `/arena/leaderboard` | Top works by Glicko-2 rating. |
| `/arena/my-ranking` | The current session's cumulative judgments and derived ranking. |
| `/arena/rank/{id}` | A single work's Glicko-2 rating and judgment count. |

### 2.5 Operations and self-description

| Path | Purpose |
|---|---|
| `/about` | What this is, what it's not, how to read a recommendation. |
| `/status` | Index age, corpus size, memory usage. Operator-oriented. |
| `/healthz` | `{"status":"ok"}`. Not browsable but listed for completeness. |
| `/404` | The 404 page (served for unknown routes). |
| `/error` | The 500 page. |

### 2.6 Routes removed from the earlier SPEC

The following paths from main SPEC §4.2 are **dropped** in this revision:
- `/leaderboard` → use `/arena/leaderboard`.
- `/my-ranking` → use `/arena/my-ranking`.
- `/rank/{id}` → use `/arena/rank/{id}`.
- `/block` → folded into `/recommend` as a filter form. Blocking a tag is a per-query action, not a persistent setting.

---

## 3. Layout and components

### 3.1 The base layout (`layout.html`)

Every page extends this.

```
┌─────────────────────────────────────────────────────────┐
│  HEADER                                                 │
│  ┌─────────────────────────────────────────────────┐    │
│  │  KINDRED      Search  Recommend  Fandoms  Arena │    │
│  │  ·            ─────────────────────────────────  │    │
│  │  tagline      [ search input                 ] 🔍│    │
│  └─────────────────────────────────────────────────┘    │
├─────────────────────────────────────────────────────────┤
│  MAIN                                                   │
│  (page content; max-width 72ch for prose,               │
│   100% for lists)                                       │
├─────────────────────────────────────────────────────────┤
│  FOOTER                                                 │
│  index built 2h ago · 112,935 works · 634,231 tags      │
│  about · source code · AGPL-3.0                         │
└─────────────────────────────────────────────────────────┘
```

- The header is sticky on scroll (CSS `position: sticky; top: 0`), but is a plain translucent bar — no shadow, no transition.
- The tagline is a `<p>` element with class `tagline`, hidden on viewports under 600px.
- The search input in the header is a GET form targeting `/search`. It works without JS. Autocomplete (§5.1) enhances it.
- The footer prints the index age (from the `X-Kindred-Index-Age` header's source), corpus counts, and license.

### 3.2 The work card

The reusable rendering of one work. Appears in every list.

```
┌─────────────────────────────────────────────────────────┐
│  The Title Goes Here, And It Can Be Long                │
│  by someauthor · 42,000 words · ~2h50m read             │
│  ✓ Complete · English · Rated T · updated 2 days ago    │
│                                                         │
│  [red] Fandom A  [red] Fandom B                         │
│  [green] A/B  [blue] Character A  [blue] Character B    │
│  [grey] hurt/comfort  [grey] +12 more ▾                 │
│                                                         │
│  Summary text, truncated at roughly 500 characters,     │
│  ending at a word boundary with an ellipsis if cut.     │
│  …                                                      │
│                                                         │
│  ❤ 2,340  ★ 180  👁 12,500                              │
│                                                         │
│  [ Read on AO3 ↗ ] [ More like this ] [ ✓ Read it ]    │
│                                                         │
│  ▾ Why this? (expand for evidence)                      │
└─────────────────────────────────────────────────────────┘
```

Specifics:
- **Title** is a `<a>` to `/work/{id}`, not to AO3 directly. The reader can get to AO3 from the work page.
- **Author** is a `<a>` to `/author/{name}`.
- **Reading time** is `word_count / 250` rounded, formatted as `Xh Ym` or `Ym`. Omitted for works under 500 words.
- **Completion**: ✓ Complete, ⟳ WIP (updated within 1 year), ✗ Abandoned (not updated in >1 year *and* not complete). The X is grey, not red — we're not judging.
- **Tag chips** are coloured by tag type (§3.4). Fandoms always render first. If more than 8 tags, truncate with "+N more ▾" that expands via `<details>` (no JS).
- **Stats** use text-adjacent symbols, with `aria-label` for screen readers.
- **"More like this"** is a GET link to `/recommend?seed=ao3_work:{id}`.
- **"✓ Read it"** is a POST form to `/profile/{current_profile}/mark-read` with the work ID. If no profile is active, the button becomes "Save to profile…" and prompts to create or select one. Without JS this is a form POST; with JS it's an inline fetch that updates the button to "✓ Marked as read" without a page reload.
- **"Why this?"** is a `<details>` element containing the rendered evidence array (§3.3). Collapsed by default. No JS required.
- The whole card has `role="article"`.

### 3.3 Evidence rendering

The API returns:

```json
{"signal": "tag_overlap", "value": 0.83, "weight": 0.6, "reason": "..."}
```

Rendered as a `<dl>`:

```
Why this?
  Tag overlap        83%     ████████▍░
    Shares 15 of 18 seed tags, including enemies-to-lovers,
    hurt/comfort, and canon-divergence.
  Collaborative      0.42    ████▏░░░░░
    127 readers who liked your seeds also bookmarked this.
  Neighbourhood      0.31    ███░░░░░░░
    Shares tag neighbours with 3 of your seeds.
```

The bar is a CSS pseudo-element (`::after`) sized by the normalised score. No `<canvas>`, no SVG.

### 3.4 Tag chip component

```html
<a class="tag tag--fandom" href="/tag/123">The Untamed</a>
```

CSS classes:
- `tag--fandom` → red (`#8b2635` background, white text)
- `tag--relationship` → green (`#2d5f3f`)
- `tag--character` → blue (`#1f4e79`)
- `tag--freeform` → grey (`#555`)

Contrast ratios all ≥ 4.5:1 on their text. In dark mode, backgrounds are lightened and text is dark.

A tag chip is always a link to `/tag/{id}`. Secondary chips (e.g. "+ block") that need to be buttons are visually distinct (outlined, not filled).

### 3.5 Filter sidebar

Appears on `/recommend`, `/search`, `/fandom/{name}`, `/tag/{id}`. Collapses to a `<details>` on mobile.

```
┌────────────────────┐
│  Filters           │
│  ────────────────  │
│  Sort by           │
│  (•) Relevance     │
│  ( ) Kudos         │
│  ( ) Word count    │
│  ( ) Recently added│
│  ( ) Random        │
│                    │
│  Length            │
│  min [       ]     │
│  max [       ]     │
│                    │
│  Status            │
│  [x] Complete      │
│  [x] WIP           │
│                    │
│  Rating            │
│  [x] G [x] T       │
│  [x] M [ ] E       │
│                    │
│  Language          │
│  [x] English       │
│  [ ] +12 more ▾    │
│                    │
│  Blocked tags      │
│  × angst           │
│  × character death │
│  [ add tag…      ] │
│                    │
│  [ Apply filters ] │
│                    │
│  ─ ─ ─ ─ ─ ─ ─ ─   │
│  [ Reset ]         │
└────────────────────┘
```

- The entire sidebar is one `<form method="get">` targeting the current page.
- The "Apply filters" button is `<button type="submit">`.
- "Reset" is a plain `<a>` to the current path with no query string.
- The "add tag" input is enhanced with autocomplete (§5.1); without JS it's a plain text input that submits a search alongside the filters.
- Blocked tags appear as removable chips. Removal without JS is a link that reconstructs the URL without that `blocked_tag` param.

### 3.6 Pagination

```
← Previous   Page 3 of 47   Next →
```

- Previous/Next are GET links with `offset` adjusted.
- Page numbers are shown but not individually linkable past the first and last — we render "1 … 2 **3** 4 … 47".
- The current page is a `<span>`, not a link, with `aria-current="page"`.
- The URL always has `offset`; a missing `offset` means `0`.
- **No infinite scroll.** Pagination is the only mode.

### 3.7 Empty states

Every list page handles the empty case explicitly:

```
No works match these filters.

Try one of:
  → remove some blocked tags
  → raise the max word count
  → include WIPs
  → clear all filters

Or [ ask for a surprise ].
```

The "surprise" link preserves whatever filters make sense (rating, language).

### 3.8 Error states

| Code | Rendered as |
|---|---|
| 400 | `/error` with a plain-language description of which parameter was wrong, with a link back to where the reader came from. |
| 404 | `/404` with a search box and links to `/`, `/recommend`, `/fandoms`. |
| 500 | `/error` with "Something went wrong. The index may be rebuilding; try again in a minute." No stack traces to the browser. |
| 501 | Same as 404 but says "This feature isn't built yet." |

Errors never render inside a shell that implies success. If `/recommend` can't produce a pool, it renders the full error page, not a success page with "0 results."

---

## 4. Templates

Server-side rendered with Go's `html/template`. One file per page, all extending `layout.html`.

### 4.1 File list

```
internal/web/templates/
├── layout.html          ← header, footer, main wrapper
├── _work_card.html      ← partial: one work card (§3.2)
├── _tag_chip.html       ← partial: one tag chip
├── _filter_sidebar.html ← partial: filter form (§3.5)
├── _pagination.html     ← partial: pagination controls
├── _evidence.html       ← partial: "why this?" (§3.3)
├── home.html
├── search.html
├── recommend.html
├── surprise.html
├── underrated.html
├── neighbours.html
├── work.html
├── tag.html
├── fandom.html
├── author.html
├── profiles.html
├── profile.html
├── profile_onboard.html
├── arena.html
├── arena_leaderboard.html
├── arena_my_ranking.html
├── arena_rank.html
├── about.html
├── status.html
├── 404.html
└── error.html
```

Partials start with `_` and are only included via `{{template}}`.

### 4.2 Template data contract

Every page handler passes a struct with these common fields plus page-specific fields:

```go
type PageData struct {
    Title       string         // becomes <title>
    Canonical   string         // canonical URL for <link rel="canonical">
    IndexAge    time.Duration  // for footer
    IndexBuilt  time.Time      // for footer tooltip
    CorpusCounts map[string]int // works, tags, etc. for footer
    ActiveProfile *Profile     // nil if none selected
    // ... page-specific below ...
}
```

Templates never call methods that can fail. Any computation that could fail is done in the handler. Templates are pure rendering.

### 4.3 Template helpers (whitelist)

The functions available in templates. No others.

| Helper | Purpose |
|---|---|
| `fmt_int` | 42000 → "42,000" |
| `fmt_duration` | `2h50m` for reading time |
| `fmt_relative` | "2 days ago" from a `time.Time` |
| `fmt_rating` | `T` → "Teen And Up" |
| `truncate` | truncate to N chars at word boundary with ellipsis |
| `tag_class` | tag type → CSS class suffix |
| `url_with` | takes current URL, overrides/adds params, returns URL string |
| `url_without` | takes current URL, removes a param (optionally only one value), returns URL string |
| `pluralize` | `pluralize 1 "work" "works"` |
| `safe_summary` | AO3 summaries may contain HTML; sanitise to a small allowlist (b, i, em, strong, a) |

### 4.4 Fallback static templates

The `internal/web/assets/*.html` fallbacks mentioned in the main SPEC §4.6 are **removed**. If a template fails to parse at startup, the binary refuses to start. There is no runtime fallback path. This is simpler and safer than silently rendering a stale shell.

---

## 5. JavaScript

One file: `/static/kindred.js`. Max 5KB minified. No build step. Plain ES2020. No dependencies.

### 5.1 Tag autocomplete

Applied to any `<input data-autocomplete="tags">`.

- On input, debounced 150ms, fetches `/api/v1/ao3/tags?q={value}&per_page=10`.
- Renders results in a `<ul role="listbox">` positioned below the input.
- Arrow keys navigate, Enter selects, Esc closes.
- Selecting a result sets the input value and submits the parent form if `data-autocomplete-submit="true"`.
- Degrades gracefully: if JS fails or is disabled, the input is a plain text field that submits on Enter.

### 5.2 Keyboard navigation on list pages

Applied to pages with `data-keynav` on `<main>`.

- `j` / `↓`: move focus to next work card (adds `.focused`, scrolls into view).
- `k` / `↑`: previous.
- `Enter`: open the focused card's title link.
- `o`: open the AO3 link in a new tab.
- `m`: trigger "More like this" on the focused card.
- `r`: trigger "✓ Read it" on the focused card.
- `?`: show the keyboard shortcuts modal (a `<dialog>`).
- `/`: focus the header search input.

Focus state is CSS `:focus-visible` plus `.focused`. The modal is a `<dialog>` with `showModal()`, closed by Esc or clicking outside.

### 5.3 Inline "Read it" marking

The "✓ Read it" button on a work card is a `<form>` that normally POSTs. With JS, the form's `submit` handler is intercepted:
- `fetch()` the form's action with the form data.
- On success, replace the button content with "✓ Marked as read" and disable it.
- On failure, submit the form normally.

### 5.4 Rapid-fire arena mode

On `/arena`, a toggle "Rapid fire" switches the submit button to a client-side batcher:
- Each judgment is pushed into a client-side array.
- A new pair is fetched via `GET /api/v1/arena/pair`.
- Every 5 judgments or every 30 seconds, the array is POSTed to `/api/v1/arena/batch` and cleared.
- If the reader leaves the page, a `beforeunload` handler flushes the batch.

Without JS, each pair comparison is a plain form POST that returns a new pair.

### 5.5 What JavaScript must not do

- Fetch anything on page load without an explicit reader action.
- Rewrite the URL via `history.pushState` without a corresponding server-renderable state.
- Hide content that was present in the server render.
- Add tracking, analytics, telemetry, "performance monitoring", or any outbound request to a third party.
- Load any script from a third party.

---

## 6. HTML, HTTP, and transport details

### 6.1 Document skeleton

```html
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="color-scheme" content="light dark">
  <meta name="referrer" content="no-referrer">
  <title>{{.Title}} — Kindred</title>
  <link rel="canonical" href="{{.Canonical}}">
  <link rel="stylesheet" href="/static/kindred.css">
  <link rel="icon" type="image/svg+xml" href="/static/favicon.svg">
  <link rel="alternate" type="application/atom+xml"
        title="New works" href="/feed/new">
</head>
<body>
  <a class="skip-link" href="#main">Skip to content</a>
  {{template "header" .}}
  <main id="main">{{template "content" .}}</main>
  {{template "footer" .}}
  <script src="/static/kindred.js" defer></script>
</body>
</html>
```

- `<meta name="referrer" content="no-referrer">` prevents AO3 from seeing where its traffic came from.
- The script is deferred, non-essential.
- No preconnect, no prefetch, no DNS prefetch to any third party.

### 6.2 Response headers

Every HTML response includes:

| Header | Value |
|---|---|
| `Server` | `kindred` |
| `X-Kindred-Index-Age` | duration since last index build, e.g. `2h34m` |
| `Content-Security-Policy` | `default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; form-action 'self'; frame-ancestors 'none'; base-uri 'self'` |
| `X-Content-Type-Options` | `nosniff` |
| `Referrer-Policy` | `no-referrer` |
| `Permissions-Policy` | `interest-cohort=(), geolocation=(), camera=(), microphone=()` |
| `Strict-Transport-Security` | `max-age=63072000` (when served over HTTPS) |
| `Cache-Control` | varies (see §6.3) |

CSP has no `unsafe-inline`. All styles are in `kindred.css`. All scripts are in `kindred.js`.

### 6.3 Caching

| Resource | `Cache-Control` |
|---|---|
| `/static/*` | `public, max-age=31536000, immutable` (filenames contain a content hash) |
| HTML pages (list and detail) | `private, max-age=60` |
| HTML pages (session-dependent, e.g. arena, profile) | `private, no-cache` |
| `/api/v1/*` JSON | `private, max-age=60` with `ETag` based on index version |
| `/healthz`, `/status` | `no-store` |

The index version is a monotonic integer, incremented on each successful `ingest`. ETags include this plus a hash of request params. `If-None-Match` is honoured.

### 6.4 Forms and CSRF

Write operations (there are few: `/profile/*/mark-read`, `/arena/compare`, `/arena/batch`, profile create/edit) require:
- Method `POST`.
- `Origin` header matching the server's origin, OR a form token.
- Form token: a hidden `<input name="_token">` whose value is HMAC-SHA256 of the arena session ID (or a per-request nonce for unauthenticated flows) with a server-side secret.

Rejected submissions return 403 with a page saying "This form expired; go back and try again."

### 6.5 Robots and discoverability

`/robots.txt`:
```
User-agent: *
Allow: /
Disallow: /arena
Disallow: /profile
Disallow: /api/
Crawl-delay: 10
```

A sitemap at `/sitemap.xml` lists the top 10,000 works (by kudos) and the top 1,000 tags. Regenerated during `ingest`.

---

## 7. CSS

### 7.1 File

One file: `/static/kindred.css`. Max 20KB uncompressed. No preprocessor.

### 7.2 Variables (both light and dark themes)

```css
:root {
  --fg: #1a1a1a;
  --bg: #fafafa;
  --bg-alt: #f0f0f0;
  --border: #d4d4d4;
  --accent: #4a1a2c;     /* AO3-ish dark red */
  --accent-fg: #fff;
  --muted: #666;
  --link: #0b4b8c;
  --link-visited: #5e2a7e;

  --tag-fandom-bg: #8b2635;
  --tag-rel-bg: #2d5f3f;
  --tag-char-bg: #1f4e79;
  --tag-free-bg: #555;

  --font-body: ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif;
  --font-mono: ui-monospace, "SF Mono", Menlo, Consolas, monospace;
  --fs-base: 16px;
  --lh-base: 1.5;
  --measure: 72ch;
  --space: 0.5rem;
}

@media (prefers-color-scheme: dark) {
  :root {
    --fg: #e8e8e8;
    --bg: #121212;
    --bg-alt: #1e1e1e;
    --border: #2e2e2e;
    --muted: #999;
    --link: #7cb0e8;
    --link-visited: #c0a0e0;
    /* tag colours lightened */
    --tag-fandom-bg: #c05566;
    --tag-rel-bg: #5fa275;
    --tag-char-bg: #4a90d9;
    --tag-free-bg: #999;
  }
}
```

### 7.3 Rules

- Mobile-first. Media query breakpoints at `600px`, `900px`, `1200px`.
- Font size never below 16px (iOS Safari zoom trigger).
- No `transition` longer than 100ms. No `animation` at all except for the keyboard-shortcuts modal fade (100ms).
- `prefers-reduced-motion: reduce` disables all transitions.
- Focus outlines are always visible on `:focus-visible`, never removed.
- Print styles (`@media print`) strip the header, footer, and sidebar; render work cards as plain blocks.

### 7.4 Theme switching

The `color-scheme` meta tag plus CSS media queries do all theme work. No toggle button. The reader controls this at the OS level. If a toggle is later added, it must be an opt-in override via a URL parameter (`?theme=dark`), not a cookie.

---

## 8. Accessibility

Non-negotiable requirements:

1. All interactive elements are reachable by `Tab` in document order.
2. All form controls have associated `<label>` elements.
3. All images have `alt` text (empty `alt=""` for decorative, descriptive otherwise). Kindred has almost no images.
4. All icons have `aria-label` or `aria-hidden="true"` + accompanying text.
5. Colour is never the only signal (completion status uses glyphs *and* text; tag types have different text patterns in addition to colour).
6. Contrast ratio ≥ 4.5:1 for all text, 3:1 for large text.
7. The site is navigable with a screen reader (tested with Orca and VoiceOver).
8. `<main>`, `<nav>`, `<header>`, `<footer>`, `<article>`, `<aside>` are used semantically.
9. Headings are hierarchical: one `<h1>` per page, no skipped levels.
10. The skip link (`<a class="skip-link" href="#main">`) is the first focusable element and visible on focus.
11. Modals use `<dialog>` with `showModal()`, trap focus, and are dismissible with Esc.
12. Form errors use `aria-invalid` and `aria-describedby` pointing at the error message.

A `/a11y` page documents the accessibility stance and known gaps.

---

## 9. Performance budget

Measured on a Raspberry Pi 4 with 512 MB RAM, over a throttled 3G connection (750 kb/s, 300ms RTT), to a cold cache.

| Metric | Budget |
|---|---|
| Time to first byte (`/recommend` with 1 seed) | ≤ 300ms |
| First contentful paint | ≤ 1.0s |
| Largest contentful paint | ≤ 2.0s |
| Total page weight (home) | ≤ 40KB gzipped |
| Total page weight (recommend, 20 results) | ≤ 80KB gzipped |
| JavaScript bytes | ≤ 5KB minified |
| CSS bytes | ≤ 20KB uncompressed, served gzipped |
| No layout shift (CLS) | 0 |
| No long tasks over 50ms | 0 |

The budget is enforced in CI: a Playwright script loads each page type against a local Pi-emulated server and asserts the metrics.

---

## 10. Progressive enhancement contract

For each JS-enhanced feature, there is a no-JS fallback. Enumerated:

| Feature | With JS | Without JS |
|---|---|---|
| Tag autocomplete | Dropdown list, arrow keys | Plain text input; submit to `/search?q=…` |
| Keyboard navigation | `j`/`k`/`Enter`/`o`/`m`/`r`/`?` | Browser's native focus + `Tab`/`Enter` |
| "Read it" button | Inline fetch, button updates | Form POST, full page reload with flash |
| Rapid-fire arena | Batched POST every 5 or 30s | One POST per judgment, full page reload |
| Why-this expansion | `<details>` toggles via native | `<details>` toggles via native |
| Shortcut modal | `<dialog>` with `showModal()` | `/help/shortcuts` page |

Features that cannot degrade (there should be none) are prohibited.

---

## 11. Content guidelines

Rules for the prose rendered on the site itself (not reader content).

- Use British English spelling in UI chrome. (`colour`, `favourite`, `organisation`.)
- Prefer verbs over nouns in buttons. ("Recommend" not "Recommendations.")
- Error messages describe what the reader can do, not what the server failed at. Bad: "500 internal server error." Good: "The index is rebuilding; please try again in about a minute."
- Reading recommendations are described neutrally. We never say "You'll love this." We say "Shares 15 of 18 tags with your seed."
- Numeric claims always include their basis. "127 readers who liked your seeds also bookmarked this," not "127 similar readers."
- Dates are relative up to 30 days, then absolute. "2 days ago," "last week," "27 August 2026."
- Tag names are rendered verbatim from AO3. We do not sanitise, re-case, or re-punctuate them. They are the reader's language.

---

## 12. Testing

### 12.1 Required test layers

1. **Template compilation**: `go test` asserts every template parses.
2. **Handler unit tests**: every page handler is tested with representative query params and asserted on status + presence of required strings.
3. **Golden file tests**: for `/`, `/recommend`, `/work/{id}`, `/tag/{id}`, `/404`, `/error`, the full HTML output for fixed input is compared against a committed golden file.
4. **Accessibility tests**: `axe-core` run via Playwright against each page type. Zero violations required.
5. **No-JS tests**: Playwright with JavaScript disabled; every page type must render and every primary action must complete.
6. **Keyboard-only tests**: Playwright script that navigates and performs each primary action using only `Tab`, arrow keys, and `Enter`.
7. **Performance budget tests** (§9): Playwright asserts page weight and timing.
8. **CSP tests**: assert no inline styles or scripts are generated by any handler.

### 12.2 Fixtures

A `testdata/corpus.sqlite` with 1,000 works, 5,000 tags, 50,000 cooccurrence edges. Committed to the repo (≤ 2MB). Deterministic; generated by `kindred generate-fixtures --seed 42`.

---

## 13. Open questions

Explicitly deferred, not forgotten:

1. **Per-reader signal weights without accounts.** Currently tunes are server-side and global. A reader who wants to upweight "collaborative" over "tag_overlap" has to encode it in the URL (`&w_collab=0.8&w_tags=0.4`). Is this good enough, or do we want ephemeral per-session tunes?
2. **i18n of the chrome.** The templates are English-only. A `lang` query param plus a message catalogue would be ~2 weeks of work. Worth it only once we have non-English readers to serve.
3. **Serving over Tor hidden service.** The CSP and no-referrer policy already make this safe; the open question is whether the index-age header or any other response field leaks timing information.
4. **The "I've read this" button without a profile.** Currently prompts the reader to make one. An alternative is a URL-encoded blob of read work IDs — but this grows unbounded and leaks into logs.
5. **Autocomplete over fandoms vs over all tags.** Current spec says all tags. Fandom-only autocomplete would be much faster and probably what readers mostly want. Consider two autocomplete modes.

---

## 14. Changes from main SPEC §4

For reviewers who read the main SPEC first.

| Change | Rationale |
|---|---|
| Dropped `/leaderboard`, `/my-ranking`, `/rank/{id}`, `/block` as top-level routes. | They were duplicates or belonged inside `/arena` or `/recommend`. |
| Added `/fandom/{name}`, `/author/{name}`, `/about`, `/status`, `/404`, `/error`. | Previously implied but not named. |
| Removed the static HTML fallbacks in `internal/web/assets/*.html`. | Silently-stale fallbacks are worse than refusing to start. |
| Added one cookie (`kindred_arena`) scoped to `/arena`. | Arena cannot be stateless without encoding Glicko-2 state in the URL. The main SPEC §4.1 said "no cookies"; this is the one principled exception. |
| Specified CSP, Referrer-Policy, Permissions-Policy. | The main SPEC implied these but did not name them. |
| Specified a 5KB JS budget and enumerated what JS is for. | The main SPEC said "minimal JavaScript" without defining minimal. |
| Added a performance budget enforced in CI. | The main SPEC claimed Pi-friendliness without defining it. |

To make the system prioritize **what you and similar readers enjoy** rather than what is globally popular across the entire site, we need to modify the recommendation engine’s math and the index design. 

By default, standard recommenders fall into the "popularity trap," where massive global fics (the ones with 50,000 kudos) dominate because they have co-occurrences with almost everything. 

To achieve true personalized taste-clustering, we will update the **Kindred Spec** with three specific architectural adjustments:

---

### 1. The Core Algorithmic Shift: De-Biased Collaborative Filtering
Instead of using raw co-bookmark counts (which biases the system toward massive fics), the collaborative filtering pool will use **normalized cosine similarity of reader overlaps** (resembling a personalized TF-IDF for bookmarks).

If you have a seed fic $A$, and the engine is evaluating candidate fic $B$:
* **Old Way:** Score $B$ based on the raw count of users who bookmarked both $A$ and $B$. (Big fics win by default because they have thousands of bookmarks).
* **New Way:** 
  $$\text{Score}(A, B) = \frac{\text{Users who bookmarked both } A \text{ and } B}{\sqrt{\text{Total bookmarks for } A \times \text{Total bookmarks for } B}}$$

**Why this works:** If a massive, globally popular fic $B$ shares 50 bookmarks with your niche seed $A$, but $B$ has 10,000 bookmarks in total, its score is heavily penalized. If a niche fic $C$ shares 40 bookmarks with your seed $A$, and only has 60 total bookmarks, its score sky-rockets. You get matched with the exact pocket of readers who share your hyperspecific taste.

---

### 2. Spec Changes to `internal/engine/engine.go` (The Index)

To implement this, we update the in-memory Collaborative Filtering index to store normalized similarity vectors rather than raw co-occurrence weights.

```go
// internal/engine/engine.go

type CFEdge struct {
    TargetWorkID uint32
    // Similarity is the cosine similarity of reader bookmarks, 
    // penalizing globally ubiquitous works.
    Similarity   float32 
}

// Instead of a global popularity pool, candidates are scored 
// strictly against the user's active Profile or Seed Set.
func (e *Engine) Recommend(seeds []string, weights Tune) []Recommendation {
    candidates := make(map[uint32]*RawScore)
    
    for _, seed := range seeds {
        // 1. Get embedding-based similar tags (your "vibe" cluster)
        // 2. Get cosine-normalized collaborative neighbours (your "reader" cluster)
        neighbors := e.CFIndex.GetNeighbors(seed)
        for _, neighbor := range neighbors {
            candidates[neighbor.TargetWorkID].CollaborativeScore += neighbor.Similarity
        }
    }
    
    // 3. Apply the "Popularity Dampening" exponent to the final ranking:
    // FinalScore = PersonalizedSignals / (GlobalKudos ^ KudosDampeningFactor)
    // where KudosDampeningFactor is a tunable value (default 0.4) in your profile.
}
```

---

### 3. Spec Changes to the Web UI (Frontend Spec §3)

We will change the default sorting behavior and visual indicators on the frontend to reflect this personalized focus.

* **"Taste Match" is the Default:** The default sort on `/recommend` and `/profile/{id}` is renamed from `Relevance` to **`Taste Match`**. It represents your personalized similarity score.
* **The "Popularity Penalty" Slider:** In the Filter Sidebar, we add a slider: 
  * `[ Niche Gems ───•─── Global Hits ]`
  * Moving it toward "Niche Gems" increases the value of the popularity dampener exponent, completely hiding globally trending works in favor of high-similarity, low-kudos fics.
* **"Why This?" Evidence Update:** The collaborative evidence string will explicitly state the taste-cluster alignment:
  * *Old:* "127 readers who liked your seeds also bookmarked this."
  * *New:* "Highly aligned with your reader cohort (94% taste overlap, 12 bookmarks)."

---

### Updated Discord Blurb (Reflecting this)

> **AO3 recommender**
> https://kindred.polarisocial.xyz
> 
> Paste a fic you loved and get highly personalized recommendations based on what *readers with your exact taste* bookmarked, rather than just what's globally popular. Features smart search filters, tag similarity matching, and a built-in pairwise comparison arena to help find hidden, underrated gems. No accounts, no tracking, works completely without JS.

## Self-Serve User Interactions (No Mod Required)

These fall into three tiers based on how much trust they require.

---

### Tier 1: Zero Trust Needed — Already in Spec

These affect only the individual reader. No moderation surface exists.

| Feature | How it works |
|---|---|
| **Arena judgments** | Pick winner/loser from a pair. Anonymous, cookie-scoped. Glicko-2 absorbs noise naturally — no mod needed because one bad actor can't move a rating much. |
| **Taste profile creation** | Thumbs-up/down 10 works to seed a profile. Stored in kindred.db under a user-chosen name. |
| **"I've read this" marking** | Attached to a profile. Removes works from future recs for that profile only. |
| **Tag blocking** | URL parameter. No server state. The reader controls their own filter. |
| **Signal weight tuning** | Per-profile sliders (e.g. "weight collaborative more, weight tags less"). Affects only that profile's recs. |
| **Profile export/import** | JSON blob. Readers share profiles out-of-band. No server-side moderation. |
| **Filtering & sorting** | All URL params. Entirely client-side from the server's perspective. |

---

### Tier 2: Low Trust — Safe by Design

These write to shared state but are structurally resistant to abuse.

| Feature | Why it doesn't need mods |
|---|---|
| **Crowdsourced "vibe" tags** | Readers add freeform vibe labels to works (e.g. "cozy," "angsty," "slow burn"). Stored as reader-attached annotations, not merged into the AO3 tag graph. Each reader sees a weighted blend of their cohort's vibes. Abuse is self-limiting because a spam tag from one reader doesn't appear for others unless their taste profiles overlap. |
| **Recommendation thumbs up/down** | After getting a rec, the reader marks it good/bad. This feeds back into their profile's signal weights automatically. No global effect — it's per-profile reinforcement learning. |
| **Shared recommendation lists** | A reader curates a list of work IDs with a title and description, gets a shareable URL (`/list/abc123`). Lists are append-only and tied to the creator's profile. No editing by others. The only moderation risk is the description text, which can be length-limited (200 chars) and auto-filtered for obvious spam patterns. |
| **"Request crawl" queue** | A reader pastes an AO3 URL they want indexed. The work ID goes into a FIFO queue that `kindred crawl` drains. Rate-limited per IP (e.g. 5/day). No mod needed because the worst case is someone queues a work that already exists (idempotent). |
| **Work status flags** | Readers flag a work as "deleted from AO3," "abandoned," or "series-complete." Flags are aggregated — a work only gets marked when N independent readers agree (e.g. 3). A single bad flag does nothing. |
| **Tag synonym suggestions** | A reader suggests "Draco Malfoy" = "Draco Lucius Malfoy." Suggestions are merged only when they reach a threshold of independent submissions from readers with uncorrelated profiles. Below threshold, they're invisible. |

---

### Tier 3: Medium Trust — Self-Correcting

These affect shared state more visibly but have built-in correction mechanisms.

| Feature | Self-correction mechanism |
|---|---|
| **Community content warnings** | Readers add CWs beyond AO3's official tags (e.g. "graphic medical trauma," "unreliable narrator"). Displayed with a confidence score based on how many independent readers added it. A single reader's CW shows as "1 reader flagged." At 5+, it shows as a standard warning. No mod needed because the threshold prevents noise, and readers can dismiss CWs they disagree with (which downweights the flag for their cohort). |
| **Quality annotations** | Short freeform notes on a work ("great worldbuilding, weak ending," "POV switches mid-chapter"). Attached to the reader's profile, visible to readers with similar taste. Functions like a lightweight review system but scoped to taste cohorts rather than global. Abuse is contained because a reader's annotations only surface to their cluster. |
| **Arena challenge/dispute** | If a reader thinks the arena paired two incomparable works (e.g. a 500-word drabble vs. a 200k epic), they can flag the pair as "not comparable." Flagged pairs are excluded from Glicko-2 updates. If a work accumulates many "not comparable" flags, it gets moved to a separate weight class. Self-correcting because the system adapts. |
| **Fandom boundary suggestions** | Readers suggest that two fandom tags should be merged or split (e.g. "MCU" vs. "Marvel Cinematic Universe"). Suggestions are aggregated and applied when consensus is reached across diverse profiles. |

---

### What Deliberately Requires Mods (for contrast)

| Feature | Why |
|---|---|
| **Corpus ingestion from new sources** | Adding fanfiction.net or Wattpad changes the entire index. Operator decision. |
| **Global signal weight defaults** | The default tune affects all readers. Should be set deliberately, not crowd-sourced. |
| **Tag graph surgery** | Merging or splitting tags in the co-occurrence graph changes recommendations for everyone. |
| **Banning/blocking readers** | If someone is systematically gaming the arena, that's a mod action. |
| **Snapshot publishing** | `kindred dump` signs data with the operator's key. Trust decision. |

---

### Summary for the Discord

> readers can: compare fics in the arena, build taste profiles, mark fics as read, block tags, tune their own rec weights, request new works be crawled, add vibe labels and content warnings (scoped to their taste cluster), and share curated lists — all without accounts and without mods. the system is designed so individual bad inputs wash out statistically.

## Algorithms for "Similar to X that U Would Enjoy" — Ranked Best to Worst

Ranked for your specific constraints: Go, single binary, ~220 MB RAM, 267 rated works, 44 bookmarks, 7.7M-edge tag graph, <300ms query budget.

---

### 1. Profile-Weighted Tag Overlap ⭐ best fit

**Idea:** Don't compute similarity and enjoyment separately. Warp the similarity space by U's taste.

**Math:** `score(C) = Σ_t min(X_t, C_t) · U_t` where `X_t`, `C_t` ∈ {0,1} are tag indicators and `U_t` is U's profile weight for tag `t` (positive for liked tags, negative for disliked, zero for unknown).

**Why it wins:** A candidate that shares 5 tags with X gets a high score *only if U actually likes those tags*. A candidate sharing 15 tags with X but all in genres U hates scores near zero. The interaction is modeled directly, not bolted on.

**Implementation cost:** Low. You already have `TagOverlap` (Jaccard) and `Profile.Tags map[int32]float64`. Replace the unweighted intersection count with a weighted dot product. One new signal, ~50 lines.

**Caveat:** Sparse profiles (few rated works in a given fandom) produce noisy weights. Smooth with a small prior: `U_t = (Σ ratings for tag t + μ) / (count + 1)` where μ is the global mean rating.

---

### 2. Calibrated Linear Blend (your α=0.7)

**Idea:** Compute `sim(X, C)` and `enjoy(U, C)` independently, normalize each to [0,1] *over the candidate pool*, then `score = 0.7·sim + 0.3·enjoy`.

**Why it's second:** It works and it's what you asked for. But it models the two signals independently — a candidate that's very similar to X *in dimensions U hates* still gets 0.7 of the sim score. The interaction is lost.

**Critical implementation detail:** You **must** normalize per-pool. Your current signal magnitudes are wildly different (neighbourhood ~180, quality ~0.8, collab ~0.3). If you don't normalize, α=0.7 is meaningless because neighbourhood alone dominates the entire score. Use rank-normalization: `norm(s) = rank(s) / pool_size`. This is distribution-free and robust to outliers.

**Implementation cost:** Medium. Add a normalization pass in `rank.Score` after signal evaluation, before the weighted sum. ~80 lines.

---

### 3. CF-Filtered Content Ranking

**Idea:** Use collaborative filtering to find U's reader neighborhood (readers who bookmarked the same works as U), restrict the candidate pool to works those neighbors bookmarked, then rank within that pool by content similarity to X.

**Why it's good:** The CF filter eliminates the popularity trap entirely — globally popular fics that U's neighbors never touched don't even enter the pool. Content similarity then does the fine-grained ranking.

**Why it's third:** U has 44 bookmarks across 6,261 readers. The neighborhood will be small (~50–200 overlapping readers) and the filtered pool may be too narrow for niche seeds. Works best for mainstream fandoms, degrades for obscure ones.

**Implementation cost:** Medium. The `collab` signal already builds the item-item matrix. Add a user-neighborhood lookup (`user_work_interactions` → reader IDs → their bookmarks) and use it as a pool filter in `engine.Recommend`. ~120 lines.

---

### 4. PMI-Weighted Profile Overlap

**Idea:** Like #1, but weight each tag match by its PMI (pointwise mutual information) from the co-occurrence graph. A rare shared tag that U loves is worth more than a common one.

**Math:** `score(C) = Σ_t min(X_t, C_t) · U_t · PMI(t, X)` where `PMI(t, X) = log P(t ∩ X_tags) / (P(t) · P(X_tags))`.

**Why it's good:** Fixes the "Angst problem" — the tag "Angst" appears on 40% of all fics, so sharing it means almost nothing. PMI downweights ubiquitous tags and upweights distinctive ones. Combined with U's profile weights, you get "distinctive tags that U loves and X has."

**Why it's fourth:** PMI values can be negative (anti-correlated tags) and need clipping. The existing `neighbourhood` signal already uses PMI internally, so you'd be duplicating logic. Better to refactor `neighbourhood` to accept a profile weight vector than to build a parallel signal.

**Implementation cost:** Medium-high. Requires precomputing per-tag PMI against the seed's tag set at query time, or caching PMI vectors for common seeds. ~150 lines.

---

### 5. Embedding Subspace Projection

**Idea:** Project the 32-dim tag embeddings into the subspace most activated by U's high-rated tags, then measure cosine distance between X and candidates in that subspace.

**Math:** Let `M` be a 32×32 diagonal mask where `M_ii = mean embedding activation for U's liked tags on dimension i`. Score = `cosine(M·emb(X), M·emb(C))`.

**Why it's good:** Captures latent taste dimensions that explicit tags miss. Two fics might share no tags but occupy the same region of embedding space (e.g., both are "competence porn SI" even if tagged differently).

**Why it's fifth:** Requires `--mode full` (the 80 MB embedding matrix). The 32-dim SVD embeddings are already lossy; projecting further into a user subspace amplifies noise. Works best when U has 100+ rated works in diverse fandoms; with 267 it's borderline.

**Implementation cost:** Low-medium. The embeddings are already in memory in full mode. The mask computation is a one-time per-profile operation. ~60 lines.

---

### 6. Item-Item CF Re-Ranked by Profile

**Idea:** Reverse of #3. Start with X's collaborative neighbors (works co-bookmarked with X by *any* reader), then re-rank by U's tag profile overlap.

**Why it's worse than #3:** The initial pool is biased toward X's global popularity, not U's taste. A mega-fic with 5,000 bookmarks will dominate the CF neighbors regardless of U's preferences. The profile re-ranking helps but can't fully undo the pool bias.

**Implementation cost:** Low. The `collab` signal already does this. Just add the profile score as a re-ranking weight. ~30 lines.

---

### 7. Personalized PageRank on the Tag Graph

**Idea:** Run a random walk on the 7.7M-edge co-occurrence graph starting from X's tags, with restart probability biased toward U's high-rated tags. The stationary distribution gives personalized tag relevance scores.

**Why it's theoretically great:** Captures multi-hop relationships (X has tag A, A co-occurs with B, B co-occurs with C, U loves C → boost candidates with C). The gold standard for graph-based personalization.

**Why it's impractical:** O(edges) per query on a 7.7M-edge graph. Even with approximate PPR (top-K pruning), you're looking at 50–100ms per query minimum, and the implementation complexity is high. Precomputing per-user PPR vectors would require 634K × 267 ratings × 4 bytes ≈ 680 MB — over budget.

**Implementation cost:** High. ~500 lines + significant testing. Not recommended for v1.

---

### 8. Bayesian Personalized Ranking (BPR)

**Idea:** Treat U's 8–10 rated works as positive and 1–4 rated works as negative. Learn a latent factor model via SGD that predicts U's pairwise preference. Re-rank X-similar candidates by predicted preference.

**Why it's worse:** 267 ratings is enough to train a simple model but not enough for BPR to outperform the profile-weighted overlap (#1). BPR shines at 10K+ interactions. The training loop is awkward in a single-binary Go setup (no autograd, manual SGD).

**Implementation cost:** High. ~400 lines for the factorization, plus serialization and retraining logic.

---

### 9. Learning to Rank (LambdaMART / GBDT)

**Idea:** Train a gradient-boosted decision tree on U's rated works with features: tag overlap, embedding distance, kudos, word count, completion, collab score, etc. Predict U's rating for each candidate.

**Why it's worse than the above:** With 267 labeled examples and ~8 features, a GBDT will overfit badly. Cross-validation would show near-random performance on held-out ratings. Also requires a training infrastructure and model format (ONNX? custom?) that doesn't fit the single-binary philosophy.

**Implementation cost:** Very high. External dependency or custom GBDT implementation.

---

### 10. Matrix Factorization (ALS) on the Full User-Work Matrix

**Idea:** Factor the 6,261 × 112,935 interaction matrix into latent factors. Predict U's rating for all candidates, filter by X-similarity.

**Why it's worse:** The matrix is 0.026% dense. ALS on this will produce noisy factors. The existing SVD tag embeddings already capture the dominant latent structure in 80 MB. Adding a user-factor matrix would double the memory for marginal gain.

**Implementation cost:** High. ~300 lines + precomputation step + 100+ MB additional memory.

---

### 11–17: Don't Bother

| Rank | Algorithm | Why Not |
|---|---|---|
| 11 | Graph neural network (GNN) | Requires PyTorch, GPU, training loop. Antithetical to single-binary Go. |
| 12 | Knowledge graph embeddings (TransE) | Beautiful theory, absurd overhead for this scale. |
| 13 | TF-IDF on summaries | Summaries are short/noisy; tag graph already captures semantics better. Inverted index would blow memory. |
| 14 | Cosine on raw tag vectors | Strictly worse than PMI-weighted Jaccard (#4). Doesn't normalize for tag frequency. |
| 15 | Pure popularity + tag filter | The degenerate case. What you're trying to move away from. |
| 16 | Pure content similarity (no U) | The current `tag_overlap` signal. Answers "similar to X" but ignores U entirely. |
| 17 | Random | The actual baseline. Everything above should beat this. |

---

## Recommendation for Your Build

**Ship #1 (profile-weighted tag overlap) as the `taste` signal, blended with #2 (calibrated α=0.7) as the scoring framework.**

Concretely:
- Build a `taste` signal in `internal/signal/taste.go` that computes the weighted dot product of candidate tags against U's profile.
- In `rank.Score`, rank-normalize all signals to [0,1] over the pool before applying weights.
- Set the default tune to: `tag_overlap=0.15, neighbourhood=0.20, taste=0.30, quality=0.07, recency=0.05, collab=0.12, peer_rating=0.06, embedding=0.05` (sum=1.0, taste gets the α=0.3 enjoyment budget plus some of the old tag_overlap weight since it subsumes it).
- Expose `?profile=leather&alpha=0.7` in the API. `alpha` controls the taste-vs-similarity split; the tune handles the rest.

This gives you the interaction modeling of #1 with the tunability of #2, implementable in ~200 lines of Go, zero additional memory, and sub-50ms query time.

---

## Online Learning for Kindred — What Actually Works in Go

You already have the feedback surfaces. The question is what to do with the signal.

---

### The Three Feedback Loops You Already Have

| Signal | Source | Volume | Quality |
|---|---|---|---|
| **Explicit like/dislike** | `kindred profile rate --like/--dislike` | Low (manual) | High |
| **Implicit "read it"** | "✓ Read it" button on work cards | Medium | Medium (read ≠ loved) |
| **Arena judgments** | Pairwise comparisons | Low (12 ratings, 44 comparisons) | High (forced choice) |

Right now all three are **write-only** — they go into the database and nothing reads them back into the ranking. That's the gap to close.

---

### The Architecture: Three Layers of Learning

Think of it as three concentric loops running at different speeds.

```
┌─────────────────────────────────────────────────┐
│  Layer 3: STRATEGY (weeks)                      │
│  "Which signals does this user trust?"          │
│  Adjusts the tune weights per-profile.          │
│  e.g. "U ignores collab, loves tag overlap"     │
├─────────────────────────────────────────────────┤
│  Layer 2: TASTE (days)                          │
│  "Which tags does this user like?"              │
│  Adjusts the profile tag weights.               │
│  e.g. "U keeps rejecting MHA, boost Worm tags"  │
├─────────────────────────────────────────────────┤
│  Layer 1: CONTEXT (minutes)                     │
│  "What is this user looking at right now?"      │
│  Adjusts the seed set for this session.         │
│  e.g. "U clicked 3 DxD fics, add DxD to seeds"  │
└─────────────────────────────────────────────────┘
```

---

### Layer 1: Session Context (Easiest, Immediate Impact)

**What changes:** When U clicks "More like this" on a work, that work becomes a new seed *in addition to* the original. The session accumulates seeds.

**Implementation:**
- Store a `seen_seeds` list in the URL (`?seed=ao3_work:123&seed=ao3_work:456&seed=ao3_work:789`).
- Cap at 10 seeds (oldest dropped).
- The engine already handles multi-seed blending.
- Zero new code in the engine. Just a frontend change to append seeds.

**Feedback signal:** Clicking "More like this" = implicit positive signal for that work's tags. Clicking "Read it" = stronger signal.

**Effort:** ~30 lines of template/JS changes. No engine changes.

---

### Layer 2: Taste Profile Updates (The Big Win)

**What changes:** Every like/dislike/read-it/arena-judgment updates the profile's tag weights. The profile is no longer a static snapshot — it's a living model.

**The algorithm: Online Gradient Descent on Tag Weights**

When U gives feedback on candidate C:

```
For each tag t on C:
  if LIKE:    profile.Tags[t] += learning_rate × (1 - profile.Tags[t])
  if DISLIKE: profile.Tags[t] -= learning_rate × profile.Tags[t]
```

This is bounded to [0, 1] and converges: liked tags drift toward 1, disliked toward 0, untouched tags stay at their prior.

**Learning rate schedule:**
- Explicit like/dislike: `lr = 0.05` (strong signal, move fast)
- "Read it" mark: `lr = 0.02` (weaker — read ≠ loved)
- Arena win: `lr = 0.03` (forced choice, moderate confidence)
- Arena loss: `lr = -0.02` (loser isn't necessarily bad, just worse than winner)

**Decay:** Every 24 hours, all weights drift toward the prior by 1% (`w = 0.99·w + 0.01·prior`). This prevents the profile from ossifying and lets taste evolve.

**Implementation in Go:**

```go
// internal/profile/online.go

func (p *Profile) UpdateFromFeedback(
    workTags []int32,
    sentiment float64, // +1.0 like, -1.0 dislike, +0.5 read, etc.
    lr float64,
) {
    for _, t := range workTags {
        w := p.Tags[t]
        if sentiment > 0 {
            w += lr * sentiment * (1.0 - w)
        } else {
            w += lr * sentiment * w
        }
        w = clamp(w, 0.0, 1.0)
        if w < 0.001 {
            delete(p.Tags, t) // prune dead weights
        } else {
            p.Tags[t] = w
        }
    }
    p.UpdatedAt = time.Now()
}
```

**Storage:** The `profile_tags` table already exists. Add an `updated_at` column and a `feedback_count` column. The update is a single `UPSERT` per tag per feedback event.

**When to apply:** In the `serve` command, after any feedback endpoint returns success, call `UpdateFromFeedback` and flush to the state DB. The in-memory engine picks up the new weights on the next request (the profile is re-read from DB each query, or cached with a 60s TTL).

**Effort:** ~100 lines in `internal/profile`, ~50 lines wiring into the API handlers.

---

### Layer 3: Strategy Adaptation (Meta-Learning)

**What changes:** The system learns *which signals* U responds to, not just which tags.

**The algorithm: Per-User Signal Weight Bandit**

Treat each signal as an arm in a multi-armed bandit. After each feedback event, update the estimated reward for the signals that contributed to the recommended work.

```
For each signal s that contributed to candidate C's score:
  if U liked C:  signal_reward[s] += 1
  if U disliked: signal_reward[s] -= 0.5
  tune.Weights[s] = softmax(signal_reward)[s]
```

**Example trajectory:**
- Week 1: U rejects 3 collab-heavy recs but loves tag-overlap recs.
- Week 2: `collab` weight drops from 0.16 to 0.08, `tag_overlap` rises from 0.22 to 0.30.
- Week 3: U starts getting recs that match their tag taste even when the collab signal disagrees.

**Implementation:** Store per-profile signal rewards in a new `profile_signal_rewards` table (8 rows per profile, one per signal). Update after each feedback event. The `resolveTune` function reads these rewards and blends them with the global default.

**Effort:** ~80 lines. New table, update logic in the feedback handler, blend in `resolveTune`.

---

### Layer 0: Exploration (Prevents Filter Bubbles)

**The problem:** Pure exploitation (always recommend what U likes) creates a feedback loop where U only sees more of the same. The profile converges to a narrow cluster and stops discovering.

**The fix: ε-greedy with decay**

- With probability ε (default 0.1, decaying to 0.03 over 500 feedback events), inject a "surprise" candidate into the top-N results.
- The surprise candidate is drawn from the `surprise` corpus-query mode: high quality, low tag overlap with the profile.
- If U *likes* the surprise, the tags it carries get a big weight boost (they broke through the bubble).
- If U ignores it, ε decays slightly faster.

**Implementation:** In `rank.Score`, after MMR diversification, replace the lowest-scored item with a surprise candidate with probability ε. Tag the item with `"signal": "exploration"` in the evidence so U can see why it's there.

**Effort:** ~40 lines in the ranker.

---

### The Full Feedback Pipeline

```
User action
    │
    ▼
┌──────────────┐
│ API handler   │  POST /profile/leather/feedback
│ (new endpoint)│  body: {work_id, action: "like"|"dislike"|"read"|"skip"}
└──────┬───────┘
       │
       ▼
┌──────────────┐
│ Layer 1:      │  Append work to session seeds if "like"
│ Context       │  (URL param, no DB write)
└──────┬───────┘
       │
       ▼
┌──────────────┐
│ Layer 2:      │  profile.UpdateFromFeedback(work.Tags, sentiment, lr)
│ Taste         │  UPSERT to profile_tags
└──────┬───────┘
       │
       ▼
┌──────────────┐
│ Layer 3:      │  Update signal rewards for signals that
│ Strategy      │  contributed to this work's score
└──────┬───────┘
       │
       ▼
┌──────────────┐
│ Layer 0:      │  Adjust ε based on surprise acceptance rate
│ Exploration   │
└──────────────┘
```

---

### What This Looks Like for You Specifically

**Day 1:** You rate 10 recs. The profile barely moves (prior dominates).

**Day 7:** You've rated 50 recs. The profile has learned you hate "Character Death" and "Major Character Death" (weights near 0), love "Gamer" and "System" (weights near 1), and are neutral on "Angst" (weight ~0.5). Recommendations shift noticeably.

**Day 30:** You've rated 150 recs. The strategy layer has learned that `tag_overlap` and `taste` signals predict your likes better than `collab` (your bookmark neighborhood is small and noisy). The tune has shifted. The exploration layer occasionally surfaces a Worm-adjacent fic from a fandom you've never read — sometimes you love it, and the profile expands.

**Day 90:** The profile is a precise model of your taste. Recommendations feel eerily accurate. You occasionally reset ε to explore a new fandom.

---

### Total Implementation Estimate

| Component | Lines | Files | Effort |
|---|---|---|---|
| `UpdateFromFeedback` | ~60 | `internal/profile/online.go` | 2 hours |
| Feedback API endpoint | ~80 | `internal/api/feedback.go` | 2 hours |
| Session seed accumulation | ~30 | templates + JS | 1 hour |
| Signal reward tracking | ~80 | `internal/profile/strategy.go` | 3 hours |
| Exploration injection | ~40 | `internal/rank/explore.go` | 1 hour |
| Decay cron (in serve) | ~30 | `internal/engine/decay.go` | 1 hour |
| Tests | ~100 | `*_test.go` | 3 hours |
| **Total** | **~420** | **7 files** | **~13 hours** |

All of this fits in the existing architecture. No new dependencies, no new memory, no new tables beyond two small ones (`profile_signal_rewards`, `feedback_log`). The engine stays under 220 MB. Query latency stays under 300ms because the profile update happens *after* the response is sent (fire-and-forget goroutine).

---

## Building a Personal Enjoyment Predictor from Your 267 Ratings + 44 Bookmarks

Your situation is actually a well-studied problem: **cold-start preference learning with a small-but-high-quality label set, enriched by implicit feedback, improved online.** Here's the full pipeline, phased by effort.

---

## Phase 0: Match Your Calibre List to the Corpus

Before any ML, you need to know which of your 267 rated fics exist in the AO3 mirror.

**The matching problem:**
- Calibre has `title` + `authors` as strings.
- The mirror has `works.title` + `works.authors` as strings.
- Expect ~70–85% match rate (variations in punctuation, author pseudonyms, title revisions).

**The algorithm:**

```go
// internal/profile/calibre.go

func MatchCalibreList(ctx context.Context, db *sql.DB, ratings []CalibreRating) ([]Match, Report) {
    var matches []Match
    report := Report{Total: len(ratings)}
    
    for _, r := range ratings {
        normTitle := normalize(r.Title)   // lowercase, strip punct, collapse spaces
        normAuthor := normalize(r.Authors)
        
        // 1. Exact match on normalized title + author
        if m := exactMatch(db, normTitle, normAuthor); m != nil {
            matches = append(matches, Match{WorkID: m.ID, Rating: r.Rating, Confidence: 1.0})
            report.Exact++
            continue
        }
        
        // 2. Fuzzy title match (Levenshtein ≤ 3) within author's works
        if m := fuzzyMatch(db, normTitle, normAuthor, 3); m != nil {
            matches = append(matches, Match{WorkID: m.ID, Rating: r.Rating, Confidence: 0.8})
            report.Fuzzy++
            continue
        }
        
        // 3. Title-only match (last resort, risky)
        if m := titleOnlyMatch(db, normTitle); m != nil {
            matches = append(matches, Match{WorkID: m.ID, Rating: r.Rating, Confidence: 0.5})
            report.TitleOnly++
            continue
        }
        
        report.Unmatched = append(report.Unmatched, r)
    }
    return matches, report
}
```

**Honest reporting:** The report shows "227/267 matched (85%): 180 exact, 35 fuzzy, 12 title-only, 40 unmatched." You print the unmatched list so you can manually resolve high-rated ones (e.g. "Worm" might be in Calibre as "Worm by Wildbow" but in AO3 as just "Worm").

**CLI:**
```bash
kindred profile import-calibre leather \
    --paste /home/alvaro/.hermes/profiles/sysadmin/pastes/paste_6_142622.txt \
    --min-confidence 0.8 \
    --report /tmp/match-report.txt
```

**Output artifact:** A rated profile `leather` with ~220 matched works, each carrying your 1–10 rating.

---

## Phase 1: Build the Initial Enjoyment Model (The Cold Start)

With 220 rated works you have enough signal for a **tag-weighted linear model**. This is the baseline that everything else refines.

### 1.1 Compute Per-Tag Enjoyment Weights

For every tag `t`, compute its enjoyment weight using your ratings:

```
weight(t) = (Σ (rating_i - 5.5) · indicator(work_i has tag t)) / (count(t) + prior_strength)
```

Breakdown:
- `rating_i - 5.5` centers ratings so 6+ is positive, 5 and below is negative. This is important: unweighted sums bias toward common tags.
- The denominator smooths rare tags toward zero (a tag appearing on 2 works gets heavily smoothed; a tag on 50 works barely at all).
- `prior_strength = 3` works well empirically.

**Example weights from your list (hypothetical):**
```
Gamer                   +3.2   (12 works, avg rating 8.1)
System                  +2.8   (18 works, avg rating 7.6)
Self-Insert             +2.1   (24 works, avg rating 7.2)
Worm                    +1.9   (8 works, avg rating 7.8)
Harry Potter            +1.4   (15 works, avg rating 6.8)
Angst                   -0.3   (28 works, avg rating 5.2)  ← neutral, you don't care
Alpha/Beta/Omega        -1.8   (3 works, avg rating 3.3)
Major Character Death   -2.4   (6 works, avg rating 3.1)
```

The sign tells you preference direction; the magnitude tells you confidence × strength.

### 1.2 Score Any Candidate Work

```
enjoyment(C) = (Σ_t weight(t) · indicator(C has tag t)) / sqrt(|tags(C)|)
```

The `sqrt(|tags|)` normalization prevents fics with 40 tags from mechanically dominating. This is the same length normalization used in TF-IDF.

### 1.3 Add the Bookmark Signal

Your 44 bookmarks are implicit positive labels (you bookmarked, so you probably liked). But they're noisy — some bookmarks are "to read later," not "loved." Treat them as **weak positives** with rating=7:

```
For each bookmarked work b not already in rated set:
    pseudo_rating[b] = 7.0
    confidence[b] = 0.4   // lower than explicit ratings
```

Include them in the weight computation with a confidence multiplier:

```
weight(t) = Σ (rating_i - 5.5) · confidence_i · indicator / (Σ confidence_i + prior)
```

Your 220 explicit ratings (confidence 1.0) dominate, but the 40ish extra bookmarks add tag coverage.

### 1.4 Validate Honestly

Hold out 20% of your rated works. Train on 80%. Compute Spearman rank correlation between predicted enjoyment and actual rating on the held-out set.

**What to expect:**
- Random: ρ ≈ 0
- Popularity baseline (kudos): ρ ≈ 0.1
- This model: ρ ≈ 0.4–0.6 (good)
- Oracle (another human predicting your ratings): ρ ≈ 0.7

If you get ρ < 0.3, something is wrong (likely a matching problem or too many unmatched top-rated works).

**CLI:**
```bash
kindred profile validate leather --holdout 0.2 --seed 42
# → Spearman ρ=0.52, Kendall τ=0.38, 5-fold CV mean 0.49 ± 0.07
# → Top predictors: Gamer, System, Self-Insert, Worm
# → Top anti-predictors: Major Character Death, Alpha/Beta/Omega
```

---

## Phase 2: Blend Enjoyment with Similarity

Now that you have `enjoyment(C)` as a well-calibrated score, blend it with the existing similarity signals from the engine (tag overlap with seed, neighbourhood, collab).

**The scoring rule** (from your α=0.7 request):

```
score(C) = 0.7 · rank_norm(similarity_to_seed(C)) + 
           0.3 · rank_norm(enjoyment(C))
```

Where `rank_norm(s) = rank(s) / pool_size` to put both terms on [0,1] regardless of their raw scales.

**Why rank-normalize:** Your enjoyment scores might range from -4 to +6. Similarity scores (summed from existing signals) might range from 0 to 200. Linear blending with raw values makes α=0.7 meaningless. Rank-normalization makes α=0.7 mean "70% of the ranking influence comes from similarity."

**The `taste` signal** registers as a new signal in `internal/signal/taste.go`:

```go
type TasteSignal struct {
    Profile *profile.Profile
}

func (s *TasteSignal) Compute(ctx context.Context, candidate *corpus.Work) (float64, string, error) {
    if s.Profile == nil || len(s.Profile.Tags) == 0 {
        return 0, "", rank.ErrSkip
    }
    
    var sum float64
    var contribs []tagContrib
    for _, t := range candidate.TagIDs {
        if w, ok := s.Profile.Tags[t]; ok {
            sum += w
            contribs = append(contribs, tagContrib{t, w})
        }
    }
    normalized := sum / math.Sqrt(float64(len(candidate.TagIDs)))
    
    // Build human-readable reason
    sort.Slice(contribs, func(i, j int) bool { 
        return math.Abs(contribs[i].w) > math.Abs(contribs[j].w) 
    })
    top := contribs[:min(3, len(contribs))]
    reason := formatTopContribs(top) // "Strong match: Gamer (+3.2), System (+2.8)"
    
    return normalized, reason, nil
}
```

The tune becomes:
```
tag_overlap=0.15, neighbourhood=0.20, taste=0.30, 
quality=0.07, recency=0.05, collab=0.10, 
peer_rating=0.08, embedding=0.05
```

Taste gets the biggest weight because it's the only personalized signal.

---

## Phase 3: Online Learning From Browsing

Once Phase 2 ships, every interaction updates the profile. This is where the system *gets better over time*.

### 3.1 Feedback Events and Their Weights

| Event | Sentiment | Learning Rate | Notes |
|---|---|---|---|
| Thumbs up on recommendation | +1.0 | 0.05 | Strong explicit positive |
| Thumbs down on recommendation | -1.0 | 0.05 | Strong explicit negative |
| "✓ Read it" marked | +0.3 | 0.02 | Read ≠ loved, weak positive |
| Clicked through to AO3 | +0.1 | 0.01 | Curiosity, very weak positive |
| Dismissed/skipped from top-N | -0.2 | 0.015 | Mild negative |
| Arena win (preferred over pair) | +0.4 | 0.03 | Forced choice, moderate |
| Arena loss (not preferred) | -0.2 | 0.015 | Softer — the loser might still be ok |
| Blocked a tag | custom | per-tag -1.0 | Hard rule, not gradient |

### 3.2 The Update Rule

For each tag `t` on the fic that received feedback:

```
old_weight = profile.Tags[t]
delta = learning_rate × sentiment × (sign_bound - old_weight)

where sign_bound = +∞_approx (effectively +10) if sentiment > 0 
                   -∞_approx (effectively -10) if sentiment < 0

new_weight = old_weight + delta
```

This is **sign-bounded gradient descent** — weights can grow unboundedly in the signed direction but each update is a fraction of the gap, so they converge logarithmically. A tag you consistently like will asymptote to a stable high positive weight; a tag you consistently hate will asymptote to a stable negative.

Why this instead of bounded-to-[0,1]: your initial weights are already in the [-3, +3] range from the rating-based computation. A [0,1] clamp would collapse all the negative information. Signed weights let the model remember "I hate this" as strongly as "I love this."

### 3.3 Confidence-Weighted Updates

Not all feedback events are equally reliable. Track `feedback_count` per tag; new tags move fast, old tags move slow:

```
effective_lr = base_lr / (1 + log(1 + feedback_count[t]))
```

A tag with 0 prior feedback updates with full `lr`. A tag with 100 prior feedback events updates at ~`lr/4.6`. This prevents a single aberrant click from undoing weeks of learned preference.

### 3.4 Decay Toward the Base Model

Your taste evolves. Without decay, old ratings dominate forever. Every 24 hours:

```
for each tag t:
    online_weight = profile.Tags[t]
    base_weight = profile.BaseTags[t]  // from phase 1, immutable
    profile.Tags[t] = 0.995 × online_weight + 0.005 × base_weight
```

The decay is slow (0.5% per day toward the base). Over 6 months, a tag with no new feedback drifts about 60% back to its initial value. This gives online learning room to adapt while preventing total profile drift.

**Important:** The base weights (`BaseTags`) are stored separately and never updated by online learning. They're the memory of what your Calibre list said. Online learning updates `Tags`, which starts as a copy of `BaseTags`.

---

## Phase 4: Arena as a Preference Oracle

The arena is the highest-quality signal you have — a forced binary choice removes the ambiguity of ratings ("was this a 7 or an 8?"). Use it to train the model more aggressively.

### 4.1 Pairwise Loss Instead of Pointwise

For each arena comparison (winner W, loser L):

```
predicted_margin = enjoyment(W) - enjoyment(L)
desired_margin   = 1.0   // winners should score at least 1.0 higher

loss = max(0, 1.0 - predicted_margin)   // hinge loss

if loss > 0:
    for each tag t on W but not L:
        profile.Tags[t] += arena_lr × loss
    for each tag t on L but not W:
        profile.Tags[t] -= arena_lr × loss
    for each tag t on both:
        no update  // shared tags don't explain the preference
```

This is **SVM-style online pairwise learning**. It only updates when the model got the preference wrong (or weakly right). It only updates on the *distinguishing* tags — tags present in both the winner and loser don't explain U's choice.

**Why this is better than pointwise:** Pointwise updates (treating winner as +1, loser as -1) assume absolute quality judgments. Pairwise updates respect that U chose *between these two specific options* — maybe both were mediocre, maybe both were great, but one was better *for the dimensions that distinguished them*.

### 4.2 Arena-Driven Signal Discovery

Over hundreds of arena judgments, the pairwise updates reveal which tag *dimensions* U actually cares about. Tags where you never show preference stay neutral; tags where you consistently pick one direction accumulate strong weights.

Expect this to surface non-obvious preferences: "You consistently prefer first-person POV even though you never thought to rate on that dimension."

---

## Phase 5: Exploration (Prevents the Bubble)

Pure exploitation creates filter bubbles. The profile converges to a narrow cluster and the system stops showing you anything new.

### 5.1 ε-Greedy Injection

In the top-N recommendation list, with probability ε (default 0.1), replace the N-th item with a surprise:

```
if random() < ε:
    surprise = pick_surprise(seed, profile)
    results[N-1] = surprise
    surprise.evidence += "Shown to help the system learn your taste."
```

Where `pick_surprise` draws from works that are:
- High quality (kudos/word_count > median)
- Low enjoyment score (profile hasn't seen tags like these)
- Not previously shown or rated

### 5.2 Adaptive ε

If U engages with surprises (clicks, likes), ε stays high. If U consistently ignores or dislikes them, ε decays:

```
if surprise_rate_accepted > 0.3:
    ε = min(0.15, ε + 0.005)
elif surprise_rate_accepted < 0.1:
    ε = max(0.03, ε - 0.005)
```

Over time, ε self-tunes to the right level of exploration for U.

---

## Phase 6: Monitoring & Trust

The system must be legible — U should be able to see what it has learned and correct it.

### 6.1 Profile Introspection UI

A page at `/profile/leather` showing:

```
TOP POSITIVE TAGS (what the system thinks you love)
  Gamer                +4.1    (base +3.2, +0.9 from 23 feedback events)
  System               +3.5    (base +2.8, +0.7 from 18 feedback events)
  Self-Insert          +2.9    (base +2.1, +0.8 from 15 feedback events)
  ...
  [edit] [reset tag]

TOP NEGATIVE TAGS (what the system thinks you avoid)
  Major Character Death  -3.1   (base -2.4, -0.7 from 12 feedback events)
  Alpha/Beta/Omega       -2.6   (base -1.8, -0.8 from 8 feedback events)
  ...
  [edit] [reset tag]

RECENT LEARNING
  2 hours ago: liked "Worm/HP Crossover" → boosted Worm, Harry Potter, SI
  5 hours ago: disliked "Fluff Only AU" → reduced Fluff, No Angst
  yesterday:   arena win: "Gamer Harry" over "Dark Harry" → boosted Gamer
  
VALIDATION
  Last held-out Spearman: ρ=0.58 (up from base ρ=0.52)
  System confidence: moderate (420 total feedback events)
```

### 6.2 Correction Interface

- Click a tag to see works that contributed its weight.
- "Reset tag" removes online learning for that tag, restoring base weight.
- "This tag is wrong" sets an immutable override.

### 6.3 A/B Validation

Every 30 days, re-run the held-out validation. If online learning *degraded* performance (ρ dropped), surface a warning: "The system has gotten less accurate over the last month. Consider resetting online weights."

Don't hide failures. If the model is getting worse, say so.

---

## Full Implementation Phasing

| Phase | What ships | Lines | Effort | Dependency |
|---|---|---|---|---|
| 0 | Calibre matcher + rated profile | ~250 | 1 day | — |
| 1 | Rating-weighted base model + validator | ~200 | 1 day | 0 |
| 2 | `taste` signal + rank normalization | ~180 | 1 day | 1 |
| 3 | Online feedback updates + decay | ~300 | 2 days | 2 |
| 4 | Arena pairwise learning | ~150 | 1 day | 3 |
| 5 | Exploration injection | ~80 | 0.5 days | 2 |
| 6 | Profile introspection UI | ~400 | 1.5 days | 3 |
| **Total** | **Shipped incrementally** | **~1560** | **~8 days** | |

Each phase is deployable independently. Phase 0+1 alone gives you a working personalized ranker from your Calibre list. Phase 2 integrates it into recommendations. Phases 3+4 make it improve over time. Phases 5+6 prevent degenerate behavior.

---

## The One-Liner for Your Discord

> paste a fic you loved and get recommendations ranked by how much the system thinks you'd enjoy them — starting from your reading history and learning from your likes, dislikes, and arena picks over time.

---

# PLAN — the me-first site

> Implements the owner's request (verbatim): *"similar to what we did before,
> changing the work recommendations to be 'works similar to x that I would
> enjoy', I want to refactor the whole site to be about me first, making it as
> useful as possible for me while still being somewhat useful to other
> users."*
>
> The "before" is the α=0.7 taste blend (`docs/handoff-2026-10-08-taste-blend.md`):
> `score = 0.7·similarity + 0.3·enjoyment`. This plan generalises that from one
> ranking mode to the organising principle of every page, and finishes the
> engine half of the handoff (the taste signal) that its step list left open.
>
> **This plan is a living contract.** When a step is found wrong, fix the plan
> in the same commit as the code fix and say in the commit message what you
> changed and why.

---

## 0. What "me first" means here, and what it does not

The site has exactly one reader who matters: the owner (AO3 account
`Leather_Release_9057`). Every surface must answer the owner's question —
*what would I enjoy* — not the corpus's question (*what exists*). A visitor
who is not the owner gets today's site: honest, generic, still fully usable,
with their own session learning from their own gestures. Nobody else ever
sees owner data.

Three rules follow, and every step below serves one of them:

1. **Personalisation is the default view, not a mode.** Where a page ranks,
   the owner's taste is in the ranking unless the URL explicitly opts out
   (`?sort=`, `?alpha=0`). Where a page seeds (For You, surprise, tag
   blends), the owner's own likes are the seeds.
2. **The generic view stays reachable and honest.** Every personalised page
   says which view it is showing (the `taste-note`/`taste-error` pattern from
   SPEC §2.6 — silence is a bug). `?personal=0` shows the generic view.
3. **No owner leakage.** Owner weights, profile names, and liked-work lists
   are resolved only for owner requests. A non-owner page must not contain a
   single byte of owner state (tested, not promised).

Deliberately NOT in scope: accounts, login, JavaScript, multi-owner support,
changing the collab algorithm, deleting any existing page.

## Verified before this plan was written

Every symbol below was checked against the tree (HEAD `ea0f31c` + the
uncommitted rated-profile work, which is step 1's baseline). Do not trust a
symbol not on this list — grep first, as `PLAN-web-ui.md` teaches.

- **Identity.** `ArenaCookieName = "kindred_arena"` — `internal/web/arena.go:26`.
  `func (d Deps) arenaSession(ctx, w, r) (sessionKey, ownerKey string, err error)`
  mints the cookie on demand — `internal/web/arena.go:1206`.
  `func (s *Store) OwnerKey(ctx, identifier) (string, error)` = HMAC-SHA256 of
  the identifier under the stable salt — `internal/store/arena.go:139`.
- **Per-reader learned state.** `arena_user_tag_weights(owner_key, tag_id,
  weight, n)`, read by `Store.TagWeights` (`internal/store/arena.go:752`),
  written by `Store.UpsertTagWeight` (:779) with n-damped averaging.
  Like/dislike feedback: `internal/store/feedback.go` — `FeedbackLike = 1` /
  `FeedbackDislike = -1` (:49), rates like 0.5 / dislike 0.7 (:66),
  `RecordFeedback` (:91), `LearnFromFeedback` (:206), `applyDeltas` (:221),
  `FeedbackCount` (:240), `FeedbackForWorks` (used at
  `internal/web/handlers.go:2260`). Web POST `/feedback` → `postFeedback`
  (`internal/web/arena.go:485`); buttons on the recommend page render pressed
  state via `feedbackStances` (`internal/web/handlers.go:2244`).
- **Rated profiles (uncommitted, green).** `profile.Profile` carries `Tags`,
  `BaseTags`, `Evidence`, `Feedback` (`internal/profile/profile.go:38`);
  `BuildFromRatings` (`internal/profile/rated.go`) builds signed shrunk
  weights `Σ(rating−5.5)·conf / (Σconf + 3)`; `internal/ratedlist`
  (`LoadFromFile` :332, `MatchToCorpus` :694) parses calibredb paste and
  matches to the mirror; CLI `kindred profile import --paste … --save` and
  `profile validate` (`cmd/kindred/profile_rated.go`); `openProfileStore`
  (`cmd/kindred/profile.go:390`). Store schema in
  `internal/store/store.go:273` + `EnsureSchema` profile tables.
- **Engine.** `Request` (`internal/engine/engine.go:123`) has Seeds, Kind, N,
  Tune, MaxPerGroup, GroupBy, PoolSize, Exclude, PoolMode, BlockedTagIDs,
  SeenIDs — **no taste field**. `signals()` (engine.go:~424) appends
  TagOverlap, Neighbourhood, Quality, Recency, Popularity, PeerRating, Collab,
  Embedding (full mode); signals are registered UNCONDITIONALLY and skip with
  `rank.ErrSkip` so absence lands in `meta.degraded[]`. `rank.Score`
  (`internal/rank/rank.go:167`) is linear: `total += tune.Weights[name] *
  value`. `DefaultTune()` (engine.go:366): tag_overlap .22, neighbourhood .30,
  quality .09, recency .09, popularity .09, embedding .10, collab .16,
  peer_rating .11. Named tunes resolve from the state DB (`resolveTune`
  engine.go:213; `kindred tune --name X --set a=1,b=2`). `n` cap 200
  (engine.go:436).
- **Collab evidence for the owner.** `user_work_interactions`: 180,677 rows /
  6,261 readers; the owner is user id **99** with **44** `bookmarked` rows
  (`users.bookmark_count` says 41 — the two sources disagree; trust the
  interaction rows). `collab.NeighboursFor` (`internal/collab/collab.go:727`)
  votes per-seed with a per-seed limit and never lets a seed vote for itself.
- **Web routes** (`internal/web/handlers.go:245 route`): `/` renders search;
  `/search`, `/author`, `/work/{id}`, `/tag/{id}` (taste blend default,
  `blendTagTaste` handlers.go:1802 — seeds are the tag's 5 most-kudoed works),
  `/recommend` (:1986), `/fandoms` (:364, `?profile=` already feeds ranking
  via `profileTagIDs`), `/underrated` (:431), `/neighbours` (:541),
  `/surprise` (:489, `corpusquery.Surprise`, no reader input),
  `/profiles`, `/profile`, `/arena`, `/leaderboard`, `/my-ranking`, `/block`,
  `/rank/{id}`; POSTs `/arena/judge`, `/block`, `/seen`, `/feedback`.
- **Config.** `internal/config/config.go:16` — env-resolved (`Load` :65),
  flag-bound (`Bind` :82). `StableSalt` is env-only on purpose (:26); the
  owner secret follows the same rule.
- **Corpus-side owner row**: `users` → `Leather_Release_9057` id 99; work ids
  resolvable via `user_work_interactions WHERE user_id=99 AND
  interaction_type='bookmarked'` (dedupe: the table stores duplicate rows per
  seed — `select distinct` is mandatory).
- **Gates.** `make verify`; `make e2e` (~115 Playwright tests, hermetic over
  `internal/testcorpus` via `cmd/e2eserver`); `scripts/budget.sh` (220 MiB
  cap); `scripts/deploy.sh` (the only deploy path) → `scripts/check-deploy.sh`
  + `scripts/check-provenance.sh`; `docs/goal-check.py`.
- **Hard constraints** (`docs/todo-everything.md` §8): no-JS, all state in the
  URL, deploy only via `scripts/deploy.sh`, sanitized filenames everywhere.

## Design decisions

### D1 — owner identity: one env secret, no accounts

`KINDRED_OWNER_SESSION` (env-only, like `StableSalt`) holds a session key the
owner installs in their browser once (a `kindred_arena` cookie with that exact
value). At startup the server computes `ownerKey = OwnerKey(ctx,
KINDRED_OWNER_SESSION)` once and holds it. A request is the owner's when its
cookie-derived ownerKey equals the held one. `KINDRED_OWNER_PROFILE` (env,
default `leather`) names the stored rated profile.

Why this shape: no new auth surface, no login page, no second cookie; the
existing `arenaSession` machinery keeps minting sessions for everyone else and
stays untouched; clearing cookies does not orphan the owner's data because
their ownerKey is a pure function of the env secret. The trade-off is stated
plainly: whoever holds the secret IS the owner — acceptable because the site
is loopback/TLS-proxy personal infrastructure, and the secret never appears in
process listings (env, not flag).

### D2 — taste enters the engine as a normal signal, and α as a derived tune

`signal.Taste{Weights map[int32]float64}` scores a candidate by the normalised
dot product of its tag vector with the reader's signed weight vector, into
[-1, 1], with the top contributing tags in the reason string. It is registered
UNCONDITIONALLY (the repo's five-times-confirmed rule) and skips with
`rank.ErrSkip` when weights are empty → `meta.degraded[]`.

α is NOT new scoring machinery. `rank.Score` stays linear; α is expressed as a
named tune: `blend70` sets `taste 0.30` and rescales the other eight weights
to sum 0.70, so `score ≈ 0.7·content + 0.3·enjoyment` exactly when taste is
the only personal signal with data. `Request.TasteWeights` carries the
weights; the tune carries the weight. The handoff's scale warning
(neighbourhood values ~180 vs a ~60 total) is handled in step 4's measurement,
not assumed away.

Weight assembly (`base + learned`) and its scale check are step 4's job, in
the web layer, not the engine's — the engine must rank with or without a
store, exactly like BlockedTagIDs.

### D3 — seeds come from the owner's own evidence, in this priority order

1. **Liked works** (`feedback` rows, polarity +1) — the strongest gesture.
2. **Corpus bookmarks** (user 99's 44, `select distinct work_id`).
3. **Rated works** — persisted at import time (step 3 adds
   `profile_works`), because `Profile.Works` is a count today.
4. **Top profile tags** — the cold-start fallback when 1–3 are empty.

Every seed-using page (For You, surprise, tag blend) draws from this list
through one helper, so the order is defined once. Cap 20 seeds; the per-seed
collab limit already bounds the vote.

### D4 — the generic view is the owner view with personalisation switched off

Not a fork. `?personal=0` (and absence of the owner cookie) runs the same
handlers with `TasteWeights = nil`, profile = nil, and the current seed
rules. One code path, two configurations, and the page note names which one
rendered. This is what "somewhat useful to other users" means concretely:
they get the corpus, search, filters, their own sessions and feedback —
minus the owner's taste.

---

## Step 1 — commit the rated-profile baseline

The tree carries the uncommitted rated-profile work (import, validate,
feedback learning, `/feedback` POST, profile evidence fields). It builds,
vets and tests green after two import-line fixes in
`cmd/kindred/profile_rated.go` (module path typo `polarosocial` →
`polarisocial`, and the `ratedlist` import alias). Commit it as its own
baseline commit so every later step diffs against a green tree, and push
`HEAD` to **both** remotes.

```bash
gofmt -w internal/ cmd/ && go build ./... && go vet ./... && go test ./...
git add -A && git commit -m "Rated profiles: calibre import, validation, like/dislike feedback learning"
git push forgejo HEAD && git push github HEAD && git ls-remote --heads forgejo main github 2>/dev/null | true
```

Expected: build/vet/test exit 0; both remote SHAs equal local HEAD.

## Step 2 — owner identity

**`internal/config/config.go`**: add `OwnerSession string` and
`OwnerProfile string`; resolve in `Load()` from `KINDRED_OWNER_SESSION` /
`KINDRED_OWNER_PROFILE` (default `leather` when a session is set). **No flags**
— env-only, same reasoning as `StableSalt`, with a comment saying so.

**`internal/web` Deps**: add `OwnerKey string` and `OwnerProfile string`
fields (empty = no owner configured). Wire them in `runServe` from config:
`OwnerKey` = `store.OwnerKey(ctx, cfg.OwnerSession)` computed once, failure to
compute logged and treated as no-owner. Add:

```go
// isOwner reports whether this request's ownerKey is the configured owner's.
func (d Deps) isOwner(ownerKey string) bool
```

and a small `owner(r) (ownerKey string, ok bool)` wrapper around
`arenaSession` that returns `ok=false` on any error.

**Verification**: unit test — with a store fixture, `OwnerKey("s1") != nil`;
`isOwner` true only for the matching key; a server built without
`KINDRED_OWNER_SESSION` has `OwnerKey == ""` and `isOwner` is always false.

## Step 3 — persist rated works, and expose the owner's evidence lists

**`internal/store/store.go` (`EnsureSchema`)**: add

```sql
CREATE TABLE IF NOT EXISTS profile_works(
    name TEXT NOT NULL, work_id INTEGER NOT NULL, rating INTEGER NOT NULL,
    PRIMARY KEY(name, work_id)
);
```

**`profile.Store.Save`** writes the rated rows (`BuildFromRatings` already has
them); `Load` returns them on `Profile.WorksList []RatedWorkRef`.

**`internal/store/feedback.go`**: add

```go
// LikedWorks returns the ids this reader pressed like on, newest first.
func (s *Store) LikedWorks(ctx context.Context, ownerKey string, limit int) ([]int64, error)
```

**`internal/collab` or `internal/corpus`**: add a corpus-side helper

```go
// BookmarkedWorks returns the distinct works a mirror user bookmarked.
func BookmarkedWorks(ctx context.Context, db *sql.DB, userID int64) ([]int64, error)
```

(`SELECT DISTINCT work_id FROM user_work_interactions WHERE user_id = ? AND
interaction_type = 'bookmarked'` — DISTINCT is mandatory; duplicate rows per
seed are a measured property of this table.)

**Verification**: unit tests over `internal/testcorpus` — liked works round-trip;
bookmarked works dedupes; a profile saved with rated works loads them back.

## Step 4 — the taste signal and the blend70 tune

**`internal/signal/taste.go`** (new):

```go
// Taste scores a candidate by the reader's signed tag weights.
// Value = (Σ w(t)·[t ∈ candidate tags]) / (Σ |w(t)| over the candidate's
// tags ∨ the weights' L1 norm, whichever is smaller) → [-1, 1].
// Reason names the top-3 contributing tags with signed contributions.
type Taste struct{ Weights map[int32]float64 }
```

Empty `Weights` → `rank.ErrSkip` for every candidate (degraded, never silent
zero). **`internal/engine/engine.go`**: add `TasteWeights map[int32]float64`
to `Request`; append `signal.Taste{Weights: req.TasteWeights}` in `signals()`
unconditionally; `DefaultTune()` gains `taste: 0` (off by default — zero
behaviour change for existing callers and tests). Create the named tune in
`cmd/kindred/tune.go`'s seeding path or document the one-liner:

```bash
kindred tune --name blend70 --set taste=0.3,tag_overlap=0.154,neighbourhood=0.21,quality=0.063,recency=0.063,popularity=0.063,embedding=0.07,collab=0.112,peer_rating=0.077
```

(other weights = old values × 0.7/1.16, preserving their ratios; `embedding`
excluded → renormalise handles lite mode as today).

**The scale measurement (do not skip)**: on thinkcentre against the real
corpus, run `recommend` with `--tune blend70` and taste weights loaded, dump
the per-signal decomposition for the top 20 (the JSON evidence array already
carries `signal/value/weight`), and check taste's contribution is neither
swamped (<5% of total) nor dominant (>60%). If swamped, the value
normalisation in `Taste` is the thing to fix — not the tune.

**Verification**: `go test ./internal/signal ./internal/engine` — new tests:
taste scores a candidate carrying a positively-weighted tag above one
carrying a negatively-weighted tag; empty weights → degraded, list unchanged
vs taste absent; tune blend70 resolves and sums to ~1.0.

## Step 5 — the web layer resolves taste once per request

**`internal/web/handlers.go`** (or a new `internal/web/personal.go`):

```go
// tasteFor resolves this request's personalisation inputs.
// Owner + configured profile: weights = profile.BaseTags ⊕ learned
// (Store.TagWeights, added on top; both sources already signed).
// Anything missing degrades independently and is reported in the page note.
func (d Deps) tasteFor(ctx context.Context, w http.ResponseWriter, r *request) (weights map[int32]float64, seeds []int64, note string, ok bool)
```

- `seeds` per D3's priority (liked → bookmarked → rated → top profile tags),
  capped 20, deduped, each as `engine.Seed{Kind: corpus.AO3Kind}`.
- `ok=false` for non-owners (and for the owner when no profile is stored —
  the note says the profile is missing, matching the taste-note/taste-error
  rule).
- The learned-weight scale check prints once at startup into the log:
  top-10 base vs learned weights, so a 10× mismatch between the two sources
  is visible rather than silently ranking.

**Verification**: unit test with a fixture store — owner request returns
weights containing a liked work's tags; non-owner request returns `ok=false`
and empty weights.

## Step 6 — `/` becomes For You; `/recommend` grows `?for=me`

**`/`**: when `tasteFor` returns ok, render `foryou.html` (new template +
`render_pages_test.go` case — the goal-check page-coverage clause fails
without it): seeds → `engine.Recommend{N: 50, Tune: "blend70", Exclude: true,
BlockedTagIDs, SeenIDs}`, standard work cards with evidence. Heading names
the view ("For you — 0.7 similar · 0.3 would-you-enjoy"); the note names the
seed source and count. `?personal=0` (or non-owner) renders today's search
page unchanged.

**`/recommend`**: `?for=me=1` (default for the owner, absent otherwise)
replaces the required `?seed=` with `tasteFor` seeds; an explicit `?seed=`
still wins. `?alpha=` (clamped [0,1], default 0.7) selects between `blend70`
and the plain tune by setting the taste weight to `1−α` and rescaling the
rest — one small helper, not a second tune table. The page keeps every
existing control; degraded signals still render.

**Verification**: e2e (hermetic fixture): owner-shaped request on `/` shows
the for-you heading and ≥1 card with evidence; `?personal=0` shows the search
box; `/recommend?for=me=1` returns 200 with ≥1 result and `meta` echoed;
non-owner gets today's behaviour on both pages.

## Step 7 — the rest of the site goes me-first

Each item: owner path uses `tasteFor`; `?personal=0`/non-owner keeps today's
behaviour; the page note names the view. No new routes except `/`'s template.

- **`/work/{id}`** — the "more like this" block passes `TasteWeights` and the
  owner's tune: the heading becomes "similar to this that you would enjoy".
  This is the original α=0.7 ask, applied at its natural home.
- **`/tag/{id}`** — `blendTagTaste` seeds: use the owner's liked works that
  carry this tag first (cap 5), fall back to the tag's top-kudos five; the
  taste-note states which seed source ran.
- **`/fandoms`** — `?profile=` defaults to the owner's profile for owner
  requests (mechanism exists: `profileTagIDs` → corpusquery seeds).
- **`/underrated`** — owner default `?profile=`; underrated pool re-ranked by
  taste before the cut, with the note carrying both halves.
- **`/surprise`** — owner: pick a random liked/bookmarked work as the seed
  (D3 list, `sessionRand` for the draw), `Recommend N=20`, show one; the page
  names the seed it surprised from. Non-owner: today's `corpusquery.Surprise`.
- **`/search`** — deliberately unchanged (lookup, not discovery; taste-ordering
  matches would hide exact hits). State this in the plan and leave it.

**Default blocked tags seeding** (SPEC §2.7, [planned] → live): on first
write to a fresh reader's block list (no rows yet), seed the defaults —
using the owner-corrected list: the SPEC §2.7 list MINUS `Romance` PLUS
`Scat` (the owner's explicit edit; fix §2.7's list in the same commit). A
reader's own `/block` edits still replace the list (mechanism already live).

**Verification**: e2e asserts per page: personalised element visible for the
owner shape, generic element visible with `?personal=0`, and the note present
in both (silence is the failure state — the taste-note/taste-error rule).

## Step 8 — no owner leakage (the privacy gate)

Unit + e2e assertions on the same template sweep: render every page twice
(owner/non-owner shapes) and assert the non-owner render contains none of the
owner's tag names, profile name, or seed work titles. The owner's evidence
lists are built per request and never cached process-wide beyond the startup
ownerKey.

**Verification**: new `internal/web` test walking all routes both ways;
`go test ./internal/web -run TestNoOwnerLeakage` exit 0.

## Step 9 — evidence wording, introspection, docs

- Taste evidence line names the movement: e.g. `matches your taste: "gamer"
  (+1.8), "system" (+1.2) — would-you-enjoy 0.62`; collab keeps its
  taste-overlap-percentage target phrasing (SPEC §2.6).
- `/profile` (introspection) shows `BaseTags` beside learned weights and the
  feedback counts (`FeedbackCount` already exists) — the reader can see what
  the ratings said vs what the buttons taught.
- **SPEC.md**: new §2.8 "Me-first surfaces" with per-page status markers
  updated as steps land; §2.7 list corrected (Romance out, Scat in); §3.2
  gains `?for=me`, `?alpha`, `?personal` parameters; §5.6 documents
  `profile import`/`validate`.
- **HANDOFF.md / docs/what-is-left.md**: dated section, tests that pin each
  item, open findings.

**Verification**: `docs/goal-check.py` (all clauses); `scripts/check-doc-commands.sh`
exit 0; `scripts/check-cli-coverage.py` COVERED.

## Step 10 — gates, deploy, verify

```bash
gofmt -w internal/ cmd/ && go build ./... && go vet ./... && go test ./...
make e2e                       # report the real count, not the remembered one
scripts/budget.sh bin/kindred  # ≤220 MiB — For You adds one pool walk, bound it
bash scripts/deploy.sh         # the ONLY deploy path
bash scripts/check-deploy.sh http://127.0.0.1:8010
bash scripts/check-provenance.sh
```

Extend `check-deploy.sh` in the same commit as step 6: assert the generic
view renders unauthenticated (hrefs taken from the page, never
hand-constructed) and — if the deploy env carries
`KINDRED_OWNER_SESSION` — the owner view for `/` (cookie sent by the gate).
Deploy sets `KINDRED_OWNER_SESSION` in the environment
(`deploy/kindred.service` Environment= line; it is loopback-only, so the
secret-in-unit trade-off is explicit and acceptable — note it in DECISIONS).

**Verification**: all gates green; live spot-checks: `/` owner-shaped shows
For You; `/tag/{id}` note names liked-seeds; `?personal=0` everywhere matches
today's site.

## Ordering, and why

Identity (2) before engine (4) because the engine step's verification needs a
principal to rank for. Engine (4) before web assembly (5) because the signal
must exist to be fed. Persistence (3) before seeds (5) because For You draws
the rated list. Pages last (6–7) because every one of them is a thin consumer
of `tasteFor`. The privacy gate (8) sits after the pages it guards, the docs
(9) after the behaviour they describe, deploy (10) last — each step's
verification command is the reason the next step can trust its inputs.

Steps 1–5 are one sitting; 6 and 7 are each independently deployable behind
their own commit; 8–10 ship together.

## If a step fails

- **Taste is swamped in the step-4 measurement** → fix `Taste`'s value
  normalisation (L1), re-measure; do NOT rescale the other signals' weights to
  compensate — that is the handoff's exact warning.
- **For You is slow on the real corpus** → the seed walk (20 seeds) plus one
  pool query is two queries; if it exceeds that, the bug is in the helper, not
  the budget. Measure before caching anything.
- **e2e fixture lacks bookmark rows** → `internal/testcorpus` grows a second
  user; the hermetic suite must be able to express the owner shape or the
  owner path is untested.
- **`check-deploy.sh` cannot hold the owner cookie** → assert the generic
  view only, and verify the owner view by hand on the live box; say so in the
  gate's output rather than letting a skipped check read as a pass.

## Out of scope, on purpose

Accounts/login (D1 replaces them), JavaScript (hard constraint), multi-owner,
AO3 account sync, changing `internal/collab`'s algorithm, deleting any page.
The generic view is a feature, not a fallback: it is what makes the site
shippable to anyone else at all.
