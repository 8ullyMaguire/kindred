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
HTML and updates the database). While `serve` runs, a background
auto-crawler grows it too: see §5.5. The `embed` subcommand computes tag
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

- **Format**: Item-item co-bookmark matrix built from `user_work_interactions`, storing
  normalized cosine similarity of reader overlaps.
- **Contents**: For each work, a list of neighbouring work IDs and the
  similarity score (float32), where similarity is computed as:
  $$\text{Similarity}(A, B) = \frac{|U_A \cap U_B|}{\sqrt{|U_A| \times |U_B|}}$$
  ($U_A$ = set of users who bookmarked work A). This penalizes globally
  ubiquitous works and prioritizes taste-based matching.
- **Size**: ~23 MB (as measured on the live mirror).
- **Access**: O(1) to get the neighbour list for a work.
- **Note**: Built by the `internal/collab` package and wired into the
  ranking pool at serve startup. The similarity values are precomputed
  and normalized during index building.

### 2.6 Web UI changes

To reflect the personalized taste-based recommendations, the web UI is updated as follows:

* The default sort on `/recommend` and `/profile/{id}` is changed from `Relevance` to `Taste Match`.
* A new slider is added in the Filter Sidebar: `[ Niche Gems ───•─── Global Hits ]` which controls the popularity dampening exponent.
* The collaborative evidence in the "Why this?" section is updated to show taste overlap: 
  "Highly aligned with your reader cohort (94% taste overlap, 12 bookmarks)."

### 2.7 Default Blocked Tags

The following tags are blocked by default in the configuration to avoid certain content:
- "M/M"
- Slash
- Gay
- Romance
- "Hurt/Comfort"
- Fluff
- Angst
- "Slow Burn"
- RPF
- "Real Person Fiction"
- Reader
- Omega
- "Needs a Hug"
- Bestiality

These tags can be overridden by the user in the URL or in their profile.
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

Fetches AO3 work pages into the mirror (the original networked subcommand).
Used to update the mirror in bulk.

- Flags:
  - `--corpus`: path to the corpus SQLite database (required).
  - `--urls`: file of work URLs to fetch, one per line (`-` for stdin).
  - `--seeds`: comma-separated `kind:id` or AO3 work URLs to seed from.
  - `--state`: resume file; a crawl interrupted mid-run continues from it.
  - `--workers`: concurrent fetchers (default 1: the crawl delay binds).
  - `--max-retries`, `--retry-backoff`: transient-failure retries.
  - `--checkpoint-urls`: persist resume state every N fetches (default 25).
  - `--crawl-delay`: override the delay; `-1` (default) reads robots.txt,
    `0` means no wait (fixtures only).
  - `--offline`: make no network request at all; report every URL as
    unfetched, with the reason.
  - `--base-url`, `--parse-only`: fixture override; fetch-and-parse without
    writing.

#### serve's auto-crawler

`serve` grows the mirror while it is used, by default; `--no-crawl` turns
it off. The rule that survives from the original design is that the
**request path never makes an outbound request**: a missing or incomplete
work is queued with one INSERT into `crawl_queue` (state database), and a
background goroutine drains the queue.

- Inputs: a work a reader requested that the mirror lacks (the 404 case),
  a stored work viewed with no summary, and — at startup — up to 500 works
  whose stored metadata is missing a summary.
- Checkpoint: one row per work. `done_at` marks a work finished; a restart
  continues from exactly the rows not yet done. Per-work, not per-batch:
  a batch resume file would redo the finished half.
- Skip rule: existing metadata is never refetched — checked against the
  mirror *before* any request.
- Politeness: one fetch per the crawl delay robots.txt states (30s; an
  unreadable robots.txt keeps 30s rather than dropping to zero), retries
  with backoff, a job parked after five failed attempts (a fresh reader
  request re-arms it).

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
- The API's request path makes no outbound network requests; the only
  networked code paths are the `crawl` subcommand and `serve`'s background
  auto-crawler (same client, same robots.txt crawl delay, `--no-crawl` to
  disable), both of which only fetch from AO3.
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