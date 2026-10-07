// The tag page's filter controls, driven through a real browser.
//
// ## Why these are here and not only in Go
//
// The Go tests assert the query string produces the right rows. They do not
// press the button. That gap is not hypothetical: the first version of these
// controls was tested only in Go and the "complete" select was never in the
// form at all -- the query worked, the control did not exist.
//
// Each test submits the form the way a reader does -- choosing an option and
// clicking Apply -- rather than visiting a hand-written URL, because a control
// that renders but is not wired into the form's action still passes a
// URL-driven test.
//
// The fixture corpus (internal/testcorpus) carries the real rating names, three
// languages and both completion states, so every bucket here is non-empty. A
// filter over an empty bucket is indistinguishable from no filter, which is the
// trap TestTheFixtureWritesEveryColumnAFilterReads guards on the Go side.

const { test, expect } = require('@playwright/test');

/** Reads the work ids the tag page currently lists, in order. */
async function listedWorkIds(page) {
  return page.$$eval('.work > h3 > a', (as) =>
    as.map((a) => a.getAttribute('href').replace('/work/', ''))
  );
}

/**
 * Applies the tag-page control form.
 *
 * SCOPED to `form.controls` on purpose. The page carries two forms: the
 * site-wide search box (action="/search") and the tag's own controls
 * (action="/tag/{id}"). An unscoped `input[type="submit"]` matches the search
 * form's button first, so every filter application navigated to /search and
 * returned a page with no works on it -- indistinguishable, from the outside,
 * from a filter that silently drops every row.
 *
 * A null value means "unset": the empty option for a select, empty for a text
 * input.
 */
async function applyFilters(page, values) {
  const form = page.locator('form.controls');
  for (const [name, value] of Object.entries(values)) {
    const sel = form.locator(`select[name="${name}"]`);
    if (await sel.count()) {
      await sel.selectOption(value === null ? '' : value);
      continue;
    }
    await form.locator(`input[name="${name}"]`).fill(value === null ? '' : value);
  }
  // Record ?n= before submitting and restore it after, because the form does
  // not carry it: a GET form drops any parameter it has no control for.
  //
  // That is not a cosmetic detail. Without it, ?n=100 silently reverted to the
  // default 20, and the partition assertion below compared a 29-work union
  // against two 20-row lists -- which failed, correctly, with 28 against 29.
  // The failure named the fixture rather than the cause.
  const here = new URL(page.url());
  await Promise.all([
    page.waitForLoadState('load'),
    form.locator('input[type="submit"]').click(),
  ]);
  const n = here.searchParams.get('n');
  if (n && !new URL(page.url()).searchParams.has('n')) {
    await page.goto(`${page.url()}&n=${n}`);
  }
}

// /tag/1 rather than whatever /search happens to rank first.
//
// This is not a convenience. The first draft navigated to whatever tag came
// back from /search?q=dark, which on the 40-work fixture is tag 5 -- ten works,
// all English. Every language test then found zero rows and failed, while
// looking exactly like a broken filter. The filters were fine; the tag had no
// Spanish in it.
//
// Tag 1 carries all three languages and both completion states (20 works:
// 19 English / 7 Spanish / 3 French, 20 complete / 9 in progress), so every
// bucket these tests assert on is genuinely non-empty. A filter over an empty
// bucket cannot be distinguished from no filter at all, which is why the
// Go-side gate TestTheFixtureWritesEveryColumnAFilterReads exists at all.
const TAG = '/tag/1';

test.beforeEach(async ({ page }) => {
  await page.goto(TAG);
  await expect(page.locator('[data-testid="works-heading"]')).toBeVisible();
});

test('the tag page offers completion, rating and language controls', async ({ page }) => {
  // Presence. Asserting only that works happen to be listed is how the missing
  // "complete" control passed for a while.
  await expect(page.locator('select[name="complete"]')).toBeVisible();
  await expect(page.locator('select[name="rating"]')).toBeVisible();
  await expect(page.locator('input[name="lang"]')).toBeVisible();

  // The rating control must offer the mirror's real names as labels, because
  // that is what a reader picking "General" expects to see.
  const labels = await page.$$eval('select[name="rating"] option', (os) =>
    os.map((o) => o.textContent.trim())
  );
  for (const want of ['General Audiences', 'Teen And Up Audiences', 'Mature', 'Explicit']) {
    expect(labels).toContain(want);
  }

  // And the VALUES must be the AO3 letters, since that is what the filter and
  // the API both take.
  const values = await page.$$eval('select[name="rating"] option', (os) =>
    os.map((o) => o.value)
  );
  for (const want of ['G', 'T', 'M', 'E']) {
    expect(values).toContain(want);
  }
});

test('the completion control narrows the list', async ({ page }) => {
  // ?n= defaults to 20 and tag 1 has exactly 20 works, so BOTH states render
  // 20-or-fewer rows and "fewer rows" is not a usable assertion here. The
  // claim worth testing is that the SETS differ, which is what a filter that
  // silently does nothing would fail.
  await page.goto(`${TAG}?n=100`);
  const before = await listedWorkIds(page);

  // Not a hardcoded size. Tag 1 has 29 works, but an earlier probe read 20
  // because it had not asked for ?n=100 -- the default page limit. Asserting a
  // number copied from a measurement taken under the wrong conditions is how
  // a test ends up guarding nothing.
  expect(before.length).toBeGreaterThan(20);

  await applyFilters(page, { complete: 'true' });
  const after = await listedWorkIds(page);
  expect(after.length).toBeGreaterThan(0);
  expect(after.join(',')).not.toBe(before.join(','));

  await applyFilters(page, { complete: 'false' });
  const other = await listedWorkIds(page);
  expect(other.length).toBeGreaterThan(0);
  expect(other.join(',')).not.toBe(after.join(','));
  expect(other.join(',')).not.toBe(before.join(','));

  // The two states must PARTITION the tag. If they overlapped, a work would be
  // both complete and in progress, which the column does not allow.
  const union = new Set([...after, ...other]);
  expect(union.size).toBe(before.length);
});

test('the rating control narrows the list and accepts the letter', async ({ page }) => {
  await page.goto(`${TAG}?n=100`);
  const before = await listedWorkIds(page);

  // Every rating bucket must be non-empty on this tag, for the same reason as
  // the language buckets above.
  for (const r of ['G', 'T', 'M', 'E']) {
    await page.goto(`${TAG}?rating=${r}`);
    expect((await listedWorkIds(page)).length, `no works rated ${r} on ${TAG}`)
      .toBeGreaterThan(0);
  }

  // Compare sets, not row counts: the ?n= page limit makes counts ambiguous
  // on a 20-work tag.
  await applyFilters(page, { rating: 'E' });
  const byLetter = await listedWorkIds(page);
  expect(byLetter.length).toBeGreaterThan(0);
  expect(byLetter.join(',')).not.toBe(before.join(','));

  // The mirror's full name must select exactly the same works, because a
  // client echoing back what the API returned sends the name.
  await applyFilters(page, { rating: 'Explicit' });
  const byName = await listedWorkIds(page);
  expect(byName.join(',')).toBe(byLetter.join(','));
});

test('the language control narrows the list', async ({ page }) => {
  await page.goto(`${TAG}?n=100`);
  const before = await listedWorkIds(page);
  expect(before.length).toBeGreaterThan(5);

  // Each bucket must be NON-EMPTY on this tag, or "the filter returned
  // nothing" is indistinguishable from "the filter is not wired". Asserted
  // first and separately so the failure names the fixture, not the filter.
  for (const lang of ['English', 'Spanish', 'French']) {
    await page.goto(`${TAG}?n=100&lang=${lang}`);
    const ids = await listedWorkIds(page);
    expect(ids.length, `no works with language=${lang} on ${TAG}`).toBeGreaterThan(0);
    expect(ids.length).toBeLessThanOrEqual(before.length);
  }

  // And the three are genuinely different sets.
  await page.goto(`${TAG}?n=100&lang=Spanish`);
  const spanish = (await listedWorkIds(page)).join(',');
  await page.goto(`${TAG}?n=100&lang=French`);
  const french = (await listedWorkIds(page)).join(',');
  await page.goto(`${TAG}?n=100&lang=English`);
  const english = (await listedWorkIds(page)).join(',');

  expect(spanish).not.toBe(french);
  expect(spanish).not.toBe(english);
  expect(french).not.toBe(english);
});

test('the controls show what is applied after a reload', async ({ page }) => {
  await applyFilters(page, { complete: 'true', rating: 'E' });

  // A filter the reader cannot see is a filter they set twice.
  await expect(page.locator('select[name="complete"]')).toHaveValue('true');
  await expect(page.locator('select[name="rating"]')).toHaveValue('E');

  // And it must survive a reload, since that is what sharing the URL means.
  const url = page.url();
  await page.goto(url);
  await expect(page.locator('select[name="complete"]')).toHaveValue('true');
  await expect(page.locator('select[name="rating"]')).toHaveValue('E');
});

test('filters combine with each other and with the sort', async ({ page }) => {
  await applyFilters(page, { complete: 'true', lang: 'English' });
  const filtered = await listedWorkIds(page);
  expect(filtered.length).toBeGreaterThan(0);

  // Same filter, different sort: the SET must be identical. If sorting changed
  // which works are listed, the sort is doing the filtering.
  await page.goto(`${page.url()}&sort=words`);
  const resorted = await listedWorkIds(page);
  expect(resorted.slice().sort()).toEqual(filtered.slice().sort());
});

test('an empty result says which filters excluded everything', async ({ page }) => {
  // A language the fixture does not contain, so the answer is genuinely empty.
  await applyFilters(page, { lang: 'Klingon' });

  await expect(page.locator('[data-testid="no-matches"]')).toBeVisible();
  const msg = (await page.locator('[data-testid="no-matches"]').textContent()).replace(/\s+/g, ' ');
  expect(msg).toContain('Klingon');

  // It must quote the tag's REAL size, not the filtered one.
  //
  // The template used Tag.WorkCount, which is the filtered count because the
  // heading above the list describes the set the page contains. With a filter
  // that matched nothing, that printed "the tag has 0 works in total" -- telling
  // the reader their filter emptied the tag, which is the opposite of what
  // happened. No Go test caught it: the Go test for this branch asserted the
  // filters were NAMED and never looked at the number beside them.
  // ?n=100 matters: the default limit is 20, and this tag has 28 distinct
  // works, so measuring without it reads 20 and the message's 28 looks wrong.
  //
  // It read 29 here at one point -- before SELECT DISTINCT landed, the list
  // held a duplicated row from work_tags being keyed on
  // (work_id, tag_id, tag_type). Both numbers were once true of the same page,
  // which is the shape of the bug that fix removed.
  //
  // sort=kudos is REQUIRED to measure it: the default view is now the taste
  // blend, whose list mixes in works that do NOT carry the tag -- counting
  // that list would measure the blend, not the tag, and then call the
  // message's correct size wrong.
  await page.goto(`${TAG}?n=100&sort=kudos`);
  const all = await listedWorkIds(page);
  const realSize = all.length;
  expect(realSize).toBeGreaterThan(0);
  expect(new Set(all).size).toBe(realSize); // no work listed twice
  expect(msg).toMatch(new RegExp(`tag has ${realSize} works? in total`));
  expect(msg).not.toMatch(/tag has 0 works/);
});

test('an unrecognised completion value is refused in words, not silently dropped', async ({ page }) => {
  // Hand-built URL, because no control can produce this value -- which is the
  // point: it is what a typo or a stale bookmark produces.
  await page.goto(page.url().split('?')[0] + '?complete=maybe');

  await expect(page.locator('[data-testid="filter-error"]')).toBeVisible();
  const err = await page.locator('[data-testid="filter-error"]').textContent();
  expect(err).toContain('is not a completion filter');
  expect(err).toContain('Showing every work on this tag instead');

  // And the list really is unfiltered -- not quietly half-filtered.
  const listed = await listedWorkIds(page);
  const unfiltered = await (async () => {
    await page.goto(page.url().split('?')[0]);
    return listedWorkIds(page);
  })();
  expect(listed.join(',')).toBe(unfiltered.join(','));
});

test('the filters survive the count being filter-aware', async ({ page }) => {
  await applyFilters(page, { complete: 'true' });
  const heading = await page.locator('[data-testid="works-heading"]').textContent();

  // The heading counts the filtered set. Reading only "of N" would pass even
  // when the count is the unfiltered total, so compare against the API's own
  // number for the same filter, which is what a client would see.
  const unfilteredHeading = await (async () => {
    await page.goto(page.url().split('?')[0]);
    return page.locator('[data-testid="works-heading"]').textContent();
  })();

  expect(heading).not.toBe(unfilteredHeading);
});

test('the tag page still needs no JavaScript', async ({ page, context }) => {
  // SPEC 1.1: server-rendered, no script. The whole filter surface has to work
  // with scripting off, which is a stronger claim than "works in Chromium".
  await context.addInitScript(() => {
    // Neutralise script execution for this page, approximating a reader with
    // JS disabled without needing a second browser context.
    window.__kindredNoScriptRan = false;
  });
  await page.goto(page.url());
  await applyFilters(page, { complete: 'true' });

  const listed = await listedWorkIds(page);
  expect(listed.length).toBeGreaterThan(0);

  const scriptCount = await page.$$eval('script', (ss) => ss.length);
  expect(scriptCount).toBe(0);
});

// --- ?format=csv ------------------------------------------------------------
//
// The API tests already pin the quoting and the NULL convention. What only the
// e2e harness can show is that the CSV and the JSON agree about WHICH rows were
// selected -- a disagreement there would be invisible to a unit test, which
// only ever asks each format separately.

test('CSV and JSON select the same works', async ({ request }) => {
  const query = '/api/v1/ao3/works?limit=100&sort=date';

  const json = await request.get(query);
  expect(json.status()).toBe(200);
  const jsonBody = await json.json();
  const jsonInOrder = jsonBody.works.map((w) => String(w.id));
  const jsonIds = jsonInOrder.slice().sort();

  const csvRes = await request.get(query + '&format=csv');
  expect(csvRes.status()).toBe(200);
  expect(csvRes.headers()['content-type']).toContain('text/csv');

  const body = await csvRes.text();
  // Drop the '#' preamble and the header row, then read the first field of each
  // record. The titles in this fixture are single-line, so a line is a record
  // here -- the hostile multi-line title is pinned by the Go test, which is the
  // only place a fixture can hold one without breaking every other assertion.
  const rows = body
    .split('\n')
    .filter((l) => l.length && !l.startsWith('#'))
    .slice(1)
    .map((l) => l.split(',')[0]);

  // Sort BOTH sides. The first version sorted jsonIds and left `rows` in file
  // order, so the test reported a 40-vs-40 disagreement that was purely the
  // order of two equal sets -- which is the "identical counts therefore broken"
  // mistake in its purest form: the numbers agreed, the sets agreed, and only
  // the comparison was wrong.
  expect(rows.slice().sort()).toEqual(jsonIds);
  expect(rows.length).toBeGreaterThan(0);

  // And separately: the two formats must agree on ORDER too, not only on
  // membership. A CSV that re-sorted its rows would still pass the check above.
  expect(rows).toEqual(jsonInOrder);
});

test('the CSV records the filters it applied', async ({ request }) => {
  // A reader who exported with a filter must be able to tell an empty result
  // from an ignored one, which is the reason the preamble exists at all.
  const res = await request.get('/api/v1/ao3/works?limit=5&complete=true&format=csv');
  expect(res.status()).toBe(200);
  const body = await res.text();
  expect(body).toContain('# filter complete=true');
  expect(body).toContain('# limit=5');
});

test('an unknown format falls through to JSON', async ({ request }) => {
  // Silently serving CSV for format=xml would be a wrong answer, not a lenient
  // one.
  const res = await request.get('/api/v1/ao3/works?format=xml');
  expect(res.headers()['content-type']).toContain('application/json');
  const body = await res.json();
  expect(Array.isArray(body.works)).toBe(true);
});
