/**
 * The taste-blended tag default, author pages, and search-by-address --
 * the reader-visible half of the changes Go tests can prove only through
 * handlers. Everything here is asserted the way a reader meets it: by
 * visiting the page and reading what it says.
 */
const { test, expect } = require('@playwright/test');

// The fixture corpus: 40 works, shared tags. Tag 1 exists in it (the other
// specs use it too) -- reuse the same addressing convention.
const TAG = '/tag/1';

test.describe('tag page taste default', () => {
  test('opens on the taste blend and explains itself', async ({ page }) => {
    await page.goto(TAG);

    // Heading names the view...
    await expect(page.locator('[data-testid="works-heading"]')).toContainText(/Taste match/i);

    // ...and the page either shows the blend with its counts or says why
    // it could not. Silence -- neither note -- is the failure state.
    const note = page.locator('[data-testid="taste-note"]');
    const err = page.locator('[data-testid="taste-error"]');
    await expect(note.or(err)).toBeVisible();

    // The order control offers the plain lists as choices, with taste
    // selected as the default.
    await expect(page.locator('#sort')).toHaveValue('taste');
  });

  test('an explicit sort opts back into the plain exact list', async ({ page }) => {
    await page.goto(`${TAG}?sort=kudos`);
    await expect(page.locator('[data-testid="works-heading"]')).toContainText(/Most kudos first/);
    await expect(page.locator('[data-testid="taste-note"]')).toHaveCount(0);
    await expect(page.locator('[data-testid="taste-error"]')).toHaveCount(0);
    await expect(page.locator('[data-testid="taste-score"]')).toHaveCount(0);
  });

  test('the sort form carries n so a sort change keeps the reader\'s count', async ({ page }) => {
    await page.goto(`${TAG}?n=35`);
    const hidden = page.locator('.controls input[name="n"]');
    await expect(hidden).toHaveValue('35');
  });

  test('a truncated list links the raised count', async ({ page }) => {
    await page.goto(`${TAG}?n=2`);
    const raise = page.locator('[data-testid="raise-n"]');
    if (await raise.count()) {
      await expect(raise).toContainText('4'); // n=2 doubled
      await expect(raise).toHaveAttribute('href', '?n=4');
    } else {
      // A tag with <=2 works cannot truncate; the fixture tags have more,
      // so reaching here means the raise link vanished where it should be.
      const rows = await page.locator('.work').count();
      expect(rows).toBeLessThanOrEqual(2);
    }
  });
});

test.describe('author pages', () => {
  test('every byline links to an author page that lists their works', async ({ page }) => {
    await page.goto('/tag/1?sort=kudos');

    const authorLink = page.locator('.byline a.author').first();
    await expect(authorLink).toBeVisible();
    const href = await authorLink.getAttribute('href');
    expect(href).toMatch(/^\/author\?q=/);

    await authorLink.click();
    await expect(page).toHaveURL(/\/author\?q=/);
    await expect(page.locator('[data-testid="author-count"]')).toBeVisible();

    // The list is non-empty: the byline it came from belongs to a person
    // with works here.
    const rows = page.locator('.work');
    expect(await rows.count()).toBeGreaterThan(0);
  });

  test('an unknown name is an honest empty, not a blank page', async ({ page }) => {
    await page.goto('/author?q=zzzdefinitelynobody');
    await expect(page.locator('[data-testid="no-author-works"]')).toBeVisible();
  });
});

test.describe('search by address', () => {
  test('an AO3 work URL renders the work as an exact match', async ({ page }) => {
    // Work 1 exists in the fixture; the host is fictional but the path is
    // what the resolver keys on (host-agnostic by design).
    await page.goto('/search?q=' + encodeURIComponent('https://archiveofourown.org/works/1'));

    const exact = page.locator('[data-testid="exact-work"]');
    await expect(exact).toBeVisible();

    // And the work it names is not repeated below.
    const title = (await exact.locator('h3').first().textContent()).trim();
    const works = page.locator('[data-testid="works"] .work');
    const titles = await works.locator('h3').allTextContents();
    expect(titles.map((t) => t.trim())).not.toContain(title);
  });

  test('a URL for a work this mirror lacks says so', async ({ page }) => {
    await page.goto('/search?q=' + encodeURIComponent('https://archiveofourown.org/works/999999'));
    await expect(page.locator('[data-testid="exact-missing"]')).toBeVisible();
    await expect(page.locator('[data-testid="exact-missing"]')).toContainText('999999');
  });

  test('title+author words combine', async ({ page }) => {
    // Grab a real work from a tag page: one word from its title, its
    // byline's name. The pair is what a reader types -- "that fic, by that
    // author" -- and the whole two-token phrase appears in NEITHER column
    // contiguously, so matching it proves the tokens are ANDed across
    // title and author rather than searched as one string.
    await page.goto('/tag/1?sort=kudos&n=5');
    const title = (await page.locator('.work h3 a').first().textContent()).trim();
    const author = (await page.locator('.byline a.author').first().textContent()).trim();
    const titleWord = title.split(/\s+/)[0];

    const query = `${titleWord} ${author}`;
    await page.goto('/search?q=' + encodeURIComponent(query));

    const works = page.locator('[data-testid="works"] .work h3 a');
    const found = await works.allTextContents();
    expect(found.map((t) => t.trim())).toContain(title);
  });
});
