// The stylesheet.
//
// The existing test ("is served and is real CSS") asserts length > 100 and
// contains `{`. That would pass a file of commented-out rules, and it did
// pass while this stylesheet had no dark mode, no print rules, no mobile
// breakpoint and no visible focus ring -- four things the ideas list scores
// 42 to 64 and calls "CSS only".
//
// So this file checks the things that are actually load-bearing:
//
//   - every token referenced with var() is DEFINED. A typo there is silently
//     ignored by every browser: the property falls back to its initial value
//     and the page looks wrong with no error anywhere. That is the failure
//     mode a length check cannot see.
//   - every brace balances.
//   - the four mechanisms exist AND are reachable, i.e. inside a media query
//     or a selector rather than only mentioned in a comment.
//   - the light theme is still the default, because "dark mode" that turns
//     the site dark unconditionally would pass a `prefers-color-scheme` grep.

import { test, expect } from '@playwright/test';

async function css(request) {
  const res = await request.get('/static/style.css');
  expect(res.ok()).toBeTruthy();
  return res.text();
}

test.describe('stylesheet', () => {
  test('braces balance', async ({ request }) => {
    const text = await css(request);
    const open = (text.match(/\{/g) || []).length;
    const close = (text.match(/\}/g) || []).length;
    expect(open, `open ${open} vs close ${close}`).toBe(close);
  });

  test('every var() reference is a defined token', async ({ request }) => {
    const text = await css(request);

    // Defined: "--name:" at the start of a declaration.
    const defined = new Set(
      [...text.matchAll(/(--[a-z0-9-]+)\s*:/g)].map((m) => m[1]));

    // Referenced: inside var(--name), with the optional fallback stripped.
    const referenced = new Set(
      [...text.matchAll(/var\(\s*(--[a-z0-9-]+)/g)].map((m) => m[1]));

    expect(referenced.size, 'no var() found -- did the test find the file?')
      .toBeGreaterThan(5);

    const missing = [...referenced].filter((t) => !defined.has(t)).sort();
    // Named explicitly, because "there are no missing tokens" as an
    // assertion is a tautology that also passes when the file is empty.
    expect(missing, 'var() references with no definition: a browser ignores '
      + 'these silently and the property falls back to its initial value')
      .toEqual([]);
  });

  test('dark mode exists and is conditional on the OS', async ({ request }) => {
    const text = await css(request);
    expect(text, 'no prefers-color-scheme block').toContain('@media (prefers-color-scheme: dark)');

    // The light palette must still be the unconditional default, or "dark
    // mode" has simply become "the theme".
    const rootIdx = text.indexOf(':root');
    const darkIdx = text.indexOf('@media (prefers-color-scheme: dark)');
    expect(rootIdx, 'no :root token block').toBeGreaterThan(-1);
    expect(darkIdx, 'dark block must come after the default tokens').toBeGreaterThan(rootIdx);

    // And the dark block must actually override something.
    const dark = text.slice(darkIdx, darkIdx + 900);
    const overridden = [...dark.matchAll(/(--[a-z0-9-]+)\s*:/g)].map((m) => m[1]);
    expect(overridden.length, 'the dark block defines no tokens, so it does nothing')
      .toBeGreaterThan(5);
  });

  test('dark mode does not leave hardcoded colours behind', async ({ request }) => {
    const text = await css(request);
    // Scoped to everything BEFORE the print block, on purpose. Print rules
    // hardcode #fff and #000, and must: paper is white, and a print
    // stylesheet that inherited the dark palette would come off the printer as
    // a solid black rectangle. The first version scanned the whole file and
    // failed on three correct lines.
    const body = text.slice(0, text.indexOf('@media print'));
    // Hex or rgb() appearing OUTSIDE a token definition or a comment is a
    // literal that the dark theme cannot override. The stylesheet had 13 of
    // these before the tokens were introduced; each one rendered
    // light-on-light in dark mode.
    const offenders = [];
    body.split('\n').forEach((line, i) => {
      const t = line.trim();
      if (t.startsWith('/*') || t.startsWith('*') || t.startsWith('//')) return;
      if (/^\s*--[a-z0-9-]+\s*:/.test(line)) return;       // a token definition
      if (/#([0-9a-f]{3}|[0-9a-f]{6})\b/i.test(t) || /rgba?\(/i.test(t)) {
        offenders.push(`${i + 1}: ${t.slice(0, 70)}`);
      }
    });
    expect(offenders, 'literal colours the dark theme cannot override:\n'
      + offenders.join('\n')).toEqual([]);
  });

  test('there is a mobile breakpoint and it is narrow enough to matter',
    async ({ request }) => {
      const text = await css(request);
      // `screen and` is allowed between @media and the condition, and this
      // project uses it. A regex of `@media\s*\(max-width` therefore finds
      // NOTHING -- which is how the idea audit reported "no @media (max-width"
      // and concluded the project had no narrow-screen rules while one had
      // been there all along. Match @media, then read the condition.
      const m = text.match(/@media[^\{]*max-width:\s*([\d.]+)\s*(rem|px|em)/);
      expect(m, 'no max-width breakpoint').not.toBeNull();
      // A 60rem breakpoint is a tablet; the point is phones, which are under
      // 30rem. Anything at or above 50rem does not test a phone.
      const v = parseFloat(m[1]);
      const rem = m[2] === 'rem' || m[2] === 'em' ? v : v / 16;
      // Phones are 20-30rem wide, tablets start around 48rem.
      //
      // The first version asserted < 32rem and failed against a correct 40em
      // breakpoint. But 40em DOES match a 360px phone: at the default 16px
      // root, 40em is 640px and 360px < 640px. The threshold was the bug.
      //
      // What matters is only that the breakpoint is below tablet width, so it
      // cannot be a desktop-only media query wearing a max-width.
      expect(rem, `breakpoint is ${m[0]} (${rem}rem); a tablet-width `
        + 'threshold would never apply to a phone')
        .toBeLessThan(48);
    });

  test('print rules exist and hide the controls', async ({ request }) => {
    const text = await css(request);
    expect(text).toContain('@media print');
    const print = text.slice(text.indexOf('@media print'));
    // A printed recommendation list is useless without the URLs, and useless
    // on ink if it prints the search box.
    expect(print, 'print rules do not print external URLs')
      .toContain('attr(href)');
    expect(print, 'print rules do not hide forms')
      .toMatch(/form[^{]*\{[^}]*display:\s*none/);
  });

  test('print forces a white page, not the dark palette', async ({ request }) => {
    // The deliberate exception to the no-literals rule above, asserted so it
    // stays deliberate: someone tidying the print block back to var() would
    // print a solid black rectangle.
    const text = await css(request);
    const print = text.slice(text.indexOf('@media print'));
    expect(print, 'print rules do not force a white page')
      .toMatch(/background:\s*#fff/);
    // And they must not reference the dark tokens.
    const darkTok = text.slice(text.indexOf('prefers-color-scheme: dark'));
    const darkNames = [...darkTok.matchAll(/(--[a-z0-9-]+)\s*:/g)]
      .map((m) => m[1]);
    const leaked = darkNames.filter(
      (t) => new RegExp(`${t}\\s*:`).test(print)
        && !print.includes(`${t}: `));
    expect(leaked, 'print rules override a dark token').toEqual([]);
  });

  test('focus is visible for keyboard users', async ({ request }) => {
    const text = await css(request);
    expect(text, 'no :focus-visible rule; keyboard users get the UA default '
      + 'or, if it is removed elsewhere, nothing at all')
      .toMatch(/:focus-visible\s*\{[^}]*outline:/);
    // And the outline must be a real width, not "outline: none".
    const block = text.match(/:focus-visible\s*\{([^}]*)\}/)[1];
    expect(block).not.toMatch(/outline:\s*(none|0)\b/);
  });

  test('reduced motion is respected', async ({ request }) => {
    // Not in the ideas list. It costs four lines and it is the difference
    // between a page some people can read and one they cannot.
    const text = await css(request);
    expect(text).toContain('@media (prefers-reduced-motion: reduce)');
  });

  test('no page pulls in a second stylesheet or an inline style block',
    async ({ page }) => {
      // Dark mode has to live in THIS file, because there is no JS to swap a
      // stylesheet link. A second <link> would be a theme that cannot be
      // conditional.
      //
      // await page.goto('/') FIRST. The first version of this test omitted it,
      // so $$eval ran against about:blank -- which has no <link> -- and it
      // reported "expected exactly one stylesheet, got []" on a page that
      // has exactly one. The lesson is the one from the repo's own history:
      // a harness that did not measure reports a confident wrong answer.
      await page.goto('/');
      const links = await page.$$eval('link[rel="stylesheet"]',
        (els) => els.map((e) => e.getAttribute('href')));
      expect(links.length, `expected exactly one stylesheet, got ${links}`)
        .toBe(1);
      expect(links[0]).toContain('style.css');

      const inline = await page.$$eval('style', (els) => els.length);
      expect(inline, 'inline <style> blocks bypass the single stylesheet')
        .toBe(0);
    });

  test('the dark palette is not applied without the OS asking for it',
    async ({ page }) => {
      // The real assertion of "dark mode is conditional": with the colour
      // scheme forced to light, the page background must be light.
      await page.emulateMedia({ colorScheme: 'light' });
      await page.goto('/');
      const bg = await page.evaluate(
        () => getComputedStyle(document.body).backgroundColor);
      const rgb = bg.match(/\d+/g).map(Number);
      // A dark background has low luminance; assert it is actually light.
      const lum = 0.2126 * rgb[0] + 0.7152 * rgb[1] + 0.0722 * rgb[2];
      expect(lum, `body background ${bg} is dark while the OS asked for light`)
        .toBeGreaterThan(200);
    });

  test('the dark palette is applied when the OS asks for it', async ({ page }) => {
    await page.emulateMedia({ colorScheme: 'dark' });
    await page.goto('/');
    const bg = await page.evaluate(
      () => getComputedStyle(document.body).backgroundColor);
    const rgb = bg.match(/\d+/g).map(Number);
    const lum = 0.2126 * rgb[0] + 0.7152 * rgb[1] + 0.0722 * rgb[2];
    expect(lum, `body background ${bg} is still light in dark mode`)
      .toBeLessThan(80);

    // And text must not be light-on-light, which is the failure a token swap
    // produces when one of the two is missed.
    const fg = await page.evaluate(
      () => getComputedStyle(document.body).color);
    const frgb = fg.match(/\d+/g).map(Number);
    const flum = 0.2126 * frgb[0] + 0.7152 * frgb[1] + 0.0722 * frgb[2];
    expect(Math.abs(flum - lum),
      `text ${fg} and background ${bg} have near-identical luminance`)
      .toBeGreaterThan(80);
  });

  test('no horizontal overflow at phone width', async ({ page }) => {
    // The layout is a single column of full-width cards, so it does not
    // overflow -- measured on the test corpus, every route scored 0px. That
    // is why the first mutation survived: the mobile CSS was fixing nothing
    // for the data the tests actually load.
    //
    // The overflow is a property of the DATA. Measured on the real mirror:
    // longest title 255 chars, longest tag name 150, longest tag with NO
    // SPACE in it 96. A single 96-character token cannot wrap on its own, so
    // it is pushed to the viewport edge unless something says otherwise.
    //
    // So the page is fed the real extremes, and the assertion is on the
    // measured scrollWidth. This is the arm that can fail when the mobile
    // block is removed.
    const longTitle =
      "Help! I Was Rinancarnated Again By This Isekai Obsessed Immortal Being "
      + 'and Now I\u2019m Surrounded By Rebels That Want to Vent Me! Feral '
      + 'Breaker floretta: The Ruthless Reincarnated Floret Forms the Ultimate '
      + 'Rinan Revolt! All Roots Lead to THEIR Domestication!!';
    // VERBATIM from the real mirror, and chosen because it has NO SPACES:
    //   izuku/ochako/mina/momo/mei/ibara/tsuyu/himiko/setsuna/itsuka/yui/
    //   pony/shoko/camie/melissa/kinoko                      (89 chars)
    //
    // The 150-character tag I used first ("no one is infected by a demon...")
    // is mostly spaces, so it wraps at ANY width and the test passed with the
    // overflow-wrap rule deleted -- it was measuring nothing. This one has no
    // break opportunity at all, so it is the string that makes the gate real.
    const unbreakableTag =
      'izuku/ochako/mina/momo/mei/ibara/tsuyu/himiko/setsuna/itsuka/yui/'
      + 'pony/shoko/camie/melissa/kinoko';

    await page.setViewportSize({ width: 360, height: 640 });

    // A tag list rendered inline, which is the element that actually wraps
    // the long names. Built in the page rather than served from a route,
    // because no fixture route carries the real strings.
    await page.goto('/');
    const over = await page.evaluate(([tag, longTitle]) => {
      const box = document.createElement('p');
      box.className = 'tags';
      // Deliberately NO width override. The first version set
      // width:max-content to "give it every chance to overflow", which
      // defeated the very rule under test: the box grew to its natural
      // 2,177px whatever the stylesheet said, and the test failed against
      // correct CSS. An 89-character token has no break opportunity, so the
      // only question is whether overflow-wrap: anywhere gives the browser
      // one -- and that is answerable inside the container's own width.
      for (let i = 0; i < 3; i += 1) {
        const a = document.createElement('a');
        a.href = '/tag/1';
        a.textContent = tag;
        box.appendChild(a);
      }
      const h = document.createElement('h3');
      h.textContent = longTitle;
      h.style.width = '100%';
      document.querySelector('main, body').append(h, box);

      const de = document.documentElement;
      return {
        over: de.scrollWidth - de.clientWidth,
        tagWidth: Math.round(box.getBoundingClientRect().width),
        clientWidth: de.clientWidth,
      };
    }, [unbreakableTag, longTitle]);

    expect(over.over,
      `the page overflows by ${over.over}px at 360px with a 96-character `
      + `unbreakable tag (element is ${over.tagWidth}px, viewport `
      + `${over.clientWidth}px)`)
      .toBeLessThanOrEqual(1);

    // And the element itself must be constrained, not merely the page.
    expect(over.tagWidth,
      `the tag list is ${over.tagWidth}px in a ${over.clientWidth}px viewport`)
      .toBeLessThanOrEqual(over.clientWidth);
  });

  test('a long unbreakable token is broken, not pushed off-screen',
    async ({ page }) => {
      // The mechanism itself: overflow-wrap: anywhere gives the browser a
      // break opportunity inside a token that has no space. Without it, a
      // single 96-character tag is one unbreakable box.
      await page.setViewportSize({ width: 360, height: 640 });
      await page.goto('/');
      const widths = await page.evaluate(() => {
        const el = document.createElement('span');
        el.textContent = 'x'.repeat(96);
        el.style.cssText = 'position:absolute;top:0;left:0;';
        document.body.appendChild(el);
        const plain = el.getBoundingClientRect().width;
        el.style.overflowWrap = 'anywhere';
        const wrapped = el.getBoundingClientRect().width;
        el.remove();
        return { plain, wrapped };
      });
      // Documented as the mechanism this stylesheet relies on. If the
      // browser ever stops honouring overflow-wrap: anywhere, this is the
      // test that says so rather than the mobile one failing mysteriously.
      expect(widths.wrapped,
        'overflow-wrap: anywhere did not narrow a 96-char token, so the '
        + 'mobile tag fix cannot work')
        .toBeLessThan(widths.plain);
    });

  test('the recommend form is usable at phone width', async ({ page }) => {
    // The seed inputs use size="18", a CHARACTER count, so their width does
    // not follow the viewport. Measured at 360px: the first seed input is
    // 317px (no size attribute at all) and the second is 169px, and the
    // submit button ends at 113px, inside the viewport. So the form was
    // already usable and the width:100% rule was cargo cult.
    //
    // What IS worth asserting: the submit button must be reachable, and the
    // inputs must not overflow.
    await page.setViewportSize({ width: 360, height: 640 });
    await page.goto('/recommend');
    const submit = await page.$('input[type="submit"]');
    expect(submit, 'the recommend form has no submit button').not.toBeNull();
    const box = await submit.boundingBox();
    expect(box.x + box.width,
      `the submit button ends at ${box.x + box.width}px, past the 360px viewport`)
      .toBeLessThanOrEqual(361);

    const over = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    expect(over, `the recommend form overflows by ${over}px at 360px`).toBeLessThanOrEqual(1);
  });

  test('tables are made scrollable on narrow screens', async ({ request }) => {
    // Asserted against the STYLESHEET, not against a rendered page, and the
    // comment says why rather than pretending otherwise.
    //
    // The rendering version of this test was written first and could not
    // work: the tune table lives inside a collapsed <details> that is only
    // emitted when .Tune is populated, and the e2e corpus (~40 works)
    // produces no recommendations at all -- so there is no table in the DOM
    // to measure. Verified against the real 112,935-work mirror: that page
    // renders 1 <details>, 1 <table> and 2 <tr>. So the rule is real and the
    // fixture cannot reach it.
    //
    // The alternative -- a Playwright test that opens <details> and finds
    // nothing -- is the harness-reporting-on-something-it-never-measured
    // shape this repo has hit repeatedly. Asserting the rule is honest;
    // claiming to have tested the rendering would not be.
    //
    // To upgrade this to a rendering test: give the e2e corpus two works that
    // share a tag, so a recommendation exists and .Tune is populated.
    const text = await css(request);
    const mobile = text.slice(text.indexOf('@media screen and (max-width:'));
    expect(mobile, 'no narrow-screen table rule').toMatch(
      /table\s*\{[^}]*display:\s*block[^}]*overflow-x:\s*auto/);
  });

  test('tap targets are large enough to hit', async ({ page }) => {
    // Not from the ideas list. The default link hit area is about 24px tall;
    // 44px is the accessibility minimum. A stylesheet that makes links
    // 16px-tall for density costs its phone readers.
    await page.setViewportSize({ width: 360, height: 640 });
    await page.goto('/search?q=general');
    const small = await page.$$eval('a[href]', (els) => els
      .map((e) => ({ h: Math.round(e.getBoundingClientRect().height), t: e.textContent.trim().slice(0, 24) }))
      .filter((x) => x.h > 0 && x.h < 24)
      .slice(0, 5));
    expect(small,
      `links under 24px tall: ${JSON.stringify(small)}`).toEqual([]);
  });
});
