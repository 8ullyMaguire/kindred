# Kindred Arena Ranking Functionality - Handoff

## Overview
This document outlines the current state of the Arena ranking functionality in the kindred codebase, what has been implemented, and what remains to be done for another AI agent to extend or verify the work.

## Current State (as of commit: latest)

### What is Working
1. **Core Arena Logic** (internal/arena package):
   - Glicko-2 rating system with four pairing strategies (max-uncertainty, max-expected, max-info, explore)
   - Batch rating updates, decay, and cold-start damping
   - Per-user tag weights learned from pairwise choices
   - Thread-safe store with five tables: comparisons, presentations, sessions, leaderboard cache, tag weights
   - HTTP API endpoints under `/api/v1/arena/*` (pair, judge, leaderboard, rank, my-ranking, batch)

2. **Web Interface** (internal/web package):
   - New templates: `arena.html`, `leaderboard.html`, `rank.html`, `myranking.html`
   - Routes registered: 
     - GET `/arena` -> renders a pairing for judgment
     - GET `/leaderboard` -> shows top works by rating
     - GET `/rank/<id>` -> shows detailed rating and history for a work
     - GET `/my-ranking` -> shows user's learned tag preferences (liked/disliked)
     - POST `/arena/judge` -> processes a comparison judgment and redirects to next pair
   - Navigation links added to base layout: Arena, Rankings, Your ranking
   - Templates use the existing AO3 2.0 theme (no Tailwind, pure CSS)
   - Arena judgment form uses radio buttons (works without JavaScript)
   - Session persistence via cookie (`kindred_arena`) shared between web and API

3. **Build and Deployment**:
   - Code builds successfully with `go build ./...`
   - Static binary produced (~14 MB) with flags `-s -w`, CGO_ENABLED=0
   - Dependencies: chi v5, modernc.org/sqlite, x/crypto
   - UI assets served via Go `embed`
   - Deployed on thinkcentre as a systemd unit, reachable at
     `kindred.polarisocial.xyz` (proxy 8009 → 8010)
   - Measured: budget gate 37 MiB of a 220 MiB cap; live service peaks at
     181 MiB of 220 after 25 arena page hits, zero panics, zero restarts

### What is Not Working / Missing

1. **API documentation** for the arena endpoints is not in the main API spec.

2. **The batch cadence is a judgement call.** The timer runs every 10 minutes
   (`deploy/kindred-arena-batch.timer`). A Glicko period is the set of games
   since the last one and the batch rule fits many games, so this trades
   period quality against how fast a reader sees their judgement count. One
   line to change.

3. **Test coverage gaps**: no browser-driven test, and no test that runs
   against the real 1.7 GB mirror — which is where every defect so far was
   found. `internal/web` has no test file of its own; page tests live in
   `internal/api` because that is where the fixture is.

4. `TestHealthzReportsOK` fails under `-race` on a clean tree. Pre-existing,
   confirmed by stashing all of this work. Untouched.

### Now wired, and worth knowing

`peer_rating` (SPEC §1's named signal) scores candidates by the arena's damped
rating, at default weight 0.11. It is registered **unconditionally**: with an
empty arena it returns `rank.ErrSkip` so `meta.degraded[]` names it, which is
SPEC §7.1 and the exact kindling failure (an arena signal inert in production
and silent about it). A signal added only when it has data is a signal absent
without saying so.

A batch reloads the engine's ratings under an RWMutex, so a period changes
recommendations immediately. The timer POSTs to the running server rather than
invoking a second binary, for the same reason.

### File Changes Summary
```
internal/web/arena.go        # NEW: web handlers for arena pages and judge endpoint
internal/web/render.go       # Updated: added arena templates to template cache
internal/web/handlers.go     # Updated: added arena routes and judge POST handler
internal/web/assets/layout.html  # Updated: added nav links to Arena, Rankings, Your ranking
internal/web/assets/arena.html   # NEW: arena pair template (judgment form)
internal/web/assets/leaderboard.html # NEW: leaderboard template
internal/web/assets/rank.html      # NEW: individual work ranking template
internal/web/assets/myranking.html # NEW: user's learned preferences template
internal/web/assets/style.css    # Updated: added CSS classes for arena components
internal/store/arena.go          # Updated: candidate pool orders by `hits`, not `comments`
internal/api/arena.go            # NEW: JSON API handlers
internal/api/arena_handlers.go   # NEW: arena API wiring
internal/api/arena_web_test.go   # NEW: page tests for the four arena pages
```

## Actionable Steps for Another AI Agent

### 1. Verify the Implementation

   **The corpus is NOT on the development host.** It is 1.7 GB and lives only
   on thinkcentre at `/home/alvaro/kindling-data/ao3_metadata.db`. Running the
   server locally will fail with `unable to open database file (14)`. Verify
   there:

   ```bash
   make build
   scp bin/kindred thinkcentre:/tmp/kindred-test
   ssh thinkcentre '/tmp/kindred-test serve \
     --corpus /home/alvaro/kindling-data/ao3_metadata.db \
     --db /tmp/kindred-test.db --listen 127.0.0.1:8012 --mode lite &'
   ```

   Then walk the arena, and note that a leaderboard is empty until a **batch
   has run** — judging alone records comparisons but rates nothing:

   ```bash
   curl -s localhost:8012/arena            # a pair, two cards
   curl -s -X POST localhost:8012/api/v1/arena/batch   # rates the period
   curl -s localhost:8012/leaderboard      # now populated
   ```

   The live service is already deployed and reachable at
   `https://kindred.polarisocial.xyz`.

### 2. Run the Test Suite
   - `make test` or `go test ./...` — all 14 packages pass.
   - Arena coverage: `internal/arena` (arithmetic), `internal/api/arena_web_test.go`
     (the four pages), `internal/api/arena_batch_test.go` (the batch write path).
   - The batch path had **no** tests until `arena_batch_test.go` was added, and
     three defects lived there. If you add arena behaviour, test the write, not
     just the arithmetic — that boundary is where every bug so far has been.

### 3. Check Memory Usage
   ```bash
   scripts/budget.sh bin/kindred <corpus> <db> 8013 full
   ```
   Exits non-zero over the cap. Last run on the real corpus: 37 MiB of 220,
   PASS, arena steps included.

   **Do not trust `routes.txt`** — it claims to be this gate's source of truth
   and nothing reads it. `scripts/budget.sh` has its own hardcoded probe list,
   so a route listed in `routes.txt` and absent from the script is outside the
   budget. Add the probe to the script.

### 4. Extend the Functionality (if desired)
   - **Wire into Recommender**: 
     - Edit `internal/engine/engine.go` to call the arena store when the tune flag is enabled
     - Use the learned tag weights to adjust recommendation scores
   - **Add More Pairing Strategies**:
     - The arena package already supports four strategies; add more in `internal/arena/pairing.go`
   - **Enhance UI**:
     - Add avatars or thumbnails for works (if available in corpus)
     - Show a sparkline of rating history on the leaderboard
     - Add a "random pair" button for exploration
   - **Improve API**:
     - Add pagination to `/api/v1/arena/leaderboard`
     - Add endpoints for resetting a user's arena session

### 5. Deploy to Thinkcentre (if access is available)
   - The systemd service expects the binary at `/usr/local/bin/kindred`
   - After building, copy the binary and restart the service:
     ```bash
     sudo cp bin/kindred /usr/local/bin/kindred
     sudo systemctl restart kindred
     ```
   - Verify via `https://kindred.polarisocial.xyz/arena`

## Notes
- The arena is designed to be a standalone feature that can be enabled/disabled via configuration (though no config flag exists yet; it is always compiled in).
- All data is sanitized; no user IDs, work IDs, or tag IDs are exposed in public snapshots.
- The implementation follows the existing codebase patterns: dependency injection via `Deps`, error handling with `d.fail`, and template rendering with `d.page`.

## Open Questions
1. Should the arena be gated by a configuration flag (e.g., `arena.enabled`)? 
2. Should the judge endpoint return JSON for AJAX use (while keeping the form fallback)?
3. Should the leaderboard support filtering by tag (like the main site does)?

These questions are left for the next agent to decide based on product goals.

---
*This handoff was generated automatically from the current session state. For detailed reasoning, see the session history.*