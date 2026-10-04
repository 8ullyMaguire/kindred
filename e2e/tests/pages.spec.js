/**
 * The reader-facing pages added for ao3-recommender parity.
 *
 * ## Why this file exists, and what it cost to write it
 *
 * The first pass of this work added six pages (/recommend with diversity
 * controls, /fandoms, /underrated, /neighbours, /profiles, /profile) and
 * reported the suite green. All 13 existing Playwright tests passed, because
 * all 13 exercised the OLD pages. Five of the six new pages had zero coverage.
 *
 * Then these tests were written and `/neighbours?tag=dark` answered:
 *
 *   500  wrong type for value; expected int64; got int
 *
 * `html/template` resolves field types at RENDER time. So `go build`, `go vet`
 * and the entire Go suite were green, and the page was broken in the browser.
 * The bug had been sitting in the tree since the page was written.
 *
 * That is the argument for this file: the Go render tests
 * (internal/web/render_pages_test.go) now cover the type errors, and THIS file
 * covers what only a browser can see — that the routes exist, the POST forms
 * actually persist, the nav reaches every page, and the controls change the
 * ranking.
 */
const { test, expect } = require('@playwright/test');

/** Every page a reader can reach, and the nav label that gets them there. */
const PAGES = [
  // heading is whether the page renders an <h1>. The home page passes an empty
  // heading on purpose (d.base("kindred", "") -- the brand is already in the
  // header, and an <h1> repeating it would be noise for a screen reader), so
  // the assertion cannot be blanket "every page has an h1". The TITLE element
  // is the invariant instead: the layout always writes it.
  { path: '/', link: '/', name: 'home', title: /kindred/i, heading: false },
  { path: '/recommend', link: '/recommend', name: 'recommend', title: /kindred/i, heading: true },
  { path: '/fandoms', link: '/fandoms', name: 'fandoms', title: /Fandoms/i, heading: true },
  { path: '/underrated', link: '/underrated', name: 'underrated', title: /Underrated/i, heading: true },
  { path: '/neighbours', link: '/neighbours', name: 'neighbours', title: /neighbours/i, heading: true },
  { path: '/profiles', link: '/profiles', name: 'profiles', title: /profiles/i, heading: true },
  // The arena family. These were in the nav and covered by no test, which is
  // the same gap as the six parity pages: a route nothing visits cannot be
  // broken by a change nobody makes.
  { path: '/arena', link: '/arena', name: 'arena', title: /arena/i, heading: true },
  { path: '/leaderboard', link: '/leaderboard', name: 'leaderboard', title: /leaderboard|rankings/i, heading: true },
  { path: '/my-ranking', link: '/my-ranking', name: 'my-ranking', title: /ranking/i, heading: true },
  { path: '/block', link: '/block', name: 'block', title: /block/i, heading: true },
];

test.describe('every reader page renders', () => {
  for (const p of PAGES) {
    test(`${p.name} answers 200 with real content`, async ({ page }) => {
      const res = await page.goto(p.path);
      expect(res.status(), `${p.path} status`).toBe(200);

      // The title is what every page is required to set, and a 500 does not.
      await expect(page).toHaveTitle(p.title);

      // A page that renders an empty body looks exactly like a page that
      // failed to find its template, so the body is asserted to have substance.
      await expect(page.locator('#main')).not.toBeEmpty();

      if (p.heading) {
        await expect(page.locator('h1')).toBeVisible();
      } else {
        await expect(page.locator('h1')).toHaveCount(0);
      }

      // The one thing a 500 never has: the nav, because the error page
      // replaces the layout.
      await expect(page.locator('#nav')).toBeVisible();
    });
  }
});

test.describe('the nav reaches every page', () => {
  test('each reader page is linked from the nav', async ({ page }) => {
    await page.goto('/');
    for (const p of PAGES) {
      // `.first()` because a page can be linked from more than one place, and
      // strict mode failing on that would be a false alarm.
      await expect(
        page.locator(`#nav a[href="${p.link}"]`).first(),
        `no nav link to ${p.link}`,
      ).toBeVisible();
    }
  });

  test('a nav link is actually clickable through to the page', async ({ page }) => {
    // Presence in the DOM is not reachability: a link under a collapsed
    // element, or one that 404s, both pass a visibility check on the <a>.
    await page.goto('/');
    await page.locator('#nav a[href="/fandoms"]').first().click();
    await page.waitForURL(/\/fandoms/);
    await expect(page.locator('#nav')).toBeVisible();
    await expect(page.locator('input[name="min_co_works"]')).toBeVisible();
  });
});

test.describe('the arena family', () => {
  test('/rank/<id> names the work and links to it', async ({ page }) => {
    // RankPage carried Title and WorkURL and the template printed neither, so
    // the page showed a number for a work the reader could not identify. It is
    // the same accepted-and-ignored shape as the Python sibling's crawl:
    // the data was there and nothing used it.
    const res = await page.goto('/rank/1');
    expect(res.status()).toBe(200);

    // The work must be identifiable and openable.
    await expect(page.locator('.rating-head a[href^="/work/"]')).toBeVisible();

    // And the uncertainty must be on the page: a rating without its deviation
    // is a number the reader over-trusts.
    // The template writes &plusmn;, which renders as U+00B1 (+/-). Escaped as
    // a character class: a bare /+/-/ is an unterminated regex, not a
    // two-character match.
    await expect(page.locator('.rating-head')).toContainText(/[\u00b1+-]/);
  });

  test('/rank/<not-an-id> is a 404, not a 500', async ({ page }) => {
    const res = await page.goto('/rank/not-a-number');
    expect(res.status()).toBe(404);
  });

  test('the arena offers a comparison or says why it cannot', async ({ page }) => {
    await page.goto('/arena');
    const cards = page.locator('form[aria-label*="judge" i], .arena-card, .card');
    const empty = page.locator('.empty');

    if (await cards.count()) {
      await expect(cards.first()).toBeVisible();
    } else {
      // An exhausted arena pool is a normal outcome. It must be STATED --
      // an empty form looks like a page that failed to load two works.
      await expect(empty.first()).toBeVisible();
    }
  });
});

test.describe('the diversity controls change the ranking', () => {
  test('the cap control is on the recommend form and reaches the query', async ({ page }) => {
    await page.goto('/recommend?seed=ao3_work:1');

    const cap = page.locator('input[name="max_per_fandom"]');
    await expect(cap, 'the diversity cap must be on the form').toBeVisible();
    await expect(cap).toHaveAttribute('min', '0');

    // Fill it in and submit the FORM, rather than crafting a URL. A control
    // that renders but does not submit is invisible to a URL-only test, and
    // that is the same accepted-and-ignored shape as the Python sibling's
    // --max-per-fandom flag.
    await cap.fill('2');
    await page.locator('select[name="group_by"]').selectOption('fandom');
    // Scoped to the recommend form: the layout puts a search form with its own
    // submit on every page, so a bare input[type=submit] is ambiguous and
    // Playwright refuses it in strict mode. Which is correct -- a selector that
    // matches two things has not said which one it meant.
    await page
      .locator('form[aria-label="Recommendation controls"] input[type="submit"]')
      .click();
    await page.waitForURL(/max_per_fandom=2/);

    // The value must survive the round trip, or the reader set a control and
    // the next page quietly shows a different one.
    await expect(page.locator('input[name="max_per_fandom"]')).toHaveValue('2');
    await expect(page.locator('select[name="group_by"]')).toHaveValue('fandom');
  });

  test('an out-of-range cap is refused, not silently clamped', async ({ page }) => {
    // A silently clamped cap is a cap the reader did not ask for, and it makes
    // the list shorter than the number on screen.
    const res = await page.goto(
      '/recommend?seed=ao3_work:1&max_per_fandom=99999');
    expect(res.status()).toBe(400);
  });

  test('a bad group axis is refused with a message naming the axis', async ({ page }) => {
    const res = await page.goto(
      '/recommend?seed=ao3_work:1&group_by=nonsense');
    expect(res.status()).toBe(400);
    await expect(page.locator('body')).toContainText(/group_by|fandom/i);
  });

  test('too many seeds are dropped and the page says so', async ({ page }) => {
    // Six seeds; the cap is five. The notice is the whole point: a reader who
    // typed six must be told the sixth did nothing.
    const seeds = [1, 2, 3, 4, 5, 6].map((i) => `seed=ao3_work:${i}`).join('&');
    await page.goto(`/recommend?${seeds}`);
    await expect(page.locator('[data-testid="seed-limit"]')).toBeVisible();
  });

  test('a short list explains itself rather than looking complete', async ({ page }) => {
    await page.goto('/recommend?seed=ao3_work:1&n=20&max_per_fandom=1&group_by=fandom');

    const count = page.locator('[data-testid="count"]');
    const empty = page.locator('[data-testid="empty"]');

    if (await empty.isVisible().catch(() => false)) {
      // An empty list must say WHY, and must point at the cap as a cause.
      await expect(empty).toContainText(/Nothing scored above zero/);
      await expect(empty).toContainText(/diversity cap/);
    } else {
      // A short list must account for the difference. "Showing 3 of 20" with
      // no explanation reads as a complete answer.
      await expect(count).toContainText(/Showing \d+ of 20/);
      await expect(count).toContainText(/cap|corpus supports/);
    }
  });
});

test.describe('corpus analysis pages', () => {
  test('fandoms ranks fandoms with a visible evidence gate', async ({ page }) => {
    await page.goto('/fandoms');
    await expect(page.locator('input[name="min_co_works"]')).toBeVisible();

    // The gate must be explained where it is set, because lowering it makes
    // the ranking WORSE (one-work fandoms outrank genuinely liked ones) and
    // that is the opposite of what a threshold suggests.
    await expect(page.locator('body')).toContainText(/evidence floor|noise/i);
  });

  test('underrated applies its filters to the query, and says the pool narrowed', async ({ page }) => {
    await page.goto('/underrated');
    await expect(page.locator('input[name="min_words"]')).toBeVisible();
    await expect(page.locator('input[name="complete"]')).toBeVisible();

    // Turning a filter on must produce a note saying the CANDIDATE POOL was
    // narrowed, not that rows were dropped. Those are different claims and the
    // page must not imply the wrong one.
    await page.goto('/underrated?min_words=1');
    await expect(page.locator('body')).toContainText(/candidate pool narrowed/i);
  });

  test('neighbours for a tag shows the tag frequency and its neighbours', async ({ page }) => {
    // This is the exact request that returned 500 while every Go test passed.
    const res = await page.goto('/neighbours?tag=dark');
    expect(res.status(), 'neighbours with a tag').toBe(200);

    // The tag's own frequency is the denominator a PMI figure needs: 0.4 over
    // 12 works and 0.4 over 12,000 are not the same claim.
    await expect(page.locator('body')).toContainText(/appears on/);
  });

  test('neighbours with no tag offers the form rather than erroring', async ({ page }) => {
    const res = await page.goto('/neighbours');
    expect(res.status()).toBe(200);
    await expect(page.locator('input[name="tag"]')).toBeVisible();
    await expect(page.locator('[data-testid="empty"]')).toBeVisible();
  });

  test('a tag name with spaces and ampersands produces a working link', async ({ page }) => {
    // Fandom and tag names contain spaces, slashes and ampersands. An unescaped
    // href renders as a link that goes somewhere else, which is worse than a
    // 404 because it looks right.
    await page.goto('/fandoms');
    const links = page.locator('table a[href^="/search?q="]');
    const n = await links.count();
    if (n > 0) {
      const href = await links.first().getAttribute('href');
      expect(href, 'a search link must be percent-escaped').not.toContain(' ');
    }
  });
});

test.describe('taste profiles round-trip through the browser', () => {
  // One name for the whole block: these mutate shared state, so they must not
  // race each other, and a stale profile from a previous run must not make a
  // later assertion pass for the wrong reason.
  const PROFILE = 'e2e-reader';

  test('building a profile persists it and lists it', async ({ page }) => {
    await page.goto('/profiles');

    await page.fill('input[name="name"]', PROFILE);
    await page.fill('input[name="works"]', '1,2,3');
    await page
      .locator('form[aria-label="Build a taste profile"] input[type="submit"]')
      .click();

    // The page must show the new profile. A POST that redirects to a page
    // without the new row in it means the write did not happen -- which is
    // exactly the "reports success, wrote nothing" bug from the Python
    // sibling's crawl.
    await expect(page.locator(`a[href^="/profile?name="]`).first()).toBeVisible();
    await expect(page.locator('[data-testid="profile-rows"]')).toContainText(PROFILE);
  });

  test('the built profile has weights, so it is not an empty shell', async ({ page }) => {
    // A profile with no tags is stored successfully and ranks nothing. The page
    // has to distinguish the two, and this test pins the distinction.
    await page.goto(`/profile?name=${PROFILE}`);
    await expect(page.locator('[data-testid="profile-tags"]')).toBeVisible();
    const rows = await page.locator('[data-testid="profile-tags"] tbody tr').count();
    expect(rows, 'a profile built from 3 works must have tag weights').toBeGreaterThan(0);
  });

  test('rating a work moves weights and shows which ones moved', async ({ page }) => {
    await page.goto(`/profile?name=${PROFILE}`);
    const before = await readWeights(page);

    await page.fill('input[name="work"]', '2');
    await page.selectOption('select[name="rating"]', 'like');
    await page.locator('form[aria-label="Rate a work"] input[type="submit"]').click();

    await expect(page.locator('[data-testid="rated"]')).toBeVisible();

    // The confirmation must NAME the weights that moved, not just say "done".
    // A rating that moves nothing is the accepted-and-ignored shape again.
    const moved = page.locator('[data-testid="rated"] code');
    expect(await moved.count(), 'no moved tags reported').toBeGreaterThan(0);

    const after = await readWeights(page);
    expect(after, 'the weight table did not change after a rating')
      .not.toEqual(before);
  });

  test('rating a work that is not in the corpus says so, and does not 500', async ({ page }) => {
    await page.goto(`/profile?name=${PROFILE}`);
    await page.fill('input[name="work"]', '999999999');
    await page.selectOption('select[name="rating"]', 'like');
    await page.locator('form[aria-label="Rate a work"] input[type="submit"]').click();

    // A reader-fixable mistake gets a message on the page, not a 500.
    await expect(page.locator('[data-testid="err"]')).toBeVisible();
  });

  test('an unknown profile is a 404 that explains itself', async ({ page }) => {
    const res = await page.goto('/profile?name=definitely-not-a-profile');
    expect(res.status()).toBe(404);
    await expect(page.locator('[data-testid="err"]')).toContainText(/no profile/i);
  });

  test('a build with no works is refused, not silently accepted', async ({ page }) => {
    // Two layers, both worth pinning. The `works` input carries `required`, so
    // the browser refuses to submit at all; and if that guard is ever removed
    // the server must still refuse. The second half posts directly, because a
    // browser that will not submit cannot test what the server does.
    await page.goto('/profiles');
    await page.fill('input[name="name"]', 'e2e-empty');
    await page.fill('input[name="works"]', '');
    await page
      .locator('form[aria-label="Build a taste profile"] input[type="submit"]')
      .click();

    // Still on /profiles, and nothing was created.
    await expect(page.locator('input[name="name"]')).toHaveValue('e2e-empty');

    const res = await page.request.post('/profiles', {
      form: { name: 'e2e-empty', works: '' },
    });
    expect(res.status()).toBe(200);
    await expect(page.locator('body')).toContainText(/work id|at least one/i);
  });
});

test.describe('method discipline', () => {
  test('POST is allowed only on the two profile pages', async ({ request }) => {
    // Everything else is read-only. A POST that mutates state from a
    // URL a reader can be linked to is a CSRF surface, so the gate is
    // asserted rather than assumed.
    for (const path of ['/', '/search?q=x', '/work/1', '/fandoms', '/underrated']) {
      const res = await request.post(path, { data: {} });
      expect(res.status(), `POST ${path}`).toBe(405);
    }
  });

  test('a POST to /profiles without a name explains what is missing', async ({ request }) => {
    const res = await request.post('/profiles', { data: {} });
    expect(res.status()).toBe(200);
    const body = await res.text();
    expect(body).toMatch(/name/i);
  });
});

/** Reads the profile's weight table as a comparable shape. */
async function readWeights(page) {
  const rows = page.locator('[data-testid="profile-tags"] tbody tr');
  const out = [];
  for (let i = 0; i < (await rows.count()); i++) {
    const cells = rows.nth(i).locator('td');
    out.push([
      (await cells.nth(1).innerText()).trim(),
      (await cells.nth(2).innerText()).trim(),
    ]);
  }
  return JSON.stringify(out);
}
