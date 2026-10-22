/**
 * /algo — two algorithms, one seed, side by side.
 *
 * ## Why this file exists
 *
 * The page shipped with Go tests for its handlers and its template, and
 * goal-check.py's page-coverage clause immediately failed on it: no file in
 * e2e/tests requested `/algo`. The same clause caught /mirror and /crawl
 * earlier, and it is the only check in the repo that sees a route registered
 * but never visited — a page nothing requests cannot be broken by a change
 * nobody makes.
 *
 * It found more than a missing test. The page's registry entry carried
 * `Nav: "/algo"`, which marks it current when you are on it, and layout.html
 * had NO link to it: the page was reachable only by typing the URL. That is
 * invisible to a Go render test, because the page renders perfectly — the
 * defect is in the layout on every OTHER page. The first test here clicks
 * through from the nav for that reason.
 *
 * ## What only a browser can see
 *
 * The two columns are rendered inside `{{if .Left}}`. A comparison that
 * returns zero candidates therefore falls through to the "name a work"
 * prompt and looks like a page that did nothing — with a 200, no error, and
 * an entirely green template test. So the columns are asserted to be
 * POPULATED, not merely present.
 *
 * The vote is a plain form POST that 303s back. /arena's control was once a
 * fetch handler and was inert with scripting off; asserting the redirect
 * target rather than the POST's status is what pins the shape.
 */
const { test, expect } = require('@playwright/test');

// The e2e server indexes a 40-work fixture corpus, so a real AO3 id does not
// exist in it. Work 1 is the fixture's seed; every spec reaches for the same
// one.
const SEED = 'ao3_work:1';

// Seeded by cmd/e2eserver at startup. A fresh state db holds no tunes, and
// with only "default" registered no comparison can be performed at all.
const OTHER = 'quality-lean';

test.describe('/algo', () => {
  test('is reachable by clicking its nav link', async ({ page }) => {
    await page.goto('/');
    const link = page.locator('#nav a[href="/algo"]').first();
    await expect(link, 'the page sets Nav: "/algo" but layout.html has no link to it')
      .toBeVisible();
    await link.click();
    await page.waitForURL(/\/algo$/);

    const res = await page.goto('/algo');
    expect(res.status()).toBe(200);
    await expect(page.locator('#main')).not.toBeEmpty();
  });

  test('the untouched form offers every stored algorithm and says what to do', async ({ page }) => {
    await page.goto('/algo');

    // Both selects come from the registry, so a tune stored with
    // `kindred tune set` appears without a code change — the claim the page
    // makes in its own hint text.
    for (const name of ['a', 'b']) {
      const select = page.locator(`select[name="${name}"]`);
      await expect(select).toBeVisible();
      await expect(select.locator('option[value="default"]')).toHaveCount(1);
      await expect(select.locator(`option[value="${OTHER}"]`)).toHaveCount(1);
    }

    // No seed: the empty prompt, not a 404 and not a blank page.
    await expect(page.locator('[data-testid="empty"]')).toBeVisible();
    await expect(page.locator('[data-testid="algo-a"]')).toHaveCount(0);
  });

  test('two algorithms rank the same seed into two populated columns', async ({ page }) => {
    const res = await page.goto(`/algo?seed=${SEED}&a=default&b=${OTHER}&n=5`);
    expect(res.status(), 'a comparison must not 500 on the fixture').toBe(200);

    const left = page.locator('[data-testid="algo-a"]');
    const right = page.locator('[data-testid="algo-b"]');
    await expect(left).toBeVisible();
    await expect(right).toBeVisible();

    // Populated, not merely present: this is the assertion the
    // {{if .Left}} gate makes load-bearing.
    await expect(left.locator('ol.algo-items li')).toHaveCount(5);
    await expect(right.locator('ol.algo-items li')).toHaveCount(5);

    // Each column names the algorithm that ranked it, so a reader knows which
    // list is which without counting columns.
    await expect(left.locator('h3')).toHaveText('default');
    await expect(right.locator('h3')).toHaveText(OTHER);

    // The delta line: what moved, and how much the lists agree. Two columns of
    // fic with no statement of what changed is a preference test whose answer
    // cannot be explained.
    const delta = page.locator('[data-testid="algo-delta"]');
    await expect(delta).toContainText('quality');
    await expect(delta).toContainText('recency');
    await expect(delta).toContainText(/of 5 works appear in both lists/);

    // The weights are meant to produce a DIFFERENT order. quality-lean carries
    // no overlap or neighbourhood weight at all, so identical lists would mean
    // the tune was not applied -- the failure this page exists to make visible.
    //
    // This assertion is only sound because the comparison ranks on relevance
    // ALONE (engine.Request.Lambda = 1). Measured before that: the MMR
    // diversifier ran on both columns and returned the same five works in the
    // same order for these two very different tunes, because its novelty
    // penalty is in the score's own units and the two tunes' scores differ in
    // magnitude. The page was showing the diversifier and calling it the
    // algorithm.
    const leftTitles = await left.locator('ol.algo-items a').allTextContents();
    const rightTitles = await right.locator('ol.algo-items a').allTextContents();
    expect(leftTitles).not.toEqual(rightTitles);

    // Each column is in its own weight order, so its printed scores never rise.
    // A column that rises has been re-ordered after scoring, which means the
    // reader is no longer looking at the algorithm.
    for (const [label, column] of [['left', left], ['right', right]]) {
      const scores = (await column.locator('span.score').allTextContents()).map(Number);
      expect(scores, `${label} column has no scores`).not.toHaveLength(0);
      for (let i = 1; i < scores.length; i += 1) {
        expect(
          scores[i],
          `${label} column prints ${JSON.stringify(scores)}: item ${i} outranks item ${i - 1}`,
        ).toBeLessThanOrEqual(scores[i - 1]);
      }
    }

    // Every item links to a work, or the list names works a reader cannot open.
    await expect(left.locator('ol.algo-items a[href^="/work/"]')).toHaveCount(5);
  });

  test('a vote is recorded and the page says which side won', async ({ page }) => {
    await page.goto(`/algo?seed=${SEED}&a=default&b=${OTHER}&n=3`);

    // The BUTTON, not the form: clicking a <form> lands on its padding and
    // submits nothing. /arena taught this the expensive way.
    await page
      .locator('form.algo button[type="submit"][name="choice"][value="b"]')
      .click();
    await page.waitForLoadState('domcontentloaded');

    // A 303 back to the comparison, still comparing the same two algorithms --
    // a redirect that dropped the query would land the reader on an empty form
    // and look like the vote was rejected.
    await expect(page).toHaveURL(/judged=b/);
    await expect(page).toHaveURL(new RegExp(`a=default`));
    await expect(page).toHaveURL(new RegExp(`b=${OTHER}`));

    await expect(page.locator('[data-testid="algo-judged"]')).toContainText(
      `you preferred ${OTHER}`,
    );
    // And the comparison is still on the page, so the vote did not consume it.
    //
    // Five, not the three the URL asked for: the vote form carries seed, a and
    // b and no n, so the redirect lands on the page's own default. Asserting
    // three would pin a promise the form never made.
    await expect(page.locator('[data-testid="algo-a"] ol.algo-items li')).toHaveCount(5);
  });

  test('an algorithm cannot be compared with itself', async ({ page }) => {
    const res = await page.goto(`/algo?seed=${SEED}&a=default&b=default`);
    // A named refusal rather than two identical columns: the lists WOULD be
    // identical, so the page would be asking for a judgement about nothing.
    expect(res.status()).toBe(400);
    await expect(page.locator('[data-testid="algo-error"]')).toContainText(
      'cannot compare "default" with itself',
    );
    await expect(page.locator('[data-testid="algo-a"]')).toHaveCount(0);
  });

  test('an unknown algorithm is refused by name, not ranked by the defaults', async ({ page }) => {
    const res = await page.goto(`/algo?seed=${SEED}&a=default&b=does-not-exist`);
    expect(res.status()).toBe(400);
    await expect(page.locator('[data-testid="algo-error"]')).toContainText(
      'no algorithm named "does-not-exist"',
    );
    // The specific failure an unknown name must never produce: a column silently
    // ranked with the default weights and labelled with a name that does not
    // exist.
    await expect(page.locator('[data-testid="algo-b"]')).toHaveCount(0);
  });
});
