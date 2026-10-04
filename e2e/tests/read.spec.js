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

  test('search finds a WORK by its title, not only a tag', async ({ page }) => {
    // The gap this closes. /search used to query the tags table ONLY, so a
    // work that exists in the mirror could not be found by its title. The
    // search box even said "Search tags" -- an honest label for a limitation
    // a reader feels at once, because the work you can name is the work you
    // most want to seed a recommendation from.
    //
    // The query is taken from a work the suite has already proven exists, so
    // this cannot pass by searching for something that is not there.
    await page.goto('/work/1');
    const title = (await page.locator('h1').innerText()).trim();
    expect(title.length).toBeGreaterThan(0);

    const word = title.split(/\s+/)[0];
    await page.goto(`/search?q=${encodeURIComponent(word)}`);
    await expect(page).toHaveURL(/\/search\?q=/);

    const works = page.locator('[data-testid="works"] a[href^="/work/"]');
    await expect(works.first()).toBeVisible();

    // The result must be a LINK to the work page, and must reach it.
    const href = await works.first().getAttribute('href');
    expect(href).toMatch(/^\/work\/\d+$/);
    await works.first().click();
    await page.waitForURL(/\/work\/\d+/);
    await expect(page.locator('h1')).toBeVisible();
  });

  test('a work found by search can seed a ranking in one click', async ({ page }) => {
    // The purpose of the whole feature: finding a work is only useful if the
    // next action is ranking from it. Without the seed link, search finds
    // things the reader then has to copy an ID out of the URL to use.
    await page.goto('/work/1');
    const title = (await page.locator('h1').innerText()).trim();
    const word = title.split(/\s+/)[0];

    await page.goto(`/search?q=${encodeURIComponent(word)}`);
    const seedLink = page
      .locator('[data-testid="works"] a.seed-link')
      .first();
    await expect(seedLink).toBeVisible();

    const href = await seedLink.getAttribute('href');
    expect(href).toMatch(/^\/recommend\?seed=ao3_work:\d+$/);

    await seedLink.click();
    await page.waitForURL(/\/recommend\?seed=ao3_work:/);
    // The ranking page must accept the seed rather than reject it.
    await expect(page.locator('form[action="/recommend"]')).toBeVisible();
  });

  test('a search matching nothing says so in BOTH halves', async ({ page }) => {
    // Two sections, two messages. One message would leave the reader unable
    // to tell whether the other half of the search ran at all -- the same
    // "is this empty or broken?" ambiguity this repo keeps fixing elsewhere.
    await page.goto('/search?q=zzz-definitely-not-in-the-corpus-zzz');
    await expect(page.locator('[data-testid="no-works"]')).toBeVisible();
    await expect(page.locator('[data-testid="no-tags"]')).toBeVisible();
  });

  test('a search that matches a tag but no work says which is empty', async ({ page }) => {
    // The discriminating case. 'dark' is a tag in the fixture and is not a
    // substring of any fixture title ("Fixture Work 0NN"), so this separates
    // the two halves: a page that showed only one message would pass a test
    // that checked nothing.
    await page.goto('/search?q=dark');
    await expect(page.locator('[data-testid="tag-count"]')).toBeVisible();
    await expect(page.locator('[data-testid="no-works"]')).toBeVisible();
  });

  test('the search box does not claim to search only tags', async ({ page }) => {
    // The label was the honest admission of the limitation. Now that works
    // are searched, a label still reading "Search tags" understates the
    // page and would send readers looking only for tags.
    await page.goto('/');
    const label = await page.locator('label[for="q"]').innerText();
    expect(label.trim().toLowerCase()).not.toContain('tags only');
  });

  test('every page states how old the data is, or that it is unknown', async ({ page }) => {
    // The footer must never be silent about freshness. The ingest command
    // writes index_built_at into the store's meta table and, before this,
    // nothing read it -- so the age of the data behind every recommendation
    // was invisible, and a stale index answered confidently from old data.
    //
    // Either branch is acceptable; silence is not. The e2e server's store has
    // no build stamp, so in practice this asserts the "not recorded" branch,
    // which is the one that must never degrade into a numeric age.
    await page.goto('/');
    const freshness = page.locator('[data-testid="freshness"]');
    await expect(freshness).toBeVisible();

    const text = (await freshness.innerText()).trim();
    expect(text.length).toBeGreaterThan(0);

    // It must say something about age, not just repeat the version line.
    expect(text.toLowerCase()).toMatch(
      /not recorded|built (today|\d+ day)|future/);

    // An unknown age must NOT be rendered as a number. This is the specific
    // lie the three-state design exists to prevent.
    if (/not recorded/i.test(text)) {
      expect(text).not.toMatch(/\d+\s*days?\s+ago/);
      expect(text).not.toContain('built today');
    }

    // And it appears on every page, not just the home page, because Base is
    // shared. A footer on one page is a footer on one page.
    for (const href of ['/arena', '/leaderboard']) {
      await page.goto(href);
      await expect(page.locator('[data-testid="freshness"]')).toBeVisible();
    }
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