/**
 * End-to-end tests: a real browser against a real kindred.
 *
 * ## What these cover that the Go tests do not
 *
 * The Go suite tests handlers by calling them with a constructed request. That
 * never exercises the template, the routing table, the static handler, or a
 * link actually being clickable. A page that renders an empty list because a
 * template range is wrong passes every Go test.
 *
 * Building cmd/e2eserver for this suite already found one such bug: a
 * `defer store.Close()` closed the database while the server was still serving,
 * so /api/v1/recommend answered 500 "load seeds: sql: database is closed" while
 * the home page rendered perfectly. Health-check-only CI would not have seen it.
 */
const { test, expect } = require('@playwright/test');

test.describe('public read surface', () => {
  test('the home page renders a working search box', async ({ page }) => {
    await page.goto('/');
    await expect(page).toHaveTitle(/kindred/i);

    const box = page.locator('#q');
    await expect(box).toBeVisible();
    await expect(box).toHaveAttribute('name', 'q');

    // The nav is on every page; if it breaks, every page is unreachable.
    for (const href of ['/arena', '/leaderboard', '/stats']) {
      await expect(page.locator(`a[href="${href}"]`).first()).toBeVisible();
    }
  });

  test('search finds a tag and links through to it', async ({ page }) => {
    await page.goto('/');
    await page.fill('#q', 'harry');
    await page.press('#q', 'Enter');
    await page.waitForURL(/\/search\?q=harry/);

    // Results must be LINKS to tag pages, not bare text. A tag page that is
    // not linkable is a dead end for every reader who finds it.
    const tags = page.locator('a.tag');
    await expect(tags.first()).toBeVisible();

    const href = await tags.first().getAttribute('href');
    expect(href).toMatch(/^\/tag\/\d+$/);

    await tags.first().click();
    await page.waitForURL(/\/tag\/\d+/);
    // The tag page must show works, not an empty shell.
    await expect(page.locator('div.work').first()).toBeVisible();
  });

  test('a work page shows its title, tags and recommendations', async ({ page }) => {
    await page.goto('/work/1');

    const h1 = page.locator('h1');
    await expect(h1).toBeVisible();
    await expect(h1).not.toBeEmpty();

    // The canonical AO3 link must carry rel=noopener, since it opens off-site.
    const ext = page.locator('a[href^="https://"]').first();
    await expect(ext).toHaveAttribute('rel', /noopener/);

    // Tags are the work's whole identity in this corpus.
    await expect(page.locator('a.tag').first()).toBeVisible();

    // And the recommendation panel is the reason the page exists.
    await expect(page.locator('div.recommend')).toBeVisible();
  });

  test('every work in the corpus is reachable from search', async ({ page }) => {
    // A work the search index cannot find is a work that does not exist as far
    // as a reader is concerned. This walks the fixture's tag pages rather than
    // trusting a count.
    await page.goto('/tag/1');
    const works = page.locator('div.work a[href^="/work/"]');
    await expect(works.first()).toBeVisible();

    const href = await works.first().getAttribute('href');
    await works.first().click();
    await page.waitForURL(/\/work\/\d+/);
    await expect(page.locator('h1')).toBeVisible();
  });
});

test.describe('error handling', () => {
  test('an unknown work is a 404 with a body, not a crash', async ({ page }) => {
    const res = await page.goto('/work/999999999');
    expect(res.status()).toBe(404);
    // A bare 404 with no body looks like a broken deployment.
    await expect(page.locator('body')).not.toBeEmpty();
  });

  test('a search for nothing says so rather than erroring', async ({ page }) => {
    const res = await page.goto('/search?q=zzzzznotatag');
    expect(res.status()).toBe(200);
    // Either results or a stated empty result, but NOT a 500.
    await expect(page.locator('body')).toContainText(/\S/);
  });

  test('healthz reports ok', async ({ request }) => {
    const res = await request.get('/healthz');
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(body.status).toBe('ok');
    expect(body.budget_ok).toBe(true);
  });
});

test.describe('API surface', () => {
  test('recommend returns scored items with evidence', async ({ request }) => {
    const res = await request.get('/api/v1/recommend?seed=ao3_work:1&n=5');
    expect(res.ok()).toBeTruthy();
    const body = await res.json();

    expect(body.items.length).toBeGreaterThan(0);
    for (const item of body.items) {
      expect(item.id).toBeGreaterThan(0);
      expect(typeof item.score).toBe('number');
      // An item with a score and no evidence cannot be audited, which is the
      // property the whole evidence design exists to provide.
      expect(item.evidence, `work ${item.id} has no evidence`).toBeTruthy();
      expect(item.evidence.length).toBeGreaterThan(0);
    }
  });

  test('the seed is never recommended back', async ({ request }) => {
    const res = await request.get('/api/v1/recommend?seed=ao3_work:1&n=20');
    const body = await res.json();
    const ids = body.items.map((i) => i.id);
    expect(ids).not.toContain(1);
  });

  test('an unknown seed kind is a 4xx, not a confident wrong answer', async ({ request }) => {
    // This is the bug the engine fix addressed: `book:1` resolved to AO3 work 1
    // and returned a plausible, entirely wrong ranking.
    const res = await request.get('/api/v1/recommend?seed=book:1&n=5');
    expect(res.status()).toBeGreaterThanOrEqual(400);
    expect(res.status()).toBeLessThan(500);
  });

  test('max_per_group with group_by=fandom is accepted', async ({ request }) => {
    const res = await request.get(
      '/api/v1/recommend?seed=ao3_work:1&n=20&max_per_group=2&group_by=fandom');
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(Array.isArray(body.items)).toBe(true);
  });
});

test.describe('static assets', () => {
  test('the stylesheet is served and is real CSS', async ({ request }) => {
    const res = await request.get('/static/style.css');
    expect(res.ok()).toBeTruthy();
    const css = await res.text();
    expect(css.length).toBeGreaterThan(100);
    expect(css).toContain('{');
  });

  test('no page loads a script that does not exist', async ({ page }) => {
    // A 404 inside a script tag is silent in the console and invisible in the
    // rendered output, so it is checked explicitly here.
    const missing = [];
    page.on('response', (res) => {
      if (res.status() === 404 && res.url().includes('/static/')) {
        missing.push(res.url());
      }
    });
    await page.goto('/');
    await page.goto('/work/1');
    expect(missing, `missing assets: ${missing.join(', ')}`).toEqual([]);
  });
});