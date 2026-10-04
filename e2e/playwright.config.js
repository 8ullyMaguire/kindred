/**
 * Playwright config for kindred's end-to-end tests.
 *
 * ## Why this config exists
 *
 * The repo had no browser test at all. HANDOFF.md named that as gap #3
 * ("no browser-driven test"), and the engine coverage note recorded that
 * `internal/engine` and `internal/web` were the two untested packages — so the
 * surface a reader actually touches was unverified by anything that executes
 * a real HTTP request against a real template render.
 *
 * It has already paid for itself: building cmd/e2eserver surfaced a
 * `defer store.Close()` that closed the database out from under a live server,
 * giving 500s of "load seeds: sql: database is closed" on a server whose home
 * page still rendered. No Go test covered that, because no Go test called
 * Recommend against a running server.
 *
 * ## Hermetic by construction
 *
 * The webServer boots `cmd/e2eserver` over a 40-work fixture corpus built by
 * `internal/testcorpus` — the same corpus the Go tests use. No network, no
 * 1.7 GB fixture, no 30-second crawl delay. The real corpus lives only on
 * thinkcentre and takes ~100s just to index, which is why a browser suite
 * against it could never run in CI.
 *
 * It is still the real product: the real router, the real handlers, the real
 * templates, the real store, a real index build. Only the corpus size is fake.
 *
 * ## The port is FIXED, deliberately
 *
 * The server listens on 8731 rather than :0. Playwright needs a URL up front
 * to set baseURL and to probe readiness, and an ephemeral port written to a
 * ready file cannot serve either — the earlier version of this config combined
 * `listen :0` with a fixed baseURL, which only worked by accident and would
 * fail the moment two checkouts ran at once.
 *
 * 8731 is outside the live deployment range (8010 is kindred, 8006 concord)
 * so a test run cannot collide with a server the user actually has running.
 */
const { defineConfig, devices } = require('@playwright/test');
const path = require('path');
const os = require('os');

const PORT = process.env.KINDRED_E2E_PORT || 8731;
const BASE = process.env.KINDRED_E2E_URL || `http://127.0.0.1:${PORT}`;
const scratch = process.env.KINDRED_E2E_DIR ||
  path.join(os.tmpdir(), 'kindred-e2e');

// The repo root, for `go build ./cmd/e2eserver`.
//
// webServer.command runs with cwd = the config's directory (e2e/), so a
// relative ./cmd/e2eserver resolves to e2e/cmd/e2eserver and fails with
// "directory not found". Absolute in, absolute out.
const repo = path.resolve(__dirname, '..');

module.exports = defineConfig({
  testDir: path.join(__dirname, 'tests'),
  // A browser test that hangs is worse than one that fails: CI cannot tell them
  // apart without a timeout, and a timeout on a whole suite says nothing about
  // which page hung.
  timeout: 30_000,
  expect: { timeout: 7_000 },
  fullyParallel: false,
  // One worker: every test hits the SAME server process, and the arena tests
  // write shared rows. Parallel workers would race each other and produce
  // failures that are about the harness rather than the product.
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: 0,
  reporter: process.env.CI ? [['github'], ['list']] : [['list']],

  use: {
    baseURL: BASE,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    // The pages are server-rendered with no client-side framework, so there is
    // nothing to wait for beyond the load event. A networkidle wait here would
    // be slower and no more reliable.
  },

  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
  ],

  webServer: {
    // Built on demand so a stale binary can never be what the suite tested,
    // and the fixture directory is cleared so every run indexes from scratch
    // rather than inheriting a previous run's state.db.
    command: `rm -rf ${scratch} && mkdir -p ${scratch} && ` +
      `go build -o ${path.join(scratch, 'e2eserver')} ${path.join(repo, 'cmd/e2eserver')} && ` +
      `${path.join(scratch, 'e2eserver')} -dir ${scratch} -works 40 ` +
      `-listen 127.0.0.1:${PORT}`,
    url: `${BASE}/healthz`,
    reuseExistingServer: !process.env.CI,
    // Indexing 40 works is fast; the budget is for a cold Go build cache.
    timeout: 120_000,
    stdout: 'pipe',
    stderr: 'pipe',
  },
});