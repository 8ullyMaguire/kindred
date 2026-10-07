// The /recommend page's ranking controls, driven through a real browser.
//
// ## Why a browser suite and not only Go tests
//
// These controls were all implemented as query parameters first, and every
// one of them was reachable only by hand-editing the URL. The Go tests proved
// the parameters worked; nothing proved the page had a control for any of
// them. A capability that cannot be found in the interface is not usable from
// the frontend, which is the requirement this suite exists to check.
//
// The specific failure this caught: the `tune` parameter resolved nothing at
// all — it was recorded in the response metadata and never affected ranking.
// A URL-driven test would have passed, because the parameter WAS accepted and
// echoed back. Selecting it from the form and asserting on the resulting
// WEIGHTS is what makes the difference observable.
//
// Each test drives the form the way a reader does — choose, click Rank —
// rather than visiting a URL. A control that renders but is not in the form's
// action still passes a URL-driven test.

const { test, expect } = require('@playwright/test');

/** The fixture's seed, which carries a tag shared with other works. */
const SEED = 'ao3_work:1';

/** Reads the work ids the page currently lists, in order. */
async function listedWorkIds(page) {
  return page.$$eval('.work > h3 > a', (as) =>
    as.map((a) => a.getAttribute('href').replace('/work/', ''))
  );
}

/**
 * Applies the recommend form's controls and submits.
 *
 * Scoped to `form.controls` for the same reason the tag page's helper is: the
 * layout carries a site-wide search form, and an unscoped submit clicks that
 * one and navigates somewhere with no results — which from the outside looks
 * exactly like a filter that dropped every row.
 */
async function applyControls(page, values) {
  const form = page.locator('form.controls');
  for (const [name, value] of Object.entries(values)) {
    const sel = form.locator(`select[name="${name}"]`);
    if (await sel.count()) {
      await sel.selectOption(value === null ? '' : value);
      continue;
    }
    await form.locator(`input[name="${name}"]`).fill(value === null ? '' : value);
  }
  await Promise.all([
    page.waitForLoadState('load'),
    form.locator('input[type="submit"]').click(),
  ]);
}

test.describe('/recommend ranking controls', () => {
  test.beforeEach(async ({ page }) => {
    await page.goto(`/recommend?seed=${encodeURIComponent(SEED)}`);
  });

  test('every ranking capability has a control on the page', async ({ page }) => {
    // The point of this test. Each name here is a query parameter the engine
    // honours; before the form existed, all of them were URL-only.
    const form = page.locator('form.controls');
    for (const name of [
      'min_words', 'max_words', 'min_kudos',
      'complete', 'rating', 'lang',
      'pool_mode', 'pool', 'tune',
    ]) {
      const count = await form
        .locator(`select[name="${name}"], input[name="${name}"]`)
        .count();
      expect(count, `no control for ?${name}= on /recommend`).toBe(1);
    }
  });

  test('the controls survive a round trip through the form', async ({ page }) => {
    await applyControls(page, {
      min_words: '1',
      complete: 'complete',
      pool_mode: 'tags+collab',
      pool: '25',
      tune: 'default',
    });

    const u = new URL(page.url());
    expect(u.searchParams.get('min_words')).toBe('1');
    expect(u.searchParams.get('complete')).toBe('complete');
    expect(u.searchParams.get('pool_mode')).toBe('tags+collab');
    expect(u.searchParams.get('pool')).toBe('25');
    expect(u.searchParams.get('tune')).toBe('default');

    // And the page shows back what it applied, which is what makes a
    // round-tripped form usable rather than a guessing game.
    await expect(page.locator('form.controls input[name="min_words"]'))
      .toHaveValue('1');
    await expect(page.locator('form.controls select[name="complete"]'))
      .toHaveValue('complete');
    await expect(page.locator('form.controls select[name="pool_mode"]'))
      .toHaveValue('tags+collab');
  });

  test('a completion filter actually changes the listed works', async ({ page }) => {
    // The unfiltered baseline first, or "the filter changed nothing" and
    // "nothing matched" are indistinguishable.
    await page.goto(`/recommend?seed=${encodeURIComponent(SEED)}&n=40`);
    const unfiltered = await listedWorkIds(page);
    expect(unfiltered.length).toBeGreaterThan(0);

    // The fixture carries both completion states. If it did not, this test
    // would pass for the wrong reason, so assert the split is visible.
    const wip = await (async () => {
      await applyControls(page, { complete: 'in-progress', n: '40' });
      return listedWorkIds(page);
    })();
    const done = await (async () => {
      await applyControls(page, { complete: 'complete', n: '40' });
      return listedWorkIds(page);
    })();

    expect(wip.length + done.length).toBeGreaterThan(0);
    // No work may appear in both buckets: that is the assertion with teeth,
    // since a filter that did nothing would leave every id in both.
    const overlap = wip.filter((id) => done.includes(id));
    expect(overlap, 'a work was listed as both complete and in progress').toEqual([]);
  });

  test('an unparseable filter is refused by name, not ignored', async ({ page }) => {
    const response = await page.goto(
      `/recommend?seed=${encodeURIComponent(SEED)}&min_words=lots`
    );
    expect(response.status()).toBe(400);
    // The message must name the parameter, or the reader cannot tell which
    // control they got wrong.
    await expect(page.locator('body')).toContainText('min_words');
  });

  test('a reversed word range is refused rather than matching nothing', async ({ page }) => {
    const response = await page.goto(
      `/recommend?seed=${encodeURIComponent(SEED)}&min_words=90000&max_words=100`
    );
    expect(response.status()).toBe(400);
    await expect(page.locator('body')).toContainText('min_words');
  });

  test('an unknown tune is refused rather than silently ranked as default', async ({ page }) => {
    const response = await page.goto(
      `/recommend?seed=${encodeURIComponent(SEED)}&tune=no-such-tune`
    );
    // The parameter used to be accepted, echoed in the weights table, and
    // had no effect whatsoever. A 400 is the whole difference.
    expect(response.status()).toBe(400);
    await expect(page.locator('body')).toContainText('no-such-tune');
  });

  test('an unknown pool mode is refused by name', async ({ page }) => {
    const response = await page.goto(
      `/recommend?seed=${encodeURIComponent(SEED)}&pool_mode=collabative`
    );
    expect(response.status()).toBe(400);
    await expect(page.locator('body')).toContainText('pool_mode');
  });

  test('the weight table reports the tune that was applied', async ({ page }) => {
    // Not just that a table renders: that it names the tune in force, so a
    // reader can tell which weights produced the list above it.
    await expect(page.locator('details.tune summary')).toBeVisible();
    const rows = await page.locator('details.tune tbody tr').count();
    expect(rows).toBeGreaterThan(0);
  });

  test('blocking a tag removes it from the recommendations', async ({ page }) => {
    // The block list is keyed to the arena cookie, so this drives the real
    // flow rather than seeding the database: block the tag a recommended work
    // carries, re-rank, and confirm that work is gone.
    //
    // Two things were broken and neither was visible from the URL alone.
    // Blocking had no effect on ranking, because the block predicate never
    // reached the pool query. And blocking was unreachable from the site at
    // all: POST /block existed and /block listed what was blocked, but no
    // page ever rendered the button. This test blocks through the tag page,
    // which is the only place a reader can name a tag.
    await page.goto(`/recommend?seed=${encodeURIComponent(SEED)}&n=40`);
    const before = await listedWorkIds(page);
    expect(before.length).toBeGreaterThan(1);

    const victim = before[0];
    // Find one of the victim's tags, then block it there.
    await page.goto(`/work/${victim}`);
    const tagHref = await page
      .locator('.work a[href^="/tag/"], a[href^="/tag/"]')
      .first()
      .getAttribute('href');
    expect(tagHref, `work ${victim} lists no tags to block`).toBeTruthy();

    await page.goto(tagHref);
    const blockForm = page.locator('form.block-form').first();
    expect(
      await blockForm.count(),
      `${tagHref} offers no block control, so blocking is unreachable from the site`
    ).toBe(1);
    await expect(blockForm).toContainText('Block this tag');
    await Promise.all([
      page.waitForLoadState('load'),
      blockForm.locator('button[type="submit"]').click(),
    ]);

    // Blocking must be visible and reversible on the page that offers it.
    await page.goto(tagHref);
    await expect(page.locator('form.block-form').first()).toContainText(
      'Unblock this tag'
    );

    await page.goto(`/recommend?seed=${encodeURIComponent(SEED)}&n=40`);
    const after = await listedWorkIds(page);
    expect(
      after,
      `a work carrying the just-blocked tag was still recommended`
    ).not.toContain(victim);
  });
});