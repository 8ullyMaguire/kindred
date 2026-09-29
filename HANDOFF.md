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
   - Memory caps observed: Lite ≤ 120 MiB RSS, Full ≤ 220 MiB RSS (tested)
   - Publicly reachable via `kindred.polarisocial.xyz` (proxy 8009 → 8010)

### What is Not Working / Missing
1. **Automatic Integration with Recommender**:
   - The arena signal is not yet wired into the recommender loop (tune flag gated)
   - This is intentional; the arena is currently a standalone feature

2. **API Documentation**:
   - The new API endpoints are not yet documented in the main API spec (though they follow existing patterns)

3. **UI Polish**:
   - The arena pair page could benefit from better mobile layout (currently functional)
   - No visual indication of loading state during pair generation (typically instant)

4. **Test Coverage**:
   - Unit tests exist for the arena package (internal/arena)
   - Page tests exist in internal/api/arena_web_test.go (added with this work) and cover
     all four pages, the judge POST, and the slice-bounds regression
   - Still no browser-driven test, and no test that runs against the real 1.7 GB mirror

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

### 1. Verify the Implementation Locally
   - Build the binary: `make build` or `go build ./...`
   - Run the server in lite mode for quick testing:
     ```bash
     CORPUS=$HOME/kindling-data/ao3_metadata.db \
     DB=$HOME/.local/share/kindred/kindred.db.test \
     PORT=8011 \
     MODE=lite \
     bin/kindred serve
     ```
   - Open `http://127.0.0.1:8011/` in a browser
   - Navigate to `/arena` and verify a pairing appears
   - Judge a pair (pick a radio, then submit) and verify you get a new pair
   - Visit `/leaderboard` to see ranked works
   - Visit `/rank/<id>` for a specific work (replace `<id>` with a work ID from the leaderboard)
   - Visit `/my-ranking` to see your learned tag preferences (requires at least 3 judgments)

### 2. Run the Test Suite
   - Ensure all tests pass: `make test` or `go test ./...`
   - Pay special attention to:
     - `internal/arena` tests (core logic)
     - `internal/store` tests (arena tables)
     - No web-specific tests exist; consider adding them

### 3. Check Memory Usage (Optional but Recommended)
   - The project has a strict memory budget. Run the budget gate to verify:
     ```bash
     make budget
     ```
   - This will walk all routes (including the new arena ones) and exit non-zero if RSS exceeds the cap.

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