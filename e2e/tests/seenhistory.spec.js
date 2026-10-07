// Drives the seen-work history the way a reader does: get a recommendation
// list, mark one read, ask for recommendations that hide what they have seen,
// and confirm the marked work is gone and the others are not.
const { test, expect } = require('@playwright/test');

// The e2e server indexes a 40-work FIXTURE corpus, not the real mirror, so a
// real AO3 id does not exist in it and /recommend answers 500 "no seed in the
// corpus". Work 1 is the fixture's seed; every other spec reaches for the same
// one.
const SEED = 'ao3_work:1';

// The e2e server has its own store, and every test in this file shares it, so
// each test uses a distinct work id and asserts on what IT marked rather than
// on the size of the exclusion set.
async function workIds(page) {
  // Read ids off the /work/<id> links, matching read.spec.js rather than
  // adding a test id the page has no reason to carry.
  return page.$$eval('[data-testid="results"] a[href^="/work/"]', (els) =>
    els.map((e) => Number(e.getAttribute('href').replace('/work/', ''))));
}

async function firstWorkId(page) {
  const ids = await workIds(page);
  expect(ids.length).toBeGreaterThan(0);
  return ids[0];
}

// Click the submit BUTTON, not the form element.
//
// Clicking a <form> clicks the middle of its box, which the button does not
// fill, so the click lands on the form's padding, nothing is submitted, and
// the test fails on an assertion about exclusion while the button it thought
// it pressed did nothing at all. Measured: clicking the form left the work
// recommended; clicking the button removed it.
async function markRead(page, workId) {
  await page
    .locator(`form.seen-form:has([name=work_id][value="${workId}"]) button[type=submit]`)
    .click();
  await page.waitForLoadState('domcontentloaded');
}

test.describe('seen-work history', () => {
  test('the recommendation page offers a mark-read control on every work', async ({ page }) => {
    await page.goto(`/recommend?seed=${SEED}&n=5`);

    const ids = await workIds(page);
    expect(ids.length).toBe(5);

    // One form per work, each carrying that work's id. A single shared form
    // would mark whichever work the reader happened to click, which is the
    // difference between the feature working and appearing to work.
    const formIds = await page.$$eval('form.seen-form', (forms) =>
      forms.map((f) => Number(f.querySelector('[name=work_id]').value)));
    expect(formIds).toEqual(ids);
    for (const id of formIds) {
      expect(id).toBeGreaterThan(0);
    }
  });

  test('marking a work read removes it from later recommendations', async ({ page }) => {
    await page.goto(`/recommend?seed=${SEED}&n=5`);
    const marked = await firstWorkId(page);

    // Mark read via the button, exactly as a reader would.
    await markRead(page, marked);
    await page.waitForLoadState('domcontentloaded');

    // The reader lands back on a list, not on an error page.
    expect(page.url()).toContain('/recommend');
    expect(await page.locator('.error, .fail').count()).toBe(0);

    // Ask for the same seed with seen-suppression on. The marked work must
    // not come back.
    await page.goto(`/recommend?seed=${SEED}&n=10&hide_seen=1`);
    const after = await workIds(page);
    expect(after).not.toContain(marked);

    // ...and it is still offered when suppression is OFF, because a read is
    // only recorded, never a permanent ban without the reader asking for one.
    await page.goto(`/recommend?seed=${SEED}&n=10&hide_seen=0`);
    const unfiltered = await workIds(page);
    expect(unfiltered.length).toBeGreaterThan(0);
  });

  test('hide_seen returns a full list, not a shorter one', async ({ page }) => {
    await page.goto(`/recommend?seed=${SEED}&n=5`);
    const marked = await firstWorkId(page);
    await markRead(page, marked);
    await page.waitForLoadState('domcontentloaded');

    await page.goto(`/recommend?seed=${SEED}&n=5&hide_seen=1`);
    const ids = await workIds(page);

    // The number requested, not "what was left after filtering". A filter
    // applied after ranking would return fewer works, so a reader would see
    // their results shrink every time they marked something read.
    expect(ids).toHaveLength(5);
    expect(ids).not.toContain(marked);
  });

  test('the repeat control reflects the state the reader chose', async ({ page }) => {
    await page.goto(`/recommend?seed=${SEED}&n=3&hide_seen=1`);
    await expect(page.locator('#hideseen')).toHaveValue('1');

    await page.goto(`/recommend?seed=${SEED}&n=3&hide_seen=0`);
    await expect(page.locator('#hideseen')).toHaveValue('0');
  });

  test('marking read returns the reader to the list they were reading', async ({ page }) => {
    const target = `/recommend?seed=${SEED}&n=7&pool=400`;
    await page.goto(target);
    const marked = await firstWorkId(page);

    await markRead(page, marked);
    await page.waitForLoadState('domcontentloaded');

    // The seed survives the round trip, so the reader is not dropped back on a
    // default recommendation with no relation to what they were doing.
    expect(page.url()).toContain('seed=');
    expect(new URL(page.url()).searchParams.get('seed')).toBe(SEED);
  });

  test('marking read is not a toggle: a double submit stays read', async ({ page }) => {
    await page.goto(`/recommend?seed=${SEED}&n=5`);
    const marked = await firstWorkId(page);

    // Submit the same explicit action twice over the wire.
    //
    // Deliberately NOT two clicks on the button: after the first submit the
    // reader lands on a fresh list, and a working exclusion means the marked
    // work is no longer on it, so the second click would wait forever for a
    // button that the feature correctly removed. Posting twice directly is
    // what a double-clicking reader or a retried request actually does.
    for (let i = 0; i < 2; i++) {
      const res = await page.request.post('/seen', {
        form: { work_id: String(marked), action: 'read' },
      });
      expect(res.status(), `submit ${i + 1}`).toBe(200);
    }

    await page.goto(`/recommend?seed=${SEED}&n=10&hide_seen=1`);
    expect(await workIds(page)).not.toContain(marked);
  });

  test('an off-site return_to is refused', async ({ page, baseURL }) => {
    await page.goto(`/recommend?seed=${SEED}&n=5`);
    const marked = await firstWorkId(page);

    // The reader has no way to set this from the UI, so drive the POST the way
    // a crafted page would.
    await page.request.post(`${baseURL}/seen`, {
      form: {
        work_id: String(marked),
        action: 'read',
        return_to: 'https://evil.example/phish',
      },
      maxRedirects: 0,
    });

    await page.goto(`/recommend?seed=${SEED}&n=10&hide_seen=1`);
    expect(new URL(page.url()).host).toBe(new URL(baseURL).host);
  });

  test('a bad work_id is refused with a 400, not a 500', async ({ page, baseURL }) => {
    for (const bad of ['', 'abc', '0', '-5', '1.5']) {
      const res = await page.request.post(`${baseURL}/seen`, {
        form: { work_id: bad, action: 'read' },
        maxRedirects: 0,
      });
      expect(res.status(), `work_id=${JSON.stringify(bad)}`).toBe(400);
    }
  });

  test('an unknown action is refused', async ({ page, baseURL }) => {
    const res = await page.request.post(`${baseURL}/seen`, {
      form: { work_id: '1', action: 'teleport' },
      maxRedirects: 0,
    });
    expect(res.status()).toBe(400);
    expect(await res.text()).toContain('action must be read or unread');
  });

  test('GET /seen is not a write endpoint', async ({ page, baseURL }) => {
    // The route guard allows POST only. A GET must not mark anything read,
    // because a prefetching crawler or a link prefetcher would then
    // silently fill a reader's history.
    const res = await page.request.get(`${baseURL}/seen?work_id=1&action=read`);
    expect(res.status()).not.toBe(303);
  });
});
