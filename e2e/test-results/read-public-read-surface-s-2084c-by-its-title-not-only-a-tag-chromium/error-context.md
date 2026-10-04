# Instructions

- Following Playwright test failed.
- Explain why, be concise, respect Playwright best practices.
- Provide a snippet of code with the fix, if possible.

# Test info

- Name: read.spec.js >> public read surface >> search finds a WORK by its title, not only a tag
- Location: tests/read.spec.js:53:3

# Error details

```
Error: expect(locator).toBeVisible() failed

Locator: locator('[data-testid="works"] a[href^="/work/"]').first()
Expected: visible
Timeout: 7000ms
Error: element(s) not found

Call log:
  - Expect "toBeVisible" locator('[data-testid="works"] a[href^="/work/"]').first() with timeout 7000ms
  - waiting for locator('[data-testid="works"] a[href^="/work/"]').first()

```

```yaml
- paragraph:
  - link "kindred":
    - /url: /
- paragraph: A general entity recommender over a local mirror of 40 works. Not affiliated with Archive of Our Own.
- search:
  - text: Search
  - textbox "Search":
    - /placeholder: a tag, a work, or an author…
    - text: Fixture
  - button "Search"
- heading "Works" [level=2]
- paragraph: No work’s title or author matches “Fixture”.
- heading "Tags" [level=2]
- paragraph: No tag matches “Fixture”. Tags are matched as substrings, so a shorter word finds more.
- navigation "Main":
  - list:
    - listitem:
      - link "Search":
        - /url: /
    - listitem:
      - link "Rank from seeds":
        - /url: /recommend
    - listitem:
      - link "Fandoms":
        - /url: /fandoms
    - listitem:
      - link "Underrated":
        - /url: /underrated
    - listitem:
      - link "Tag neighbours":
        - /url: /neighbours
    - listitem:
      - link "Taste profiles":
        - /url: /profiles
  - list:
    - listitem:
      - link "Arena":
        - /url: /arena
    - listitem:
      - link "Rankings":
        - /url: /leaderboard
    - listitem:
      - link "Your ranking":
        - /url: /my-ranking
    - listitem:
      - link "Block tags":
        - /url: /block
  - list:
    - listitem:
      - link "Stats":
        - /url: /stats
    - listitem:
      - link "Health":
        - /url: /healthz
    - listitem:
      - link "JSON API":
        - /url: /api/v1/stats
- paragraph: kindred e2e · full mode. The API is the product; these pages are a front for it.
- paragraph: The age of this mirror is not recorded, so how current these results are cannot be stated.
```

# Test source

```ts
  1   | /**
  2   |  * End-to-end tests: a real browser against a real kindred.
  3   |  *
  4   |  * ## What these cover that the Go tests do not
  5   |  *
  6   |  * The Go suite tests handlers by calling them with a constructed request. That
  7   |  * never exercises the template, the routing table, the static handler, or a
  8   |  * link actually being clickable. A page that renders an empty list because a
  9   |  * template range is wrong passes every Go test.
  10  |  *
  11  |  * Building cmd/e2eserver for this suite already found one such bug: a
  12  |  * `defer store.Close()` closed the database while the server was still serving,
  13  |  * so /api/v1/recommend answered 500 "load seeds: sql: database is closed" while
  14  |  * the home page rendered perfectly. Health-check-only CI would not have seen it.
  15  |  */
  16  | const { test, expect } = require('@playwright/test');
  17  | 
  18  | test.describe('public read surface', () => {
  19  |   test('the home page renders a working search box', async ({ page }) => {
  20  |     await page.goto('/');
  21  |     await expect(page).toHaveTitle(/kindred/i);
  22  | 
  23  |     const box = page.locator('#q');
  24  |     await expect(box).toBeVisible();
  25  |     await expect(box).toHaveAttribute('name', 'q');
  26  | 
  27  |     // The nav is on every page; if it breaks, every page is unreachable.
  28  |     for (const href of ['/arena', '/leaderboard', '/stats']) {
  29  |       await expect(page.locator(`a[href="${href}"]`).first()).toBeVisible();
  30  |     }
  31  |   });
  32  | 
  33  |   test('search finds a tag and links through to it', async ({ page }) => {
  34  |     await page.goto('/');
  35  |     await page.fill('#q', 'harry');
  36  |     await page.press('#q', 'Enter');
  37  |     await page.waitForURL(/\/search\?q=harry/);
  38  | 
  39  |     // Results must be LINKS to tag pages, not bare text. A tag page that is
  40  |     // not linkable is a dead end for every reader who finds it.
  41  |     const tags = page.locator('a.tag');
  42  |     await expect(tags.first()).toBeVisible();
  43  | 
  44  |     const href = await tags.first().getAttribute('href');
  45  |     expect(href).toMatch(/^\/tag\/\d+$/);
  46  | 
  47  |     await tags.first().click();
  48  |     await page.waitForURL(/\/tag\/\d+/);
  49  |     // The tag page must show works, not an empty shell.
  50  |     await expect(page.locator('div.work').first()).toBeVisible();
  51  |   });
  52  | 
  53  |   test('search finds a WORK by its title, not only a tag', async ({ page }) => {
  54  |     // The gap this closes. /search used to query the tags table ONLY, so a
  55  |     // work that exists in the mirror could not be found by its title. The
  56  |     // search box even said "Search tags" -- an honest label for a limitation
  57  |     // a reader feels at once, because the work you can name is the work you
  58  |     // most want to seed a recommendation from.
  59  |     //
  60  |     // The query is taken from a work the suite has already proven exists, so
  61  |     // this cannot pass by searching for something that is not there.
  62  |     await page.goto('/work/1');
  63  |     const title = (await page.locator('h1').innerText()).trim();
  64  |     expect(title.length).toBeGreaterThan(0);
  65  | 
  66  |     const word = title.split(/\s+/)[0];
  67  |     await page.goto(`/search?q=${encodeURIComponent(word)}`);
  68  |     await expect(page).toHaveURL(/\/search\?q=/);
  69  | 
  70  |     const works = page.locator('[data-testid="works"] a[href^="/work/"]');
> 71  |     await expect(works.first()).toBeVisible();
      |                                 ^ Error: expect(locator).toBeVisible() failed
  72  | 
  73  |     // The result must be a LINK to the work page, and must reach it.
  74  |     const href = await works.first().getAttribute('href');
  75  |     expect(href).toMatch(/^\/work\/\d+$/);
  76  |     await works.first().click();
  77  |     await page.waitForURL(/\/work\/\d+/);
  78  |     await expect(page.locator('h1')).toBeVisible();
  79  |   });
  80  | 
  81  |   test('a work found by search can seed a ranking in one click', async ({ page }) => {
  82  |     // The purpose of the whole feature: finding a work is only useful if the
  83  |     // next action is ranking from it. Without the seed link, search finds
  84  |     // things the reader then has to copy an ID out of the URL to use.
  85  |     await page.goto('/work/1');
  86  |     const title = (await page.locator('h1').innerText()).trim();
  87  |     const word = title.split(/\s+/)[0];
  88  | 
  89  |     await page.goto(`/search?q=${encodeURIComponent(word)}`);
  90  |     const seedLink = page
  91  |       .locator('[data-testid="works"] a.seed-link')
  92  |       .first();
  93  |     await expect(seedLink).toBeVisible();
  94  | 
  95  |     const href = await seedLink.getAttribute('href');
  96  |     expect(href).toMatch(/^\/recommend\?seed=ao3_work:\d+$/);
  97  | 
  98  |     await seedLink.click();
  99  |     await page.waitForURL(/\/recommend\?seed=ao3_work:/);
  100 |     // The ranking page must accept the seed rather than reject it.
  101 |     await expect(page.locator('form[action="/recommend"]')).toBeVisible();
  102 |   });
  103 | 
  104 |   test('a search matching nothing says so in BOTH halves', async ({ page }) => {
  105 |     // Two sections, two messages. One message would leave the reader unable
  106 |     // to tell whether the other half of the search ran at all -- the same
  107 |     // "is this empty or broken?" ambiguity this repo keeps fixing elsewhere.
  108 |     await page.goto('/search?q=zzz-definitely-not-in-the-corpus-zzz');
  109 |     await expect(page.locator('[data-testid="no-works"]')).toBeVisible();
  110 |     await expect(page.locator('[data-testid="no-tags"]')).toBeVisible();
  111 |   });
  112 | 
  113 |   test('a search that matches a tag but no work says which is empty', async ({ page }) => {
  114 |     // The discriminating case. 'dark' is a tag in the fixture and is not a
  115 |     // substring of any fixture title ("Fixture Work 0NN"), so this separates
  116 |     // the two halves: a page that showed only one message would pass a test
  117 |     // that checked nothing.
  118 |     await page.goto('/search?q=dark');
  119 |     await expect(page.locator('[data-testid="tag-count"]')).toBeVisible();
  120 |     await expect(page.locator('[data-testid="no-works"]')).toBeVisible();
  121 |   });
  122 | 
  123 |   test('the search box does not claim to search only tags', async ({ page }) => {
  124 |     // The label was the honest admission of the limitation. Now that works
  125 |     // are searched, a label still reading "Search tags" understates the
  126 |     // page and would send readers looking only for tags.
  127 |     await page.goto('/');
  128 |     const label = await page.locator('label[for="q"]').innerText();
  129 |     expect(label.trim().toLowerCase()).not.toContain('tags only');
  130 |   });
  131 | 
  132 |   test('every page states how old the data is, or that it is unknown', async ({ page }) => {
  133 |     // The footer must never be silent about freshness. The ingest command
  134 |     // writes index_built_at into the store's meta table and, before this,
  135 |     // nothing read it -- so the age of the data behind every recommendation
  136 |     // was invisible, and a stale index answered confidently from old data.
  137 |     //
  138 |     // Either branch is acceptable; silence is not. The e2e server's store has
  139 |     // no build stamp, so in practice this asserts the "not recorded" branch,
  140 |     // which is the one that must never degrade into a numeric age.
  141 |     await page.goto('/');
  142 |     const freshness = page.locator('[data-testid="freshness"]');
  143 |     await expect(freshness).toBeVisible();
  144 | 
  145 |     const text = (await freshness.innerText()).trim();
  146 |     expect(text.length).toBeGreaterThan(0);
  147 | 
  148 |     // It must say something about age, not just repeat the version line.
  149 |     expect(text.toLowerCase()).toMatch(
  150 |       /not recorded|built (today|\d+ day)|future/);
  151 | 
  152 |     // An unknown age must NOT be rendered as a number. This is the specific
  153 |     // lie the three-state design exists to prevent.
  154 |     if (/not recorded/i.test(text)) {
  155 |       expect(text).not.toMatch(/\d+\s*days?\s+ago/);
  156 |       expect(text).not.toContain('built today');
  157 |     }
  158 | 
  159 |     // And it appears on every page, not just the home page, because Base is
  160 |     // shared. A footer on one page is a footer on one page.
  161 |     for (const href of ['/arena', '/leaderboard']) {
  162 |       await page.goto(href);
  163 |       await expect(page.locator('[data-testid="freshness"]')).toBeVisible();
  164 |     }
  165 |   });
  166 | 
  167 |   test('a work page shows its title, tags and recommendations', async ({ page }) => {
  168 |     await page.goto('/work/1');
  169 | 
  170 |     const h1 = page.locator('h1');
  171 |     await expect(h1).toBeVisible();
```