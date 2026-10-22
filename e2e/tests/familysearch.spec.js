'use strict';

// The family intersection — "recent works in THIS fandom that are THIS
// concept" — folded from the deleted /browse page into /search.
//
// Two things are tested, and they are different claims:
//   1. the capability still works (a pair returns works, and says how large
//      each pool was), and
//   2. the surface is actually gone (/browse is a 404, no nav entry points at
//      it), because a fold that leaves the old route alive is a move, not a
//      removal.
//
// The corpus is the 40-work testcorpus: "harry potter - j. k. rowling" is a
// declared spelling of the harry potter family and "alternate universe -
// canon divergence" is a declared tag of the alternate universe family, so
// the pair is guaranteed to intersect if the query is wired at all. A test
// whose fixture has no intersection would pass on an empty implementation.

const { test, expect } = require('@playwright/test');

// A window wide enough for the fixture's dates (its update dates sit months
// back from "now"), narrow enough to still be a real filter.
const FAMILY_URL =
  '/search?universe=harry+potter&concept=alternate+universe&recent=730';

test.describe('the family search, folded into /search', () => {
  test('the form is on the search page and it is a GET to /search', async ({ page }) => {
    await page.goto('/search');

    const form = page.locator('form[aria-label="Find works in two tag families"]');
    await expect(form).toBeVisible();
    // A GET, so every result is a URL a reader can bookmark or share.
    await expect(form).toHaveAttribute('method', 'get');
    await expect(form).toHaveAttribute('action', '/search');

    // The controls are populated from the declared vocabulary, not hardcoded
    // in the template: a reader cannot name a family the form does not offer.
    const universes = page.locator('#universe option');
    expect(await universes.count()).toBeGreaterThan(1);
    await expect(page.locator('#universe option[value="worm"]')).toHaveCount(1);
    await expect(page.locator('#concept option[value="isekai"]')).toHaveCount(1);
  });

  test('submitting the form returns works and states the pool sizes', async ({ page }) => {
    await page.goto(FAMILY_URL);

    const counts = page.locator('[data-testid="family-counts"]');
    await expect(counts).toBeVisible();
    // The counts are the difference between "no overlap" and "no data": a
    // reader seeing zero rows must be able to see that both pools had works.
    await expect(counts).toContainText(/fandom pool/);
    await expect(counts).toContainText(/concept pool/);
    await expect(counts).toContainText(/730 days/);

    const works = page.locator('[data-testid="family-works"] .work');
    expect(await works.count()).toBeGreaterThan(0);

    // Every row links to its work page, so the section is not decoration.
    const href = await works.first().locator('h3 a').getAttribute('href');
    expect(href).toMatch(/^\/work\/\d+$/);

    // A family-only request must not render the TEXT sections: they would
    // say "no work matches ''" — a claim about a search nobody made.
    await expect(page.locator('[data-testid="works"]')).toHaveCount(0);
    await expect(page.locator('[data-testid="no-works"]')).toHaveCount(0);
  });

  test('the form submits through the browser and keeps the selection', async ({ page }) => {
    await page.goto('/search');
    await page.selectOption('#universe', 'harry potter');
    await page.selectOption('#concept', 'alternate universe');
    await page.selectOption('#recent', '730');
    // Scoped to THIS form: the layout's header search box is an earlier form
    // on the page with its own submit, and an unscoped selector submitted it
    // instead (the URL came back /search?q=). A passing test here has to know
    // which of the page's two forms it pressed.
    await Promise.all([
      page.waitForNavigation(),
      page.locator('form[aria-label="Find works in two tag families"] input[type=submit]').click(),
    ]);

    expect(page.url()).toContain('universe=harry+potter');
    expect(page.url()).toContain('concept=alternate+universe');
    // The selection survives the round trip, so the form reflects the result
    // it produced rather than resetting to "any".
    await expect(page.locator('#universe')).toHaveValue('harry potter');
    await expect(page.locator('#concept')).toHaveValue('alternate universe');
    await expect(page.locator('[data-testid="family-counts"]')).toBeVisible();
  });

  test('a family pair can be narrowed by a text search without losing it', async ({ page }) => {
    // q is carried as a hidden field: adding a family to a text search must
    // narrow it, not silently drop the words the reader typed.
    await page.goto(FAMILY_URL + '&q=anything');
    await expect(page.locator('input[type=hidden][name=q]')).toHaveAttribute('value', 'anything');
    await expect(page.locator('[data-testid="family-counts"]')).toBeVisible();
  });

  test('an unknown family is refused by name', async ({ page }) => {
    await page.goto('/search?universe=not-a-family');
    const err = page.locator('[data-testid="family-error"]');
    await expect(err).toBeVisible();
    await expect(err).toContainText('unknown universe');
    // Named, not silently ignored: a filter that cannot fire and returns
    // everything is the accepted-and-ignored shape this project refuses.
    await expect(err).toContainText('harry potter');
  });

  test('/browse is gone and nothing links to it', async ({ page }) => {
    const res = await page.goto('/browse');
    expect(res.status(), '/browse status').toBe(404);

    await page.goto('/search');
    await expect(page.locator('a[href="/browse"]')).toHaveCount(0);
    // The nav is the place a stale link would survive.
    await expect(page.locator('nav a[href="/browse"]')).toHaveCount(0);
  });
});
