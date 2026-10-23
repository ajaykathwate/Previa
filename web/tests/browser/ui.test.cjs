const {test, before, after, beforeEach, afterEach} = require('node:test');
const assert = require('node:assert/strict');
const {mkdirSync} = require('node:fs');
const path = require('node:path');
const {chromium} = require('playwright');

const origin = process.env.BROWSER_BASE_URL;
if (!origin) throw new Error('Run make test-browser to start the test-only HTTP fixture.');
let browser, context, page, errors;

before(async () => {
  browser = await chromium.launch({headless: true,
    ...(process.env.UI_BROWSER_EXECUTABLE ? {executablePath: process.env.UI_BROWSER_EXECUTABLE} : {}),
  });
});
after(async () => { await browser?.close(); });
beforeEach(async () => {
  context = await browser.newContext({viewport: {width: 1440, height: 900}});
  page = await context.newPage();
  errors = [];
  page.on('pageerror', error => errors.push(error.message));
  // Content-Security-Policy violations are console errors, not page errors.
  page.on('console', message => { if (message.type() === 'error' && /Content Security Policy|Refused to/.test(message.text())) errors.push(message.text()); });
});
afterEach(async t => {
  try {
    assert.deepEqual(errors, [], 'unexpected browser JavaScript errors');
    if (process.env.UI_BROWSER_SCREENSHOTS === '1') {
      const directory = path.join(__dirname, 'artifacts');
      mkdirSync(directory, {recursive: true});
      await page.emulateMedia({reducedMotion: 'reduce'});
      await page.evaluate(() => scrollTo({top: 0, behavior: 'instant'}));
      await page.screenshot({path: path.join(directory, `${t.name.replace(/[^a-z0-9]+/gi, '-')}.png`), fullPage: true});
    }
  } finally { await context?.close(); }
});

async function login() {
  await page.goto(origin);
  await page.getByRole('link', {name: 'Get started with GitHub', exact: true}).first().click();
  await page.waitForURL(`${origin}/dashboard`);
  assert.match(await page.locator('.app-header .account-name').innerText(), /design-review/);
  assert.equal(await page.locator('h1').innerText(), 'Overview');
}

test('signed-out PR build logs return to the same logs after GitHub login', async () => {
  await context.request.post(`${origin}/__fixture/logs/seed`);
  const path = '/repositories/1/pull-requests/3/logs';
  await page.goto(`${origin}${path}`);
  await page.waitForURL(`${origin}${path}`);
  assert.match(await page.locator('.app-breadcrumb').innerText(), /#3[\s\S]*Build logs/);
  assert.equal(new URL(page.url()).pathname, path);
});

test('automatic build shows its progress live as status, not a dead button, and refreshes after a confirmation closes', async () => {
  const change = state => context.request.post(`${origin}/__fixture/build-state`, {form: {state}});
  await change('pending');
  try {
    await login();
    await page.goto(`${origin}/repositories/1/pull-requests/7`);
    assert(await page.getByRole('button', {name: 'Build now', exact: true}).isEnabled());
    await page.waitForFunction(() => document.documentElement.dataset.liveState === 'live');
    await change('queued');
    const queued = page.locator('.build-in-progress', {hasText: 'Build queued'});
    await queued.waitFor();
    assert.equal(await page.getByRole('button', {name: 'Build queued'}).count(), 0, 'progress is a status, not a button');
    await change('running');
    const running = page.locator('.build-in-progress', {hasText: 'Building preview'});
    await running.waitFor();
    assert.equal(await page.getByRole('button', {name: 'Building preview'}).count(), 0);
    await page.getByRole('button', {name: 'Cancel build', exact: true}).click();
    await page.waitForFunction(() => document.querySelector('#teardown-preview').matches(':modal'));
    await change('succeeded');
    // The live signal is deferred while this dialog is open.
    await page.waitForTimeout(200);
    assert(await running.isVisible());
    await page.keyboard.press('Escape');
    await page.getByRole('button', {name: 'Rebuild preview', exact: true}).waitFor();
  } finally { await change(''); }
});
// A live page refetches its panel once its event stream connects, and htmx
// wires the swapped-in fields only after that swap settles: a value typed in
// between sends no request. Wait until the stream is live and nothing is
// requesting, swapping or settling before typing into a live page.
async function liveIdle(target = page, options) {
  await target.waitForFunction(() => document.documentElement.dataset.liveState === 'live' &&
    !document.querySelector('.htmx-request, .htmx-swapping, .htmx-settling, .htmx-added'), null, options);
}
async function readyPanel(id) {
  await page.locator(`[data-selected-repo="${id}"]`).waitFor();
  await page.waitForFunction(()=>!document.getElementById('repository-detail').hasAttribute('aria-busy'));
}
async function openLogout() {
  // Real pointer clicks avoid automation scrolling sticky-header elements into view.
  for (const locator of [page.locator('.account-menu summary'), page.locator('.account-menu').getByRole('button', {name: 'Log out', exact: true})]) {
    const box = await locator.boundingBox();
    assert(box, 'logout control must be visible');
    await page.mouse.click(box.x + box.width / 2, box.y + box.height / 2);
  }
  await page.waitForFunction(() => document.querySelector('#logout-confirmation').matches(':modal'));
}
async function assertNoOverflow() {
  assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'page has horizontal overflow');
}

test('public home and protected dashboard access', async () => {
  for (const route of ['/dashboard','/repositories','/builds','/previews','/environments','/developer-tools']) {
    assert.equal((await context.request.get(`${origin}${route}`, {maxRedirects:0})).status(),303);
  }
  await page.goto(origin);
  assert.match(await page.locator('h1').innerText(), /Review the branch/);
  assert.equal(await page.locator('[data-queue-form]').count(), 0);
  await page.getByRole('link', {name: 'Why Previa', exact: true}).click();
  assert.equal(new URL(page.url()).hash, '#why');
  // The home page states only what works today, without the internal stack.
  const home = await page.locator('main').innerText();
  assert.doesNotMatch(home, /coming next|coming soon|self-hosted|PostgreSQL 17, which|River|Traefik|BuildKit/i);
  // The FAQ is public: signed out, /faq is the home page's FAQ section.
  const faq = await context.request.get(`${origin}/faq`, {maxRedirects: 0});
  assert.equal(faq.status(), 303);
  assert.equal(faq.headers().location, '/#faq');
});

test('preview unavailable page is an uncached cookie-free 404 in both themes', async () => {
  for (const scheme of ['light', 'dark']) {
    await page.emulateMedia({colorScheme: scheme});
    const response = await page.goto(`${origin}/previews/unavailable`);
    assert.equal(response.status(), 404);
    const headers = await response.allHeaders();
    assert.equal(headers['set-cookie'], undefined);
    assert.equal(headers['cache-control'], 'no-store');
    await page.getByRole('heading', {name: 'Preview not found or expired', level: 1}).waitFor();
    await assertNoOverflow();
  }
  assert.equal((await context.cookies()).length, 0);
  await page.setViewportSize({width: 390, height: 844});
  await assertNoOverflow();
  await page.getByRole('link', {name: 'Go to Previa', exact: true}).click();
  await page.waitForURL(`${origin}/`);
});

test('responsive dashboard sticky navigation and honest placeholders', async () => {
  await login();
  await page.goto(`${origin}/repositories`);
  for (const width of [320, 390, 768, 1024, 1440]) {
    await page.setViewportSize({width, height: 844});
    await assertNoOverflow();
    await page.evaluate(() => scrollTo({top: 500, behavior: 'instant'}));
    assert.equal(await page.evaluate(() => scrollY), 0);
    const list = page.locator('.app-content');
    await list.evaluate(e => {e.scrollTop = 300;});
    assert(await list.evaluate(e => e.scrollTop > 0));
    assert.equal(await page.locator('.app-header').evaluate(e => Math.round(e.getBoundingClientRect().top)), 0);
    await page.evaluate(() => scrollTo({top: 0, behavior: 'instant'}));
  }
  assert.equal(await page.locator('.repository-row').count(), 10);
  assert.equal(await page.locator('[data-queue-form]').count(), 0);
});

test('Overview places open PRs along the pipeline and its activity tabs and rows are interactive', async () => {
  await login();
  // An empty workspace: the blocked notice, setup steps and honest zeros.
  assert(await page.locator('.ov-alert').isVisible());
  assert.match(await page.locator('.ov-setup-progress').innerText(), /0 of 4\s+done/);
  assert.match(await page.locator('.ov-step.is-next').innerText(), /Connect a repository/);
  assert.deepEqual(await page.locator('.ov-flow-count').allInnerTexts(), ['0', '0', '0', '0']);
  assert.match(await page.locator('.ov-status').innerText(), /No worker running/);
  // A busy workspace.
  await page.goto(`${origin}/__fixture/overview?state=busy`);
  await page.waitForURL(`${origin}/dashboard`);
  assert.equal(await page.locator('.ov-alert').count(), 0);
  assert.equal(await page.locator('.ov-setup').count(), 0);
  assert.match(await page.locator('.ov-status').innerText(), /Worker connected/);
  assert.match(await page.locator('[data-overview-updated]').innerText(), /^Updated just now$/);
  assert.deepEqual(await page.locator('.ov-flow-count').allInnerTexts(), ['12', '2', '1', '4']);
  assert.deepEqual(await page.locator('.ov-flow-share small').allInnerTexts(), ['All tracked', '16% of open PRs', '8% of open PRs', '33% of open PRs']);
  assert.match(await page.locator('.ov-flow-failed').innerText(), /1 failed request/);
  assert.equal(await page.locator('.ov-ring').getAttribute('aria-label'), '33% of open pull requests have a live preview');
  // Activity reads as a table: commit, status and when, with the failure in plain words.
  assert.deepEqual(await page.locator('#ov-panel-builds .ov-table-head span').allTextContents(), ['Pull request', 'Commit', 'Status', 'Updated']);
  assert.match(await page.locator('#ov-panel-builds .ov-row', {hasText: 'Fixture pull request 4'}).locator('.ov-col-reason').innerText(), /Your app didn't become ready/);
  // Tabs switch by click or arrow keys, and the choice is kept for the session.
  const builds = page.locator('#ov-panel-builds');
  const previews = page.locator('#ov-panel-previews');
  assert(await builds.isVisible());
  assert.equal(await previews.isVisible(), false);
  await page.locator('#ov-tab-builds').focus();
  await page.keyboard.press('ArrowRight');
  assert.equal(await page.locator('#ov-tab-previews').getAttribute('aria-selected'), 'true');
  assert(await page.evaluate(() => document.activeElement.id === 'ov-tab-previews'));
  assert(await previews.isVisible());
  assert.equal(await builds.isVisible(), false);
  // A preview expands to what it serves and how much of its lifetime is left.
  const preview = previews.locator('.ov-row').first();
  await preview.locator('summary').click();
  assert.match(await preview.locator('.ov-facts').innerText(), /newer commit .* is not live yet/);
  assert.equal(await preview.locator('[data-lifetime-text]').innerText(), '75% of its lifetime left');
  // Open goes through Previa, which records the open for the Setup checklist.
  assert.equal(await preview.locator('.ov-open').getAttribute('href'), '/repositories/1/pull-requests/3/open');
  await page.reload();
  assert.equal(await page.locator('#ov-tab-previews').getAttribute('aria-selected'), 'true');
  // A failed request says why and how long it took.
  await page.locator('#ov-tab-builds').click();
  const failed = builds.locator('.ov-row', {hasText: 'Fixture pull request 4'});
  await failed.locator('summary').click();
  assert.match(await failed.locator('.ov-facts').innerText(), /The app did not become ready\./);
  assert.match(await failed.locator('.ov-facts').innerText(), /took 3 min/);
  assert.equal(await failed.locator('.ov-row-links a').last().getAttribute('href'), '/repositories/1/pull-requests/4/logs');
  for (const width of [320, 390, 768, 1024, 1440]) {
    await page.setViewportSize({width, height: 844});
    await assertNoOverflow();
  }
  // Without JavaScript both lists show and their rows still expand.
  const noJS = await browser.newContext({javaScriptEnabled: false});
  try {
    const plain = await noJS.newPage();
    await plain.goto(`${origin}/auth/github/login`);
    await plain.waitForURL(`${origin}/dashboard`);
    await plain.goto(`${origin}/__fixture/overview?state=busy`);
    await plain.waitForURL(`${origin}/dashboard`);
    assert.equal(await plain.locator('[data-overview-tabs]').isVisible(), false);
    assert.equal(await plain.locator('[data-overview-updated]').isVisible(), false);
    assert(await plain.locator('#ov-panel-builds').isVisible());
    assert(await plain.locator('#ov-panel-previews').isVisible());
    await plain.locator('#ov-panel-builds .ov-row summary').first().click();
    assert(await plain.locator('#ov-panel-builds .ov-row .ov-facts').first().isVisible());
  } finally { await noJS.close(); }
});

test('mobile navigation opens separate pages and highlights the destination', async () => {
  await login();
  await page.setViewportSize({width: 390, height: 844});
  await page.emulateMedia({reducedMotion: 'reduce'});
  await page.locator('.mobile-workspace-nav summary').click();
  await page.locator('.mobile-workspace-nav a[href="/builds"]').click();
  assert.equal(await page.locator('.mobile-workspace-nav').getAttribute('open'), null);
  await page.waitForURL(`${origin}/builds`);
  assert.equal(await page.locator('h1').innerText(), 'Builds');
  await page.locator('.mobile-workspace-nav summary').click();
  await page.locator('.mobile-workspace-nav a[href="/developer-tools"]').click();
  await page.waitForFunction(() => document.querySelector('.tools-disclosure').open);
  assert.notEqual(await page.locator('.tools-disclosure').getAttribute('open'), null);
});

test('logout modal preserves page scroll and traps and restores focus', async () => {
  await login();
  await page.goto(`${origin}/repositories`);
  await page.locator('.app-content').evaluate(e => {e.scrollTop = 300;});
  const scroll = await page.evaluate(() => scrollY);
  let logoutRequests = 0;
  page.on('request', request => { if (request.method() === 'POST' && request.url().endsWith('/auth/logout')) logoutRequests++; });
  await openLogout();
  assert.equal(page.url(), `${origin}/repositories`);
  assert.equal(await page.evaluate(() => scrollY), scroll);
  assert.equal(await page.evaluate(() => document.activeElement.textContent.trim()), 'Cancel');
  for (const key of ['Tab', 'Tab', 'Tab', 'Shift+Tab', 'Shift+Tab', 'Shift+Tab']) {
    await page.keyboard.press(key);
    assert(await page.locator('#logout-confirmation').evaluate(e => e.contains(document.activeElement)));
  }
  await page.getByRole('button', {name: 'Cancel', exact: true}).click();
  await page.waitForFunction(() => document.activeElement === document.querySelector('.account-menu summary'));
  await openLogout();
  await page.keyboard.press('Escape');
  assert.equal(await page.locator('#logout-confirmation').evaluate(e => e.open), false);
  await openLogout();
  await page.mouse.click(5, 5);
  assert.equal(await page.locator('#logout-confirmation').evaluate(e => e.open), false);
  assert.equal(logoutRequests, 0);
  assert.equal((await context.request.get(`${origin}/dashboard`, {maxRedirects: 0})).status(), 200);
});

test('modal works on session page and confirmed logout invalidates session', async () => {
  await login();
  await page.goto(`${origin}/protected`);
  await openLogout();
  assert.equal(page.url(), `${origin}/protected`);
  await page.getByRole('button', {name: 'Cancel', exact: true}).click();
  await page.goto(`${origin}/auth/logout`);
  await page.waitForURL(`${origin}/dashboard?confirm=logout`);
  await page.waitForFunction(() => document.querySelector('#logout-confirmation').open);
  assert.equal((await context.request.post(`${origin}/auth/logout`, {form: {csrf_token: 'wrong'}, maxRedirects: 0})).status(), 403);
  for (const width of [320, 390, 768, 1440]) {
    await page.setViewportSize({width, height: 844});
    await assertNoOverflow();
  }
  await page.getByRole('button', {name: 'Confirm logout', exact: true}).click();
  await page.waitForURL(`${origin}/`);
  assert.equal((await context.request.get(`${origin}/dashboard`, {maxRedirects: 0})).status(), 303);
});

test('queue feedback handles pending duplicate failures and expired session', async () => {
  await login();
  await page.goto(`${origin}/developer-tools`);
  const pingInput=page.locator('#pr-key');
  await pingInput.click();
  assert.equal(await pingInput.evaluate(e => getComputedStyle(e).outlineStyle),'none');
  assert.equal(await pingInput.evaluate(e => getComputedStyle(e).boxShadow),'none');
  const submit = page.getByRole('button', {name: 'Send a ping', exact: true});
  const result = page.locator('[data-queue-result]');
  await submit.click();
  await page.locator('[data-queue-result][data-state=success]').waitFor();
  assert.match(await result.innerText(), /Job #17/);
  await page.route('**/queue/ping', route => route.fulfill({status: 202, contentType: 'application/json', body: '{"job_id":17,"duplicate":true}'}));
  await submit.click();
  await page.waitForFunction(() => document.querySelector('[data-queue-result]').textContent.includes('already exists'));
  await page.unroute('**/queue/ping');
  for (const [status, message] of [[400, /Check the key/], [403, /Refresh/], [503, /could not be queued/]]) {
    await page.route('**/queue/ping', route => route.fulfill({status, body: 'fixture failure'}));
    await submit.click();
    await page.locator('[data-queue-result][data-state=error]').waitFor();
    assert.match(await result.innerText(), message);
    await page.unroute('**/queue/ping');
  }
  let pendingRoute;
  await page.route('**/queue/ping', route => { pendingRoute = route; });
  await submit.click();
  assert(await submit.isDisabled());
  assert.equal(await page.locator('[data-queue-form]').getAttribute('aria-busy'), 'true');
  assert.match(await result.innerText(), /Sending/);
  await pendingRoute.abort();
  await page.locator('[data-queue-result][data-state=error]').waitFor();
  assert.match(await result.innerText(), /Could not reach/);
  await page.unroute('**/queue/ping');
  const token = await page.locator('#logout-confirmation input[name=csrf_token]').inputValue();
  assert.equal((await context.request.post(`${origin}/auth/logout`, {form: {csrf_token: token}, maxRedirects: 0})).status(), 303);
  await submit.click();
  await page.locator('[data-queue-result][data-state=error]').waitFor();
  // CSRF runs before RequireUser: an expired session rejects this stale form.
  assert.match(await result.innerText(), /session check failed/);
});

test('the home page story video loads nothing until played and has a transcript', async () => {
  await page.goto(origin);
  await page.getByRole('link', {name: /^Watch the story/}).click();
  assert.equal(new URL(page.url()).hash, '#watch');
  const video = page.locator('[data-film-video]');
  assert.equal(await video.getAttribute('preload'), 'none');
  assert.equal(await video.evaluate(v => v.readyState), 0, 'nothing downloads before play');
  assert.equal((await context.request.get(`${origin}/static/video/previa-story-poster.jpg`)).status(), 200);
  const play = page.getByRole('button', {name: /Play the story/});
  assert(await play.isVisible());
  assert.equal(await video.evaluate(v => v.controls), false, 'one play button at a time');
  await play.click();
  await page.waitForFunction(() => document.querySelector('[data-film]').classList.contains('is-playing'));
  assert.equal(await play.isVisible(), false);
  assert.equal(await video.evaluate(v => v.controls), true, 'native controls take over while it plays');
  await page.locator('.film-transcript summary').click();
  assert.match(await page.locator('.film-transcript ol').innerText(), /Minutes, not an afternoon/);
  for (const width of [320, 390, 768, 1440]) {
    await page.setViewportSize({width, height: 844});
    await assertNoOverflow();
  }
});

test('home reveals once pauses offscreen and respects reduced motion', async () => {
  await page.goto(origin);
  for (const width of [320, 390, 768, 1024, 1440]) {
    await page.setViewportSize({width, height: 844});
    await assertNoOverflow();
  }
  assert(await page.locator('.scroll-pending').count() > 0);
  await page.locator('#features').scrollIntoViewIfNeeded();
  await page.waitForFunction(() => document.querySelector('#features [data-scroll-reveal]').classList.contains('is-visible'));
  await page.waitForFunction(() => getComputedStyle(document.querySelector('.preview-window')).animationPlayState === 'paused');
  await page.evaluate(() => scrollTo({top: 0, behavior: 'instant'}));
  await page.waitForFunction(() => getComputedStyle(document.querySelector('.preview-window')).animationPlayState === 'running');
  assert(await page.locator('#features .is-visible').count() > 0);
  await page.emulateMedia({reducedMotion: 'reduce'});
  assert.equal(await page.locator('.preview-window').evaluate(e => getComputedStyle(e).animationName), 'none');
  assert.equal(await page.locator('.scroll-pending').count(), 0);
});

test('how it works asks one question at a time and jumps to any stage', async () => {
  await page.goto(origin);
  await page.getByRole('link', {name: 'How it works', exact: true}).first().click();
  assert.equal(new URL(page.url()).hash, '#how-it-works');
  const story = page.locator('[data-story]');
  const turns = story.locator('[data-story-turn]');
  const next = story.locator('[data-story-next]');
  assert.equal(await turns.count(), 0);
  assert.match(await next.innerText(), /I just opened a pull request/);
  await next.click();
  // Previa "types" first; the next question waits until the answer arrives.
  assert.equal(await next.getAttribute('aria-disabled'), 'true');
  await page.waitForFunction(() => !document.querySelector('[data-story-next]').hasAttribute('aria-disabled'));
  assert(await story.locator('#story-discover [data-story-answer]').isVisible());
  assert.match(await story.locator('#story-discover').innerText(), /I pick it up/);
  assert.match(await story.locator('[data-story-stage][aria-current="step"]').innerText(), /^01\s*Notice/);
  assert.match(await next.innerText(), /when does it actually build/);
  assert(await page.evaluate(() => document.activeElement.matches('[data-story-next]')));
  // A stage reveals every turn up to it and moves focus there.
  await story.locator('[data-story-stage]', {hasText: 'Share'}).click();
  assert.equal(await turns.count(), 5);
  await page.waitForFunction(() => document.activeElement?.id === 'story-route');
  assert.equal(await story.locator('[data-story-count]').innerText(), '5 / 7');
  await story.locator('[data-story-all]').click();
  assert.equal(await turns.count(), 7);
  await story.locator('[data-story-outro]').waitFor();
  assert.equal(await next.isVisible(), false);
  await story.locator('[data-story-again]').click();
  assert.equal(await turns.count(), 0);
  assert(await page.evaluate(() => document.activeElement.matches('[data-story-next]')));
  // With reduced motion the answer arrives at once.
  await page.emulateMedia({reducedMotion: 'reduce'});
  await next.click();
  assert(await story.locator('#story-discover [data-story-answer]').isVisible());
  assert.equal(await story.locator('[data-story-typing]').isVisible(), false);
  // A shared stage link opens the conversation at that stage.
  await page.goto('about:blank');
  await page.goto(`${origin}/#story-build`);
  await page.waitForFunction(() => document.activeElement?.id === 'story-build');
  assert.equal(await turns.count(), 3);
  for (const width of [320, 390, 768, 1024, 1440]) {
    await page.setViewportSize({width, height: 844});
    await assertNoOverflow();
  }
});

test('the FAQ answers as a conversation on the home page, from a link and in the workspace', async () => {
  await page.goto(origin);
  await page.getByRole('link', {name: 'FAQ', exact: true}).first().click();
  assert.equal(new URL(page.url()).hash, '#faq');
  const faq = page.locator('#faq [data-faq]');
  const item = faq.locator('#faq-fork-pull-requests');
  await item.locator('summary').click();
  assert.equal(await item.evaluate(e => e.classList.contains('is-typing')), true);
  await page.waitForFunction(() => !document.querySelector('#faq-fork-pull-requests').classList.contains('is-typing'));
  assert.match(await item.locator('.faq-answer .chat-bubble').innerText(), /never receive your secrets/);
  // Topics and search narrow the questions; a status line counts them.
  const visible = faq.locator('[data-faq-item]:not([hidden])');
  await faq.getByRole('button', {name: /^Security/}).click();
  assert.equal(await visible.count(), 5);
  assert.deepEqual([...new Set(await visible.evaluateAll(items => items.map(i => i.dataset.topic)))], ['security']);
  await faq.getByRole('button', {name: /^All topics/}).click();
  await faq.locator('[data-faq-search]').fill('frameworks');
  assert.equal(await visible.count(), 1);
  assert.equal(await faq.locator('[data-faq-status]').innerText(), '1 of 24 questions shown.');
  await faq.locator('[data-faq-search]').fill('zzz-nothing');
  assert(await faq.locator('[data-faq-empty]').isVisible());
  await faq.getByRole('button', {name: 'show every question', exact: true}).click();
  assert.equal(await visible.count(), 24);
  // A shared #faq-… link opens its answer.
  await page.goto('about:blank');
  await page.goto(`${origin}/#faq-secrets`);
  await page.waitForFunction(() => document.querySelector('#faq-secrets').open);
  // In the workspace the signed-in owner asks, from the sidebar or the palette.
  await login();
  await page.locator('.app-sidebar a[href="/faq"]').click();
  await page.waitForURL(`${origin}/faq`);
  assert.equal(await page.locator('h1').innerText(), 'Frequently asked questions');
  assert.equal(await page.locator('.app-sidebar a[href="/faq"]').getAttribute('aria-current'), 'page');
  await page.locator('#faq-build-disabled summary').click();
  await page.locator('#faq-build-disabled .faq-answer .chat-bubble').waitFor();
  assert.equal(await page.locator('#faq-build-disabled .chat-avatar.is-you').innerText(), 'D');
  for (const width of [320, 390, 768, 1024, 1440]) {
    await page.setViewportSize({width, height: 844});
    await assertNoOverflow();
  }
  await page.goto(`${origin}/dashboard`);
  await page.keyboard.press('Control+k');
  await page.waitForFunction(() => document.querySelector('#command-palette').open);
  await page.locator('#command-input').fill('faq');
  assert.equal(await page.locator('[data-command-item]:not([hidden])').count(), 1);
  await page.keyboard.press('Enter');
  await page.waitForURL(`${origin}/faq`);
});

test('content and native confirmation remain usable without JavaScript', async () => {
  const noJS = await browser.newContext({javaScriptEnabled: false});
  try {
    const nativePage = await noJS.newPage();
    await nativePage.goto(origin);
    assert.equal(await nativePage.locator('#security .security-grid').evaluate(e => getComputedStyle(e).opacity), '1');
    // How it works is a full transcript, and FAQ questions open natively.
    assert.equal(await nativePage.locator('[data-story-turn]').count(), 7);
    assert.equal(await nativePage.locator('[data-story-outro]').isVisible(), true);
    assert.equal(await nativePage.locator('[data-story-composer]').isVisible(), false);
    assert.equal(await nativePage.locator('[data-faq-tools]').isVisible(), false);
    assert.equal(await nativePage.locator('[data-film-play]').isVisible(), false);
    assert.equal(await nativePage.locator('[data-film-video]').getAttribute('controls'), '');
    await nativePage.locator('#faq-secrets summary').click();
    assert.match(await nativePage.locator('#faq-secrets .faq-answer').innerText(), /Encrypted as soon as you save them/);
    await nativePage.goto(`${origin}/auth/github/login`);
    await nativePage.waitForURL(`${origin}/dashboard`);
    await nativePage.goto(`${origin}/repositories?page=2`);
    assert.equal(await nativePage.locator('.repository-row').count(),10);
    await nativePage.getByRole('link',{name:'Previous page',exact:true}).click();
    assert.equal(new URL(nativePage.url()).searchParams.get('page'),'1');
    await nativePage.locator('#repository-search').fill('verification-149');
    await nativePage.locator('#repository-search').press('Enter');
    await nativePage.waitForURL(url => url.searchParams.get('q') === 'verification-149');
    assert.equal(await nativePage.locator('.repository-row').count(),1);
    await nativePage.locator('.account-menu summary').click();
    await nativePage.getByRole('button', {name: 'Log out', exact: true}).click();
    assert(await nativePage.locator('#logout-confirmation').evaluate(e => e.matches(':modal')));
    await nativePage.getByRole('button', {name: 'Cancel', exact: true}).click();
    assert.equal(await nativePage.locator('#logout-confirmation').evaluate(e => e.open), false);
    // The teardown confirmation is a native modal too, and its form posts.
    await nativePage.goto(`${origin}/repositories/1/pull-requests/3`);
    await nativePage.getByRole('button', {name: 'Tear down preview', exact: true}).click();
    assert(await nativePage.locator('#teardown-preview').evaluate(e => e.matches(':modal')));
    await nativePage.locator('#teardown-preview').getByRole('button', {name: 'Tear down preview', exact: true}).click();
    await nativePage.waitForURL(url => url.pathname === '/repositories/1/pull-requests/3');
    assert.match(await nativePage.locator('.pr-detail-grid').innerText(), /Removing/);
  } finally { await noJS.close(); }
});

test('owner builds once and tears a preview down only after confirming in a modal', async () => {
  await login();
  await page.goto(`${origin}/repositories/1/pull-requests/5`);
  await page.getByRole('button', {name: 'Build latest commit', exact: true}).click();
  await page.waitForFunction(() => /Queued/.test(document.querySelector('.pr-detail-grid')?.innerText || ''));
  assert.equal(await page.getByRole('button', {name: 'Build latest commit'}).count(), 0, 'a queued request offers no second build');

  await page.goto(`${origin}/repositories/1/pull-requests/3`);
  const preview = page.locator('[data-preview-url]');
  assert.equal(await preview.getAttribute('href'), 'https://pr-3-1-fixture.preview.example.com');
  assert.equal(await preview.getAttribute('target'), '_blank');
  assert.equal(await page.getByRole('link', {name: 'Open preview', exact: true}).getAttribute('href'), '/repositories/1/pull-requests/3/open', 'Open preview records the open; the address stays direct');
  await context.grantPermissions(['clipboard-read', 'clipboard-write'], {origin});
  const copy = page.getByRole('button', {name: 'Copy the preview address', exact: true});
  await copy.click();
  await page.waitForFunction(() => document.querySelector('[data-copy] [data-copy-status]')?.textContent === 'Copied');
  assert.equal(await page.evaluate(() => navigator.clipboard.readText()), 'https://pr-3-1-fixture.preview.example.com');
  assert.match(await page.locator('.pr-detail-grid').innerText(), /Expires/);
  assert.match(await page.locator('.pr-detail-grid').innerText(), /earlier commit/);
  for (const width of [320, 390, 768, 1440]) {
    await page.setViewportSize({width, height: 844});
    await assertNoOverflow();
  }
  await page.setViewportSize({width: 1440, height: 900});
  const open = page.getByRole('button', {name: 'Tear down preview', exact: true});
  await open.click();
  await page.waitForFunction(() => document.querySelector('#teardown-preview').matches(':modal'));
  await page.keyboard.press('Escape');
  await page.waitForFunction(() => !document.querySelector('#teardown-preview').open);
  assert.equal(await page.evaluate(() => document.activeElement?.textContent.trim()), 'Tear down preview', 'focus returns to the trigger');
  assert.match(await page.locator('.pr-detail-grid').innerText(), /Running/, 'cancelling changes nothing');
  await open.click();
  await page.waitForFunction(() => document.querySelector('#teardown-preview').matches(':modal'));
  await page.locator('#teardown-preview').getByRole('button', {name: 'Tear down preview', exact: true}).click();
  await page.waitForFunction(() => /Removing/.test(document.querySelector('.pr-detail-grid')?.innerText || ''));
  assert.match(await page.locator('.pr-detail-grid').innerText(), /tore this preview down/);
  assert.equal(await page.getByRole('button', {name: 'Tear down preview'}).count(), 0);
});

test('owners approve outbound destinations and remove them only after confirming in a modal', async () => {
  await login();
  await page.goto(`${origin}/repositories/1/outbound`);
  assert.equal(await page.getByRole('link', {name: 'Outbound', exact: true}).getAttribute('aria-current'), 'page');
  assert.match(await page.locator('main').innerText(), /No destinations approved/);
  // Addresses are refused with the reason, next to the field.
  await page.getByLabel('Host').fill('10.0.0.1');
  await page.getByRole('button', {name: 'Approve', exact: true}).click();
  await page.waitForSelector('#destination-host-error');
  assert.match(await page.locator('#destination-host-error').innerText(), /not an IP address/);
  assert.equal(await page.getByLabel('Host').getAttribute('aria-invalid'), 'true');
  await page.getByLabel('Host').fill('db.example.com');
  await page.getByLabel('Port').selectOption('5432');
  await page.getByRole('button', {name: 'Approve', exact: true}).click();
  await page.waitForFunction(() => /Approved db\.example\.com:5432/.test(document.querySelector('main')?.innerText || ''));
  assert.match(await page.locator('.outbound-table').innerText(), /db\.example\.com/);
  // A used port is no longer offered.
  assert.deepEqual(await page.getByLabel('Port').locator('option').evaluateAll((options) => options.map((o) => o.value)), ['6379']);
  for (const width of [320, 390, 768]) {
    await page.setViewportSize({width, height: 844});
    await assertNoOverflow();
  }
  await page.setViewportSize({width: 1440, height: 900});
  const remove = page.getByRole('button', {name: 'Remove db.example.com:5432', exact: true});
  await remove.click();
  await page.waitForFunction(() => document.querySelector('dialog[id^="remove-destination-"]')?.matches(':modal'));
  await page.keyboard.press('Escape');
  await page.waitForFunction(() => !document.querySelector('dialog[id^="remove-destination-"]').open);
  assert.equal(await page.evaluate(() => document.activeElement?.textContent.trim()), 'Remove', 'focus returns to the trigger');
  await remove.click();
  await page.locator('dialog[id^="remove-destination-"]').getByRole('button', {name: 'Remove destination', exact: true}).click();
  await page.waitForFunction(() => /Removed db\.example\.com:5432/.test(document.querySelector('main')?.innerText || ''));
  assert.equal(await page.locator('.outbound-table').count(), 0);
  // A repository without GitHub access says why and offers no add form.
  await page.goto(`${origin}/repositories/2/outbound`);
  assert.match(await page.locator('main').innerText(), /GitHub access to this repository was removed/);
  assert.equal(await page.getByRole('button', {name: 'Approve', exact: true}).count(), 0);
  assert.equal(await page.locator('form.outbound-form').count(), 0);
});

test('unavailable repository PRs offer no actions and say why', async () => {
  await login();
  await page.goto(`${origin}/repositories/2/pull-requests/1`);
  assert.match(await page.locator('[data-build-unavailable]').innerText(), /GitHub access to this repository is unavailable[\s\S]*choose repositories on GitHub/);
  assert.equal(await page.getByRole('button', {name: 'Build latest commit', exact: true}).count(), 0, 'no dead Build button');
  assert.equal(await page.getByRole('button', {name: /Retry|Rebuild|Tear down|Cancel build/}).count(), 0);
  assert.equal(await page.locator('form[action$="/build"]').count(), 0, 'nothing can be submitted');
});

test('a blocked build offers no dead button: it says the reason and the fix', async () => {
  await login();
  await page.goto(`${origin}/repositories/1/pull-requests/9`);
  assert.equal(await page.getByRole('button', {name: 'Build latest commit', exact: true}).count(), 0);
  assert.equal(await page.locator('main button[disabled]').count(), 0);
  assert.match(await page.locator('#build-unavailable').innerText(), /no preview domain[\s\S]*PREVIEW_BASE_DOMAIN/);
  assert.equal(await page.locator('form[action$="/build"]').count(), 0, 'nothing can be submitted');
  assert.match(await page.locator('[data-pr-status] h2').innerText(), /No preview yet/);
  for (const width of [320, 390, 1440]) {
    await page.setViewportSize({width, height: 844});
    await assertNoOverflow();
  }
});

test('a failed build says what went wrong and what to do, with the technical details one click away', async () => {
  await login();
  await page.goto(`${origin}/repositories/1/pull-requests/4`);
  const status = page.locator('[data-pr-status]');
  assert.equal(await status.locator('h2').innerText(), "GitHub didn't let Previa download your code");
  assert.match(await status.locator('.pr-status-kicker').innerText(), /Build failed/);
  assert.equal(await status.locator('.tracker-step.is-failed').getAttribute('data-stage'), 'code');
  assert.deepEqual(await status.locator('.tracker-step.is-done').evaluateAll(steps => steps.map(step => step.dataset.stage)), ['queue']);
  const steps = await status.locator('.diagnosis-steps li').allInnerTexts();
  assert.match(steps[0], /Contents to Read-only/);
  assert.equal(steps.at(-1), 'Choose Retry build.');
  const details = status.locator('.diagnosis-details');
  assert.equal(await details.evaluate(element => element.open), false, 'technical details start collapsed');
  await details.locator('summary').click();
  assert.match(await details.locator('.diagnosis-reason').innerText(), /GitHub denied access to this commit's files/);
  assert.match(await details.innerText(), /source_failed/);
  assert.equal(await details.getByRole('link', {name: 'Show recent logs'}).getAttribute('href'), '/repositories/1/pull-requests/4/logs');
  assert.match(await page.locator('[data-failure]').innerText(), /GitHub didn't let Previa download your code/);
  for (const width of [320, 390, 768, 1440]) {
    await page.setViewportSize({width, height: 844});
    await assertNoOverflow();
  }
  // Retry says it is starting while the request is on its way, then shows the queued step.
  let release;
  const held = new Promise(resolve => { release = resolve; });
  await page.route('**/repositories/1/pull-requests/4/build', async route => { await held; await route.continue(); });
  await page.getByRole('button', {name: 'Retry build', exact: true}).click();
  const starting = page.getByRole('button', {name: 'Starting…', exact: true});
  await starting.waitFor();
  assert(await starting.isDisabled(), 'a starting build cannot be sent twice');
  assert(await starting.locator('.spinner').isVisible());
  release();
  await page.waitForFunction(() => document.querySelector('.build-tracker .is-current')?.dataset.stage === 'queue');
  await page.unroute('**/repositories/1/pull-requests/4/build');
  assert.equal(await page.locator('[data-diagnosis]').count(), 0, 'a new request replaces the old explanation');
});

test('a running build follows its log to the current step, with a clock and the latest line', async () => {
  await context.request.post(`${origin}/__fixture/logs/seed`);
  await login();
  await page.goto(`${origin}/repositories/1/pull-requests/6`);
  await liveIdle();
  // A live refresh swaps in the server's tracker (queue done, the rest working)
  // until its log stream repaints it, so read each state in one snapshot.
  const trackerAt = async stage => (await page.waitForFunction(at => {
    const tracker = document.querySelector('[data-pr-status] .build-tracker');
    const current = tracker?.querySelector('.is-current');
    if (current?.dataset.stage !== at) return false;
    return {
      done: [...tracker.querySelectorAll('.is-done')].map(step => step.dataset.stage),
      text: tracker.querySelector('[data-progress-text]')?.textContent || '',
      status: current.querySelector('.sr-only')?.textContent || '',
    };
  }, stage)).jsonValue();
  // Output from the image build means the code is in and the image is building.
  const building = await trackerAt('image');
  assert.deepEqual(building.done, ['queue', 'code']);
  assert.equal(building.text, 'indented output');
  await context.request.post(`${origin}/__fixture/logs/append`, {form: {text: 'Pulling the postgres:17 image.', system: '1'}});
  const starting = await trackerAt('start');
  assert.equal(starting.text, 'Pulling the postgres:17 image.');
  assert.match(starting.status, /in progress/);
  const clock = page.locator('time[data-elapsed]');
  const first = await clock.innerText();
  assert.match(first, /^\d+m \d+s$/);
  await page.waitForFunction(text => document.querySelector('time[data-elapsed]').textContent !== text, first);
  assert(await page.getByRole('link', {name: 'Follow the build log', exact: true}).isVisible());
  for (const width of [320, 390, 768, 1440]) {
    await page.setViewportSize({width, height: 844});
    await assertNoOverflow();
  }
});

test('failures need attention for a week, then stay on Builds quietly, and can be dismissed or undone', async () => {
  await login();
  // Overview: the recent failure is red and can be dismissed; older ones are counted apart, not in red.
  await page.goto(`${origin}/__fixture/overview?state=busy`);
  await page.waitForURL(`${origin}/dashboard`);
  const kpi = page.locator('.ov-kpi', {hasText: 'Needs attention'});
  assert.match(await kpi.innerText(), /1\s+failed build in the last 7 days/);
  assert.equal(await kpi.getAttribute('href'), '/builds?failures=attention');
  const older = page.locator('#ov-panel-builds .ov-row', {hasText: 'Fixture pull request 5'});
  assert.match(await older.locator('.ov-col-status .badge').getAttribute('class'), /tone-neutral/);
  assert.match(await older.locator('.ov-col-when').innerText(), /Over 7 days old/);
  assert.match(await older.locator('.ov-col-reason').getAttribute('class'), /is-quiet/);
  const recent = page.locator('#ov-panel-builds .ov-row', {hasText: 'Fixture pull request 4'});
  await recent.locator('summary').click();
  const dismiss = recent.locator('form.ov-row-dismiss');
  assert.equal(await dismiss.getAttribute('action'), '/repositories/1/pull-requests/4/failure/dismiss');
  assert.equal(await dismiss.locator('input[name="revision"]').inputValue(), '3');
  assert.equal(await dismiss.locator('input[name="return"]').inputValue(), 'overview');
  assert(await dismiss.getByRole('button', {name: 'Dismiss this failure'}).isVisible());
  // Builds: filters, a quiet older row, Dismiss per row, and Undo after a dismissal.
  await page.goto(`${origin}/builds?fixture=rows&failures=older`);
  assert.equal(await page.locator('.activity-filters a[aria-current="page"]').innerText(), 'Older failures');
  assert.equal(await page.locator('.activity-filters a', {hasText: 'Needs attention'}).getAttribute('href'), '/builds?failures=attention&page=1');
  const oldRow = page.locator('.activity-table tr', {hasText: '#5'});
  assert.match(await oldRow.locator('.badge').getAttribute('class'), /tone-neutral/);
  assert.match(await oldRow.innerText(), /Over 7 days old/);
  const rowDismiss = page.getByRole('button', {name: 'Dismiss the failure of fixture-owner/repository-1 #5'});
  assert.equal(await rowDismiss.locator('xpath=ancestor::form').getAttribute('action'), '/repositories/1/pull-requests/5/failure/dismiss');
  assert.equal(await rowDismiss.locator('xpath=ancestor::form').locator('input[name="failures"]').inputValue(), 'older');
  await page.goto(`${origin}/builds?fixture=rows&dismissed=1-4`);
  assert.match(await page.locator('.activity-notice').innerText(), /Failure dismissed/);
  assert.equal(await page.locator('.activity-notice form').getAttribute('action'), '/repositories/1/pull-requests/4/failure/restore');
  for (const width of [320, 390, 768, 1440]) {
    await page.setViewportSize({width, height: 844});
    await assertNoOverflow();
  }
  // An empty filter says so and leads back to every build.
  await page.goto(`${origin}/builds?failures=dismissed`);
  assert.match(await page.locator('.activity-empty h2').innerText(), /Nothing dismissed/);
});

test('Previews show what is live with its link, Builds show attempts, and teardown confirms per row', async () => {
  await login();
  // The page's former address still works.
  await page.goto(`${origin}/environments?fixture=rows`);
  await page.waitForURL(url => url.pathname === '/previews' && url.searchParams.get('fixture') === 'rows');
  assert.equal(await page.locator('h1').innerText(), 'Previews');
  assert.equal(await page.locator('.app-sidebar a[href="/previews"]').getAttribute('aria-current'), 'page');
  assert.match(await page.locator('.page-header-text [role=tooltip]').textContent(), /What is live now/);
  assert.ok(await page.locator('.page-header-text [role=tooltip]').isHidden(), 'the description waits in its tooltip');
  const preview = page.getByRole('link', {name: 'Open the preview of fixture-owner/repository-1 #3 in a new tab'});
  assert.equal(await preview.getAttribute('href'), '/repositories/1/pull-requests/3/open');
  assert.equal(await preview.getAttribute('target'), '_blank');
  // Status is what reviewers care about: live, even while a newer build failed.
  assert.deepEqual(await page.locator('.activity-table .pr-cell-state .badge').allInnerTexts(), ['Live', 'Live · update failed']);
  assert.match(await page.locator('.activity-table').innerText(), /Newer build failed: Your app didn't become ready/);
  // A row is not one big link: clicking it stays here; the title opens the pull request.
  await page.locator('.activity-table tr', {hasText: '#3'}).locator('.pr-cell-sha').click();
  assert.equal(new URL(page.url()).pathname, '/previews');
  assert.match(await page.locator('.activity-table').innerText(), /Earlier than the latest commit/);
  assert.match(await page.locator('.activity-table').innerText(), /Your app didn't become ready/);
  assert.equal(await page.locator('.activity-table [title="The app did not become ready."]').count(), 1, 'the recorded sentence stays one hover away');
  for (const width of [320, 390, 768, 1440]) {
    await page.setViewportSize({width, height: 844});
    await assertNoOverflow();
  }
  await page.setViewportSize({width: 1440, height: 900});
  await page.getByRole('button', {name: 'Tear down the preview of fixture-owner/repository-1 #3'}).click();
  await page.waitForFunction(() => document.querySelector('#teardown-1-3').matches(':modal'));
  await page.locator('#teardown-1-3').getByRole('button', {name: 'Tear down preview', exact: true}).click();
  await page.waitForURL(url => url.pathname === '/previews' && url.searchParams.get('page') === '1');
  await page.goto(`${origin}/builds?fixture=rows`);
  assert.match(await page.locator('.page-header-text [role=tooltip]').textContent(), /latest attempt to build/);
  assert.match(await page.locator('.activity-table').innerText(), /Waiting for capacity/);
  // Row controls stay clickable above the row's whole-row link.
  await page.getByRole('link', {name: 'Build logs for fixture-owner/repository-1 #4'}).click();
  await page.waitForURL(url => url.pathname === '/repositories/1/pull-requests/4/logs');
});

test('repository pagination preserves filters and connection action is in the header', async () => {
  await login();
  await page.goto(`${origin}/repositories?q=fixture&access=available`);
  assert.equal(await page.locator('.repository-row').count(), 10);
  assert.equal(await page.locator('.repository-row .unavailable').count(), 0);
  const form = page.locator('form[action="/installations/start"]');
  assert.equal(await form.getAttribute('method'), 'post');
  assert(await form.locator('[name="csrf_token"]').inputValue());
  const manage = page.getByRole('button', {name: 'Choose repositories on GitHub'});
  assert(await manage.isEnabled());
  assert.equal(await manage.evaluate(e => getComputedStyle(e).cursor), 'pointer');
  assert.equal(await page.locator('.workspace-page-heading form').count(), 1);
  await page.getByRole('link', {name:'Next page',exact:true}).click();
  await page.waitForURL(url => url.searchParams.get('page') === '2');
  const location = new URL(page.url());
  assert.equal(location.searchParams.get('page'), '2');
  assert.equal(location.searchParams.get('q'), 'fixture');
  assert.equal(location.searchParams.get('access'), 'available');
  assert.match(await page.locator('.repository-pagination p').innerText(), /11–20 of 75/);
  assert.equal(await page.locator('.repository-row').count(), 10);
  for (const width of [320,390,768,1440]) { await page.setViewportSize({width,height:844}); await assertNoOverflow(); }
});


test('repository live search restores all records and keeps the input focused', async () => {
  await login();
  await page.goto(`${origin}/repositories?page=3`);
  await page.evaluate(() => {window.repositoryDocument = document;});
  const search = page.locator('#repository-search');
  assert.equal(await page.locator('#repository-filters button').count(),0);
  assert(await page.getByRole('radio',{name:'Available',exact:true}).isChecked());
  assert.equal(await page.locator('#repository-filters select').count(),0);
  assert.equal(await page.locator('#repository-filters a').count(),0);
  await search.click();
  assert.equal(await search.evaluate(e => getComputedStyle(e).outlineStyle),'none');
  assert.equal(await search.evaluate(e => getComputedStyle(e).boxShadow),'none');
  await search.fill('verification-149');
  await page.waitForFunction(() => document.querySelectorAll('.repository-row').length === 1);
  assert.match(await page.locator('.repository-row').innerText(), /verification-149/);
  assert.equal(await search.evaluate(e => e === document.activeElement),true);
  assert(await page.evaluate(() => document === window.repositoryDocument));
  assert.equal(new URL(page.url()).searchParams.get('page'),null);
  await search.fill('no-such-repository');
  await page.getByRole('heading',{name:'No repositories match these filters.'}).waitFor();
  await search.fill('');
  await page.waitForFunction(() => document.querySelector('.repository-pagination p')?.textContent.includes('of 75'));
  assert.equal(await page.locator('.repository-row').count(),10);
  assert.equal(new URL(page.url()).searchParams.get('q'),'');
  await page.locator('.repository-access-tabs').getByText('All',{exact:true}).click();
  await page.waitForFunction(() => document.querySelector('.repository-pagination p')?.textContent.includes('of 150'));
  await page.locator('.repository-access-tabs').getByText('Available',{exact:true}).click();
  await page.waitForFunction(() => document.querySelector('.repository-pagination p')?.textContent.includes('of 75'));
  assert.equal(await page.locator('.repository-row .unavailable').count(),0);
  await page.route('**/repositories?*',route => route.fulfill({status:503,body:'fixture unavailable'}));
  await search.fill('verification-1');
  await page.waitForFunction(() => document.querySelector('[data-repository-feedback]')?.hasAttribute('data-error'));
  assert.equal(await page.locator('.repository-row').count(),10);
});

test('PR workspace live search pagination modal and build control', async () => {
  await login();
  await page.goto(`${origin}/repositories/1`);
  assert.equal(await page.locator('.pr-table tbody tr').count(),10);
  await page.getByRole('link',{name:'Next',exact:true}).click();
  await page.waitForURL(/page=2/);
  assert.match(await page.locator('.pr-table tbody tr').first().innerText(),/#11/);
  const search=page.locator('#pull-request-search');
  await liveIdle();
  await search.fill('preview 25');
  await page.waitForFunction(()=>document.querySelectorAll('.pr-table tbody tr').length===1);
  await search.fill('');
  await page.waitForFunction(()=>document.querySelectorAll('.pr-table tbody tr').length===10);
  await page.getByRole('switch',{name:'Automatic builds',exact:true}).click();
  await page.waitForFunction(()=>document.querySelector('#build-policy').matches(':modal'));
  assert.equal(new URL(page.url()).pathname,'/repositories/1');
  await page.keyboard.press('Escape');
  for(const width of [320,390,768,1440]) {
    await page.setViewportSize({width,height:844});
    await assertNoOverflow();
    assert.equal(await page.locator('.app-header').evaluate(e=>Math.round(e.getBoundingClientRect().top)),0);
    assert(await page.locator('#pull-request-results .repository-table-scroll').evaluate(e=>e.clientHeight>60),'table must remain usable');
  }
  await page.locator('.pr-table tbody .pr-cell-text a').first().click();
  await page.waitForURL(url=>url.pathname.includes('/pull-requests/'));
  assert(await page.getByRole('button',{name:'Build latest commit'}).isEnabled());
  assert.match(await page.locator('.pr-detail-grid').innerText(),/No build requested yet/);
});

test('PR search and pagination work without JavaScript', async () => {
  const noJS=await browser.newContext({javaScriptEnabled:false,viewport:{width:1280,height:900}});
  try {
    const p=await noJS.newPage();
    await p.goto(`${origin}/auth/github/login`);
    await p.waitForURL(`${origin}/dashboard`);
    await p.goto(`${origin}/repositories/1`);
    await p.locator('#pull-request-search').fill('preview 25');
    await p.getByRole('button',{name:'Apply filters',exact:true}).click();
    assert.equal(await p.locator('.pr-table tbody tr').count(),1);
    assert.match(await p.locator('.pr-table').innerText(),/#25/);
    await p.locator('.pr-table tbody .pr-cell-text a').click();assert.equal(new URL(p.url()).pathname,'/repositories/1/pull-requests/25');
    await p.locator('[data-trail-parent]').click();assert.equal(await p.locator('#pull-request-search').inputValue(),'preview 25');
    await p.getByRole('switch',{name:'Automatic builds',exact:true}).click();
    assert(await p.locator('#build-policy').evaluate(e=>e.matches(':modal')));
    await p.locator('#build-policy').getByRole('button',{name:'Enable automatic',exact:true}).click();
    assert.equal(await p.locator('#pull-request-search').inputValue(),'preview 25');assert.match(await p.locator('.pr-panel-heading').innerText(),/Automatic/);
  } finally {await noJS.close();}
});



test('Automatic confirms before enabling saves in place and live updates respect modal focus',async()=>{
 await login();await page.goto(`${origin}/repositories`);
 await page.locator('[data-repo-link="1"]').click();await readyPanel('1');
 await page.evaluate(()=>window.originalDocument=document);
 await page.locator('#pull-request-search').fill('preview 25');await page.waitForFunction(()=>document.querySelectorAll('.pr-table tbody tr').length===1);
 let policyRequests=0;
 page.on('request',request=>{if(request.method()==='POST' && request.url().endsWith('/repositories/1/policy')) policyRequests++;});
 const mode=page.getByRole('switch',{name:'Automatic builds',exact:true});
 assert.equal(await mode.getAttribute('aria-checked'),'false');
 await mode.click();await page.waitForFunction(()=>document.querySelector('#build-policy').matches(':modal'));
 assert.equal(await mode.getAttribute('aria-checked'),'false');assert.equal(policyRequests,0);
 await page.getByRole('button',{name:'Cancel',exact:true}).click();
 assert.equal(policyRequests,0);assert.equal(await mode.getAttribute('aria-checked'),'false');
 await mode.click();await page.waitForFunction(()=>document.querySelector('#build-policy').matches(':modal'));
 for(const key of ['Shift+Tab','Tab','Tab','Tab','Tab']) {await page.keyboard.press(key);assert(await page.locator('#build-policy').evaluate(e=>e.contains(document.activeElement)));}
 await page.route('**/repositories/1/policy',route=>route.fulfill({status:503,body:'fixture unavailable'}));
 await page.getByRole('button',{name:'Enable automatic',exact:true}).click();await page.locator('#build-policy [data-policy-feedback][data-error]').waitFor();
 assert(await page.locator('#build-policy').evaluate(e=>e.matches(':modal')));
 assert(await page.getByRole('button',{name:'Enable automatic',exact:true}).isEnabled());assert.equal(await mode.getAttribute('aria-checked'),'false');
 await page.unroute('**/repositories/1/policy');await page.getByRole('button',{name:'Enable automatic',exact:true}).click();
 await page.waitForFunction(()=>document.querySelector('.automatic-switch').getAttribute('aria-checked')==='true');await readyPanel('1');
 assert.equal(await page.locator('#build-policy').evaluate(e=>e.open),false);
 assert.equal(await page.locator('#pull-request-search').inputValue(),'preview 25');
 assert(await page.evaluate(()=>document===window.originalDocument));
 await mode.click();await page.waitForFunction(()=>document.querySelector('.automatic-switch').getAttribute('aria-checked')==='false');await readyPanel('1');
 assert.equal(await page.locator('#build-policy').evaluate(e=>e.open),false);
 await page.getByRole('button',{name:'Sync Pull Requests',exact:true}).click();
 await page.locator('.sync-state').filter({hasText:'Syncing'}).waitFor();await readyPanel('1');
 assert.equal(await page.getByRole('button',{name:'Sync Pull Requests',exact:true}).count(),0,'a running sync is a status, not a dead button');
 await mode.click();await page.waitForFunction(()=>document.querySelector('#build-policy').matches(':modal'));
 await page.waitForTimeout(3500);assert(await page.locator('#build-policy').evaluate(e=>e.matches(':modal')));assert.match(await page.locator('.pr-summary').innerText(),/Syncing/);
 const requestsBeforeEscape=policyRequests;await page.keyboard.press('Escape');assert.equal(policyRequests,requestsBeforeEscape);
 await page.waitForFunction(()=>!document.querySelector('.pr-summary').textContent.includes('Syncing'));
 await page.getByRole('button',{name:'Sync Pull Requests',exact:true}).waitFor();
 assert.equal(await mode.getAttribute('aria-checked'),'false');
 assert.equal(await page.locator('#pull-request-search').inputValue(),'preview 25');assert.equal(await page.locator('.pr-table tbody tr').count(),1);
 assert(await page.evaluate(()=>document===window.originalDocument));
});




test('unavailable repositories and empty PR states remain clear and cannot request mutations',async()=>{
 await login();await page.goto(`${origin}/repositories/2?access=all&pr_state=closed`);
 assert.match(await page.locator('.pr-summary').innerText(),/Repository unavailable/);
 assert.equal(await page.getByRole('button',{name:'Sync Pull Requests',exact:true}).count(),0);
 assert.equal(await page.getByRole('switch',{name:'Automatic builds',exact:true}).count(),0);
 assert.equal(await page.locator('.pr-table').count(),0);assert.match(await page.locator('#pull-request-results').innerText(),/No pull requests/);
 assert.equal(await page.locator('#repository-filters').count(),0);
});

test('expired sessions navigate through sign-in and return to the requested page',async()=>{
 await login();await page.goto(`${origin}/repositories/1`);
 const token=await page.locator('#logout-confirmation input[name=csrf_token]').inputValue();
 assert.equal((await context.request.post(`${origin}/auth/logout`,{form:{csrf_token:token},maxRedirects:0})).status(),303);
 await page.getByRole('link',{name:'Repositories',exact:true}).last().click();await page.waitForURL(`${origin}/repositories`);
 assert.equal(await page.locator('#repository-detail').count(),0);await page.locator('#repository-filters').waitFor();
});







test('dedicated repository and PR pages preserve filters through breadcrumbs and history',async()=>{
 await login();await page.goto(`${origin}/repositories?q=fixture&page=2`);
 assert.equal(await page.locator('#repository-detail').count(),0);
 await page.evaluate(()=>window.directoryDocument=document);
 await page.locator('[data-repo-link="25"]').click();await page.waitForURL(url=>url.pathname==='/repositories/25');
 assert.equal(await page.locator('#repository-filters').count(),0);
 assert.equal(await page.evaluate(()=>document===window.directoryDocument),false);
 assert.match(await page.locator('.pr-panel-heading').innerText(),/fixture-owner[\s\S]*repository-25/);
 assert.equal(await page.locator('[data-trail-parent]').innerText(),'Repositories','the header trail is the previous page and this one');
 const synced=page.locator('.pr-summary time');
 assert.equal(await synced.getAttribute('title'),'4 Oct 2026, 10:00 UTC');
 // A live refresh can swap the summary in just before ui.js makes it relative again.
 await page.waitForFunction(()=>{const t=document.querySelector('.pr-summary time');return t && t.textContent.trim()!==t.title;});
 assert.notEqual(await synced.innerText(),'4 Oct 2026, 10:00 UTC','shown relative, with the exact time as its title');
 await liveIdle();
 await page.locator('#pull-request-search').fill('preview 25');await page.waitForFunction(()=>document.querySelectorAll('.pr-table tbody tr').length===1);
 await page.locator('.pr-table tbody .pr-cell-text a').click();await page.waitForURL(url=>url.pathname==='/repositories/25/pull-requests/25');
 assert.match(await page.locator('.app-breadcrumb-current').innerText(),/#25.*A careful change to preview 25/);
 assert.equal(await page.locator('#pull-request-search').count(),0);
 assert.equal(await page.locator('.app-breadcrumb a').count(),1,'only the previous page is a link');
 await page.locator('.app-breadcrumb').getByRole('link',{name:'repository-25',exact:true}).click();await page.waitForURL(url=>url.pathname==='/repositories/25');
 assert.equal(await page.locator('#pull-request-search').inputValue(),'preview 25');assert.equal(await page.locator('.pr-table tbody tr').count(),1);
 await page.locator('[data-trail-parent]').click();await page.waitForURL(url=>url.pathname==='/repositories');
 assert.equal(await page.locator('#repository-search').inputValue(),'fixture');assert.equal(new URL(page.url()).searchParams.get('page'),'2');
 await page.goBack();await page.waitForURL(url=>url.pathname==='/repositories/25');
 assert.equal(await page.locator('#pull-request-search').inputValue(),'preview 25');
 await page.goForward();await page.waitForURL(url=>url.pathname==='/repositories');assert.equal(await page.locator('.repository-row').count(),10);
});

test('full pages align desktop filters and remain usable on short mobile and tablet screens',async()=>{
 await login();
 for(const route of ['/repositories','/repositories/1','/repositories/1/pull-requests/7']) {
  await page.goto(`${origin}${route}`);
  for(const width of [320,390,768,1024,1440,1917]) {
   await page.setViewportSize({width,height:700});await assertNoOverflow();
   assert.equal(await page.locator('.app-header').evaluate(e=>Math.round(e.getBoundingClientRect().top)),0);
   if(width>1100 && route!=='/repositories/1/pull-requests/7') {
    const form=route==='/repositories'?'#repository-filters':'#pull-request-filters';
    const input=await page.locator(`${form} input[type=search]`).boundingBox(), tabs=await page.locator(`${form} .repository-access-tabs`).boundingBox();
    assert(Math.abs(input.y-tabs.y)<2, 'desktop search and filters share a row');
   }
   if(route==='/repositories/1') assert(await page.locator('#pull-request-results .repository-table-scroll').evaluate(e=>e.clientHeight>60),'PR list remains scrollable');
  }
  if(process.env.UI_BROWSER_SCREENSHOTS==='1') await page.screenshot({path:path.join(__dirname,'artifacts',route.replaceAll('/','-')+'.png')});
 }
 assert(await page.getByRole('button',{name:'Build latest commit'}).isEnabled());
 const github=page.getByRole('link',{name:'View on GitHub',exact:true});
 assert(await github.locator('svg').count()>=1);assert.equal(await github.getAttribute('target'),'_blank');
 await page.locator('[data-trail-parent]').click();await page.waitForURL(url=>url.pathname==='/repositories/1');
});

test('failed PR searches preserve records and query bookmarks redirect to dedicated pages',async()=>{
 await login();await page.goto(`${origin}/repositories?repo_id=1&pr=7&pr_q=preview`);
 await page.waitForURL(url=>url.pathname==='/repositories/1/pull-requests/7');
 assert.match(await page.locator('.pr-detail-grid').innerText(),/No build requested yet/);
 await page.locator('[data-trail-parent]').click();await page.waitForURL(url=>url.pathname==='/repositories/1');
 // Intercept the owner's search after the initial live refetch settles.
 await page.waitForFunction(()=>document.documentElement.dataset.liveState==='live' && !document.getElementById('repository-detail').hasAttribute('aria-busy'));
 await page.route('**/repositories/1?*',route=>route.fulfill({status:503,body:'fixture unavailable'}));
 await page.locator('#pull-request-search').fill('preview 25');
 await page.locator('#pull-request-filters [data-repository-feedback][data-error]').waitFor();
 assert.equal(await page.locator('.pr-table tbody tr').count(),10);assert.equal(new URL(page.url()).searchParams.get('pr_q'),'preview');
 await page.unroute('**/repositories/1?*');
 await page.locator('#pull-request-search').press('Enter');await page.waitForFunction(()=>document.querySelectorAll('.pr-table tbody tr').length===1);
 assert.equal(new URL(page.url()).searchParams.get('pr_q'),'preview 25');
});

test('sync and automatic controls appear only on the PR list and breadcrumbs use consistent sizing',async()=>{
 await login();await page.goto(`${origin}/repositories/1?pr_q=preview&q=fixture&page=2`);
 await page.getByRole('switch',{name:'Automatic builds'}).click();await page.waitForFunction(()=>document.querySelector('#build-policy').matches(':modal'));
 await page.getByRole('button',{name:'Enable automatic',exact:true}).click();await readyPanel('1');
 const automatic=page.getByRole('switch',{name:'Automatic builds'});
 assert.equal(await automatic.getAttribute('aria-checked'),'true');
 assert.notEqual(await automatic.evaluate(e=>getComputedStyle(e).backgroundColor),'rgba(0, 0, 0, 0)');
 // Private previews confirm in their own modal; Escape changes nothing.
 const privacy=page.getByRole('switch',{name:'Private previews'});
 assert.equal(await privacy.getAttribute('aria-checked'),'false');
 await privacy.click();await page.waitForFunction(()=>document.querySelector('#preview-access').matches(':modal'));
 assert.equal(await page.locator('#preview-access-title').textContent(),'Make previews private?');
 await page.keyboard.press('Escape');await page.waitForFunction(()=>!document.querySelector('#preview-access').open);
 await page.getByRole('button',{name:'Sync Pull Requests'}).click();await page.locator('.sync-state').filter({hasText:'Syncing'}).waitFor();
 await page.waitForFunction(()=>!document.querySelector('.pr-summary').textContent.includes('Syncing'));await readyPanel('1');
 await page.locator('.pr-table tbody .pr-cell-text a').first().click();await page.waitForURL(url=>url.pathname.includes('/pull-requests/'));
 assert.equal(await page.getByRole('switch',{name:'Automatic builds'}).count(),0);
 assert.equal(await page.getByRole('button',{name:'Sync Pull Requests'}).count(),0);assert.equal(await page.locator('#build-policy').count(),0);
 const sizes=await page.locator('.app-breadcrumb').evaluate(e=>Array.from(e.children).map(child=>getComputedStyle(child).fontSize));
 assert(sizes.length>2 && sizes.every(size=>size===sizes[0]), 'breadcrumb text and separators share one size');
 await page.locator('[data-trail-parent]').click();await page.waitForURL(url=>url.pathname==='/repositories/1');
 assert.equal(await page.getByRole('switch',{name:'Automatic builds'}).getAttribute('aria-checked'),'true');
 assert.equal(new URL(page.url()).searchParams.get('page'),'2');
 await page.locator('[data-trail-parent]').click();await page.waitForURL(url=>url.pathname==='/repositories');
 assert.equal(await page.getByRole('switch',{name:'Automatic builds'}).count(),0);assert.equal(await page.getByRole('button',{name:'Sync Pull Requests'}).count(),0);
});

test('private previews explain missing auth and preserve the owner public toggle',async()=>{
 await login();await page.goto(`${origin}/repositories/101`);
 const privacy=page.getByRole('switch',{name:'Private previews'});
 assert.equal(await privacy.count(),0,'not offered until private previews can work');
 assert.match(await page.locator('#private-preview-setup').textContent(),/PREVIEW_ACCESS_URL/);
 assert.equal(await page.locator('#preview-access').count(),0);
 await page.goto(`${origin}/repositories/103`);
 assert.equal(await privacy.getAttribute('aria-checked'),'true');
 assert.equal(await privacy.isDisabled(),false);
 assert.match(await page.locator('#private-preview-setup').textContent(),/remain private/);
 await privacy.click();await page.waitForFunction(()=>document.querySelector('#preview-access').matches(':modal'));
 assert.equal(await page.locator('#preview-access-title').textContent(),'Make previews public?');
 assert.equal(await page.getByRole('button',{name:'Make public',exact:true}).isEnabled(),true);
 await page.keyboard.press('Escape');
});

test('small repository collections use compact cards with natural height and no pagination',async()=>{
 await login();
 for(const count of [1,2,3,5]) {
  await page.goto(`${origin}/repositories?access=all&fixture_count=${count}`);
  assert.equal(await page.locator('.repository-row').count(),count);
  assert.equal(await page.getByRole('navigation',{name:'Repository pages'}).count(),0);
  assert.match(await page.locator('.repository-count').innerText(),new RegExp(`${count} repositor`));
  assert.equal(await page.locator('.repository-select').first().locator('svg').count(),2,'the repository icon and the arrow');
  assert.deepEqual(await page.locator('.repository-select').first().locator('.repository-card-stats dt').allInnerTexts(),['Open pull requests','Live previews']);
  assert.match(await page.locator('.repository-select').first().locator('.repository-card-foot').innerText(),/(Manual|Automatic) builds[\s\S]*(Synced|Not synced yet)/);
  assert.equal(await page.locator('.repository-card-name p').first().innerText(),'fixture-owner');
  assert.equal(await page.locator('.repository-card-name h2').first().innerText().then(v=>v.includes('fixture-owner/')),false);
  for(const width of [320,390,768,1440,1917]) {
   await page.setViewportSize({width,height:960});await assertNoOverflow();
   const cards=await page.locator('.repository-select').evaluateAll(items=>items.map(e=>{const r=e.getBoundingClientRect();return {x:r.x,y:r.y,width:r.width,height:r.height};}));
   if(count>1 && width>700) assert(Math.abs(cards[0].y-cards[1].y)<4,'desktop cards share a row (hover lift allowed)');
   if(count>1 && width<=700) assert(cards[1].y>cards[0].y,'mobile cards stack');
   assert(cards.every(c=>c.height<280), 'long repository names fit compact cards');
   if(count<=2 && width>700) {
    const catalog=await page.locator('.repository-catalog').boundingBox();
    // Cards include two stats and a footer (mode, sync); a 960px screen leaves room to spare.
    assert(catalog.height<560, 'small collections do not stretch toward the footer: '+catalog.height+' at '+width);
    assert(catalog.width<=1241,'catalog stays within the content column');
   }
  }
  if(process.env.UI_BROWSER_SCREENSHOTS==='1') await page.screenshot({path:path.join(__dirname,'artifacts',`repository-cards-${count}.png`)});
 }
 await page.locator('[data-repo-link="1"]').click();await page.waitForURL(url=>url.pathname==='/repositories/1');
});

test('command palette filters destinations, navigates and restores focus',async()=>{
 await login();await page.goto(`${origin}/repositories`);
 const trigger=page.locator('.command-trigger');
 await trigger.click();await page.waitForFunction(()=>document.querySelector('#command-palette').matches(':modal'));
 assert(await page.locator('#command-input').evaluate(e=>e===document.activeElement));
 await page.keyboard.press('Escape');
 await page.waitForFunction(()=>document.activeElement===document.querySelector('.command-trigger'));
 await page.keyboard.press('Control+k');await page.waitForFunction(()=>document.querySelector('#command-palette').open);
 await page.locator('#command-input').fill('environ');
 assert.equal(await page.locator('[data-command-item]:not([hidden])').count(),1);
 assert.match(await page.locator('[data-command-item][aria-selected=true]').innerText(),/Previews/);
 await page.keyboard.press('Enter');await page.waitForURL(`${origin}/previews`);
 assert.equal(await page.locator('h1').innerText(),'Previews');
 await page.keyboard.press('Control+k');await page.waitForFunction(()=>document.querySelector('#command-palette').open);
 await page.locator('#command-input').fill('zzz-no-match');
 assert.equal(await page.locator('[data-command-empty]').isVisible(),true);
 await page.locator('#command-input').fill('log out');await page.keyboard.press('Enter');
 await page.waitForFunction(()=>document.querySelector('#logout-confirmation').matches(':modal'));
 assert.equal(await page.locator('#command-palette').evaluate(e=>e.open),false);
 await page.getByRole('button',{name:'Cancel',exact:true}).click();
 assert.equal((await context.request.get(`${origin}/dashboard`,{maxRedirects:0})).status(),200);
});

test('theme toggle switches and persists the colour theme',async()=>{
 await page.emulateMedia({colorScheme:'light'});
 await login();
 const toggle=page.locator('.app-header [data-theme-toggle]');
 assert.equal(await toggle.getAttribute('aria-label'),'Switch to dark theme');
 const before=await page.evaluate(()=>getComputedStyle(document.body).backgroundColor);
 await toggle.click();
 assert.equal(await page.evaluate(()=>document.documentElement.dataset.theme),'dark');
 await page.waitForFunction(b=>getComputedStyle(document.body).backgroundColor!==b,before);
 await page.reload();
 assert.equal(await page.evaluate(()=>document.documentElement.dataset.theme),'dark');
 assert.equal(await toggle.getAttribute('aria-label'),'Switch to light theme');
 for(const width of [320,1440]) {await page.setViewportSize({width,height:844});await assertNoOverflow();}
});

test('live updates push changes across tabs without polling',async()=>{
 await login();await page.goto(`${origin}/repositories/1`);await readyPanel('1');
 await page.waitForFunction(()=>document.documentElement.dataset.liveState==='live');
 assert(await page.locator('.live-indicator').isVisible());
 let refreshes=0;
 page.on('request',request=>{if(request.method()==='GET'&&new URL(request.url()).pathname==='/repositories/1') refreshes++;});
 await page.waitForTimeout(4000);
 assert.equal(refreshes,0,'an idle repository page must not poll');
 const other=await context.newPage();
 try {
  await other.goto(`${origin}/repositories/1`);
  await other.getByRole('button',{name:'Sync Pull Requests',exact:true}).click();
  await page.locator('.sync-state').filter({hasText:'Syncing'}).waitFor();
  await page.waitForFunction(()=>!document.querySelector('.pr-summary').textContent.includes('Syncing'));
  assert(refreshes>0,'pushed changes refetch the panel');
  assert(await page.getByRole('button',{name:'Sync Pull Requests',exact:true}).isEnabled());
 } finally {await other.close();}
});

test('repository secrets editor saves several at once, never shows values, and deletes selected ones',async()=>{
 await login();await page.goto(`${origin}/repositories/1`);await readyPanel('1');
 await page.locator('.repo-tabs').getByRole('link',{name:'Secrets',exact:true}).click();
 await page.waitForURL(`${origin}/repositories/1/secrets`);
 assert.match(await page.locator('.app-breadcrumb').innerText(),/repository-1[\s\S]*Secrets/);
 assert.equal(await page.locator('.repo-tabs a[aria-current=page]').innerText(),'Secrets');
 const name=n=>page.getByRole('textbox',{name:`Name, line ${n}`,exact:true});
 const value=n=>page.getByRole('textbox',{name:`Value, line ${n}`,exact:true});
 // One line per secret; a duplicate name is refused before anything is sent.
 await name(1).fill('API_KEY');await value(1).fill('sk_test_never_shown_123');
 await page.getByRole('button',{name:'Add another line',exact:true}).click();
 await name(2).fill('API_KEY');await value(2).fill('duplicate');
 await page.locator('[data-secret-submit]').click();
 assert.match(await page.locator('[data-secret-row]').nth(1).innerText(),/appears more than once/);
 assert.equal(new URL(page.url()).search,'','a refused save stays on the page');
 await name(2).fill('WEBHOOK_SECRET');
 assert.equal(await page.locator('[data-secret-submit-label]').innerText(),'Save 2 secrets');
 await page.locator('[data-secret-submit]').click();
 await page.waitForURL(url=>url.searchParams.get('saved_count')==='2');
 assert.match(await page.locator('.connection-notice').innerText(),/Saved 2 secrets \(2 new, 0 replaced\)/);
 assert.match(await page.locator('.secrets-table').innerText(),/API_KEY[\s\S]*WEBHOOK_SECRET/);
 assert(!(await page.content()).includes('sk_test_never_shown_123'),'the value must never be rendered');
 // Build credentials require a modal opt-in and can be revoked in place.
 await page.getByRole('button',{name:'Let builds read API_KEY',exact:true}).click();
 const allow=page.locator('#allow-build-secret-0');
 assert(await allow.evaluate(e=>e.matches(':modal')));
 assert.equal(await page.evaluate(()=>document.activeElement.textContent.trim()),'Cancel');
 await page.keyboard.press('Escape');
 assert.equal(await allow.evaluate(e=>e.open),false);
 await page.getByRole('button',{name:'Let builds read API_KEY',exact:true}).click();
 await allow.getByRole('button',{name:'Allow builds',exact:true}).click();
 await page.waitForURL(url=>url.searchParams.get('build_on')==='API_KEY');
 assert.match(await page.locator('.secrets-table tbody tr').first().innerText(),/Allowed/);
 await page.getByRole('button',{name:'Stop giving API_KEY to builds',exact:true}).click();
 await page.waitForURL(url=>url.searchParams.get('build_off')==='API_KEY');
 // Pasting NAME=value lines into a name splits them into lines.
 await name(1).evaluate(input=>{const data=new DataTransfer();data.setData('text/plain','API_KEY=replaced\nFEATURE_FLAG=on');input.dispatchEvent(new ClipboardEvent('paste',{clipboardData:data,bubbles:true,cancelable:true}));});
 assert.equal(await name(2).inputValue(),'FEATURE_FLAG');
 assert.match(await page.locator('[data-secret-row]').first().innerText(),/Replaces the stored value/);
 await page.locator('[data-secret-submit]').click();
 await page.waitForURL(url=>url.searchParams.get('saved_count')==='2');
 assert.match(await page.locator('.connection-notice').innerText(),/1 new, 1 replaced/);
 // A row's Replace prefills its name and never its value.
 await page.getByRole('link',{name:'Replace API_KEY',exact:true}).click();
 await page.waitForURL(url=>url.searchParams.get('replace')==='API_KEY');
 assert.equal(await name(1).inputValue(),'API_KEY');assert.equal(await value(1).inputValue(),'');
 // A single delete confirms in its own modal; Escape changes nothing.
 const remove=page.getByRole('button',{name:'Delete API_KEY',exact:true});
 await remove.click();await page.waitForFunction(()=>document.querySelector('#delete-secret-0').matches(':modal'));
 assert.equal(await page.evaluate(()=>document.activeElement.textContent.trim()),'Cancel');
 await page.keyboard.press('Escape');
 assert.equal(await page.locator('#delete-secret-0').evaluate(e=>e.open),false);
 assert.equal(await page.locator('.secrets-table tbody tr').count(),3);
 // Selected secrets are deleted together after one confirmation.
 const bulk=page.locator('[data-bulk-delete]');
 assert(await bulk.isDisabled(),'nothing is selected yet');
 await page.getByRole('checkbox',{name:'Select all secrets'}).check();
 assert.match(await bulk.innerText(),/\(3\)/);
 await bulk.click();await page.waitForFunction(()=>document.querySelector('#delete-selected-secrets').matches(':modal'));
 assert.match(await page.locator('[data-bulk-delete-summary]').innerText(),/3 secrets: API_KEY, FEATURE_FLAG, WEBHOOK_SECRET/);
 await page.locator('#delete-selected-secrets').getByRole('button',{name:'Delete selected',exact:true}).click();
 await page.waitForURL(url=>url.searchParams.get('deleted_count')==='3');
 await page.getByText('No secrets yet').waitFor();
 for(const width of [320,390,768,1440]) {await page.setViewportSize({width,height:844});await assertNoOverflow();}
 await page.locator('.repo-tabs').getByRole('link',{name:'Pull requests',exact:true}).click();
 await page.waitForURL(`${origin}/repositories/1`);
});

test('the secrets editor works without JavaScript and pasted .env text is saved',async()=>{
 const noJS=await browser.newContext({javaScriptEnabled:false});
 try {
  const plain=await noJS.newPage();
  await plain.goto(`${origin}/auth/github/login`);await plain.waitForURL(`${origin}/dashboard`);
  await plain.goto(`${origin}/repositories/3/secrets`);
  assert.equal(await plain.locator('[data-secret-row]').count(),4,'one line plus three spare lines');
  await plain.locator('summary').filter({hasText:'Paste or import a .env file'}).click();
  await plain.locator('#secret-dotenv').fill('# comment\nexport DB_URL=postgres://preview\nPRIVATE_KEY="-----BEGIN\nabc\n-----END"');
  await plain.locator('[data-secret-submit]').click();
  await plain.waitForURL(url=>url.searchParams.get('saved_count')==='2');
  assert.match(await plain.locator('.secrets-table').innerText(),/DB_URL[\s\S]*PRIVATE_KEY/);
  await plain.getByRole('button',{name:'Let builds read DB_URL',exact:true}).click();
  const allow=plain.locator('#allow-build-secret-0');
  assert(await allow.evaluate(e=>e.matches(':modal')),'build opt-in is a native modal without JavaScript');
  await allow.getByRole('button',{name:'Allow builds',exact:true}).click();
  await plain.waitForURL(url=>url.searchParams.get('build_on')==='DB_URL');
  await plain.getByRole('button',{name:'Stop giving DB_URL to builds',exact:true}).click();
  await plain.waitForURL(url=>url.searchParams.get('build_off')==='DB_URL');
  await plain.locator('summary').filter({hasText:'Paste or import a .env file'}).click();
  await plain.locator('#secret-dotenv').fill('NOT A LINE');
  await plain.locator('[data-secret-submit]').click();
  assert.match(await plain.locator('#secret-dotenv-error').innerText(),/Line 1: expected NAME=value/);
  assert.equal(await plain.locator('#secret-dotenv').inputValue(),'','pasted text is never rendered back');
 } finally {await noJS.close();}
});

test('build logs stream live, reset for a new attempt and render output as text',async()=>{
 await context.request.post(`${origin}/__fixture/logs/seed`);
 await login();await page.goto(`${origin}/repositories/1/pull-requests/3`);
 await page.getByRole('link',{name:'Build logs',exact:true}).click();
 await page.locator('.log-panel-inline .log-line').first().waitFor();
 assert.equal(new URL(page.url()).pathname, '/repositories/1/pull-requests/3');
 assert.equal(await page.locator('.log-panel-inline .log-line').count(),300);
 await page.getByRole('link',{name:'View full build logs',exact:true}).click();
 await page.waitForURL(`${origin}/repositories/1/pull-requests/3/logs`);
 assert.match(await page.locator('.app-breadcrumb').innerText(),/#3[\s\S]*Build logs/);
 assert.match(await page.locator('.page-hero-kicker').innerText(),/repository-1[\s\S]*#3/);
 await page.waitForFunction(()=>document.querySelector('[data-log-connection]')?.dataset.state==='live');
 assert.equal(await page.locator('.log-line').count(),500,'the newest 500 lines render first');
 assert.equal(await page.evaluate(()=>window.__pwned),undefined,'script-looking output must stay inert');
 assert.equal(await page.locator('.log-lines script, .log-lines img').count(),0);
 assert.equal(await page.locator('#L519 .log-text').innerText(),'2026-10-13T12:00:00.000Z <img src=x onerror="window.__pwned = true">');
 assert(await page.locator('.log-lines').evaluate(e=>e.scrollHeight-e.scrollTop-e.clientHeight<24),'the newest output is in view');
 let documents=0;page.on('request',request=>{if(request.resourceType()==='document') documents++;});
 await context.request.post(`${origin}/__fixture/logs/append`,{form:{text:'Step 3/3 : RUN <b>npm test</b>'}});
 await page.locator('#L521').waitFor();
 assert.equal(await page.locator('#L521 .log-text').innerText(),'2026-10-13T12:00:00.000Z Step 3/3 : RUN <b>npm test</b>');
 assert(await page.locator('.log-lines').evaluate(e=>e.scrollHeight-e.scrollTop-e.clientHeight<24),'following keeps new output in view');
 // Scrolling up pauses following; new output must not pull the reader down.
 await page.locator('.log-lines').evaluate(e=>{e.scrollTop=0;e.dispatchEvent(new Event('scroll'));});
 assert.equal(await page.locator('[data-log-follow]').isChecked(),false);
 await context.request.post(`${origin}/__fixture/logs/append`,{form:{text:'more output'}});
 await page.locator('#L522').waitFor();
 assert.equal(await page.locator('.log-lines').evaluate(e=>e.scrollTop),0);
 // A new attempt replaces the logs instead of being appended to the old ones.
 await context.request.post(`${origin}/__fixture/logs/restart`,{form:{text:'Retry attempt output'}});
 await page.locator('.log-line').filter({hasText:'Retry attempt output'}).waitFor();
 assert.equal(await page.locator('.log-line').count(),1);
 assert.equal(await page.locator('#L1 .log-text').innerText(),'2026-10-13T12:00:00.000Z Retry attempt output');
 // The step tracker follows the runtime's own progress lines as they arrive.
 assert.equal(await page.locator('.build-tracker .is-current').getAttribute('data-stage'),'image');
 await context.request.post(`${origin}/__fixture/logs/append`,{form:{text:'The preview is healthy. It is not routed yet.',system:'1'}});
 await page.waitForFunction(()=>document.querySelector('.build-tracker .is-current')?.dataset.stage==='publish');
 assert(await page.locator('.log-line.log-system').filter({hasText:'The preview is healthy'}).isVisible());
 assert.equal(documents,0,'live logs never reload the page');
 for(const width of [320,390,768,1440]) {await page.setViewportSize({width,height:844});await assertNoOverflow();}
});

test('PR logs stay inline, filter both sources, copy dates and release closed streams',async()=>{
 await context.request.post(`${origin}/__fixture/logs/seed`);
 await context.request.post(`${origin}/__fixture/logs/append`,{form:{text:'Previa downloaded source',system:'1'}});
 await context.request.post(`${origin}/__fixture/logs/append`,{form:{text:'Previa waiting for health',system:'1'}});
 await context.request.post(`${origin}/__fixture/logs/append`,{form:{text:'2026/10/12 15:22:34 [notice] native application date'}});
 await context.addInitScript(()=>{
  const Original=window.EventSource;window.__logStreams=0;
  window.EventSource=class extends Original {
   constructor(url,...args){super(url,...args);this.counted=String(url).includes('/logs/events');if(this.counted) window.__logStreams++;}
   close(){if(this.counted){window.__logStreams--;this.counted=false;}super.close();}
  };
 });
 await context.grantPermissions(['clipboard-read','clipboard-write'],{origin});
 await login();await page.goto(`${origin}/repositories/1/pull-requests/3`);
 await page.getByRole('link',{name:'Build logs',exact:true}).click();
 const panel=page.locator('.log-panel-inline');
 await panel.locator('.log-line').first().waitFor();
 assert.equal(new URL(page.url()).pathname,'/repositories/1/pull-requests/3');
 assert.equal(await panel.locator('.log-line').count(),300);
 assert.equal(await panel.locator('.log-time, time[data-log-time]').count(),0);
 assert.equal(await panel.locator('.log-timestamp').count(),300);
 assert.equal(await panel.getByLabel('Log source',{exact:true}).inputValue(),'all');
 assert.equal(await panel.locator('.log-line').last().locator('.log-text').innerText(),'2026/10/12 15:22:34 [notice] native application date');
 const previaColour=await panel.locator('.log-system .log-text').first().evaluate(e=>getComputedStyle(e).color);
 const appColour=await panel.locator('[data-log-source="application"] .log-text').first().evaluate(e=>getComputedStyle(e).color);
 assert.notEqual(previaColour,appColour);
 await panel.getByLabel('Log source',{exact:true}).selectOption('previa');
 await panel.getByRole('button',{name:'Apply filters'}).click();
 await page.waitForFunction(()=>document.querySelector('.log-panel-inline')?.dataset.logSource==='previa');
 assert.equal(await panel.locator('.log-line').count(),2);
 await panel.getByRole('button',{name:'Copy the lines shown'}).click();
 await panel.locator('[data-copy-status]').filter({hasText:'Copied'}).waitFor();
 assert.equal(await page.evaluate(()=>navigator.clipboard.readText()),'2026-10-13T12:00:00.000Z Previa downloaded source\n2026-10-13T12:00:00.000Z Previa waiting for health');
 await context.request.post(`${origin}/__fixture/logs/append`,{form:{text:'excluded application output'}});
 await context.request.post(`${origin}/__fixture/logs/append`,{form:{text:'Previa matched live notice',system:'1'}});
 await panel.locator('.log-line').filter({hasText:'Previa matched live notice'}).waitFor();
 assert.equal(await panel.locator('.log-line').count(),3);
 // A repository refresh replaces the detail DOM, preserving this open viewer and filter.
 await page.evaluate(()=>htmx.ajax('GET',location.pathname,{target:'#repository-detail',swap:'innerHTML'}));
 await page.waitForFunction(()=>document.querySelector('.log-panel-inline')?.dataset.logSource==='previa' && document.querySelectorAll('.log-panel-inline .log-line').length===3);
 assert(await page.locator('[data-pr-logs]').evaluate(e=>e.open));
 await page.locator('[data-pr-logs] > summary').click();
 await page.waitForFunction(()=>window.__logStreams===0);
 await context.request.post(`${origin}/__fixture/logs/append`,{form:{text:'Previa while closed',system:'1'}});
 await page.getByRole('link',{name:'Build logs',exact:true}).click();
 await panel.locator('.log-line').filter({hasText:'Previa while closed'}).waitFor();
 assert.equal(await panel.locator('.log-line').count(),4);
 await panel.getByLabel('Log source',{exact:true}).selectOption('application');
 await panel.getByLabel('Find in logs').fill('native application');
 await panel.getByRole('button',{name:'Apply filters'}).click();
 await page.waitForFunction(()=>document.querySelector('.log-panel-inline')?.dataset.logSource==='application' && document.querySelectorAll('.log-panel-inline .log-line').length===1);
 const full=panel.getByRole('link',{name:'View full build logs',exact:true});
 assert.match(await full.getAttribute('href'),/source=application/);
 await full.click();await page.waitForURL(url=>url.pathname.endsWith('/logs') && url.searchParams.get('q')==='native application');
 assert.equal(await page.locator('.log-line').count(),1);
 for(const width of [320,390,768,1440]) {await page.setViewportSize({width,height:844});await assertNoOverflow();}
});

test('recent log live tail keeps only 300 matching events and reports failed loads',async()=>{
 await context.request.post(`${origin}/__fixture/logs/seed`);
 await login();await page.goto(`${origin}/repositories/1/pull-requests/3`);
 await page.getByRole('link',{name:'Build logs',exact:true}).click();
 const panel=page.locator('.log-panel-inline');await panel.locator('.log-line').first().waitFor();
 for(let i=0;i<12;i++) await context.request.post(`${origin}/__fixture/logs/append`,{form:{text:'bounded tail '+i}});
 await panel.locator('.log-line').filter({hasText:'bounded tail 11'}).waitFor();
 assert.equal(await panel.locator('.log-line').count(),300);
 assert.equal(await panel.locator('.log-line').first().getAttribute('id'),'L233');
 assert.equal(await panel.locator('[data-log-count]').innerText(),'300 events shown');
 for(const width of [320,390,768,1440]) {await page.setViewportSize({width,height:844});await assertNoOverflow();}
 if(process.env.UI_BROWSER_SCREENSHOTS==='1') await page.screenshot({path:path.join(__dirname,'artifacts','pr-recent-build-logs.png'),fullPage:true});
 await page.route('**/logs/recent*',route=>route.fulfill({status:503,body:'Unavailable'}));
 await panel.getByRole('link',{name:'Refresh',exact:true}).click();
 await page.locator('#pr-recent-log-content').filter({hasText:'Recent logs are temporarily unavailable'}).waitFor();
});

test('App output retains live status, inline dates, copying and attempt resets',async()=>{
 await context.request.post(`${origin}/__fixture/logs/restart`,{form:{text:'Application initial output'}});
 await context.grantPermissions(['clipboard-read','clipboard-write'],{origin});
 await login();await page.goto(`${origin}/repositories/1/pull-requests/3/output`);
 await page.waitForFunction(()=>document.querySelector('[data-log-connection]')?.dataset.state==='live');
 assert.equal(await page.locator('.log-time').count(),0);
 assert.equal(await page.locator('#L1 .log-text').innerText(),'2026-10-13T12:00:00.000Z Application initial output');
 await context.request.post(`${origin}/__fixture/logs/append`,{form:{text:'2026/10/12 15:22:34 [notice] application native date'}});
 await page.locator('#L2').waitFor();
 assert.equal(await page.locator('#L2 .log-text').innerText(),'2026/10/12 15:22:34 [notice] application native date');
 await page.getByRole('button',{name:'Copy the lines shown'}).click();
 await page.locator('[data-copy-status]').filter({hasText:'Copied'}).waitFor();
 assert.equal(await page.evaluate(()=>navigator.clipboard.readText()),'2026-10-13T12:00:00.000Z Application initial output\n2026/10/12 15:22:34 [notice] application native date');
 await context.request.post(`${origin}/__fixture/logs/restart`,{form:{text:'New app output'}});
 await page.locator('.log-line').filter({hasText:'New app output'}).waitFor();
 assert.equal(await page.locator('.log-line').count(),1);
 for(const width of [320,390,768,1440]) {await page.setViewportSize({width,height:844});await assertNoOverflow();}
});

test('build logs paginate and stay readable without JavaScript',async()=>{
 await context.request.post(`${origin}/__fixture/logs/seed`);
 const noJS=await browser.newContext({javaScriptEnabled:false});
 try {
  const plain=await noJS.newPage();
  await plain.goto(`${origin}/auth/github/login`);await plain.waitForURL(`${origin}/dashboard`);
  await plain.goto(`${origin}/repositories/1/pull-requests/3`);
  await plain.getByRole('link',{name:'Build logs',exact:true}).click();
  await plain.waitForURL(`${origin}/repositories/1/pull-requests/3/logs`);
  assert.equal(await plain.locator('.log-line').count(),500);
  assert.equal(await plain.locator('.log-line').first().getAttribute('id'),'L21');
  assert.equal(await plain.locator('[data-log-connection]').innerText(),'Live updates off');
  assert(await plain.locator('[data-log-follow-control]').isHidden());
  await plain.getByRole('link',{name:'Earlier lines',exact:true}).click();
  await plain.waitForURL(url=>url.searchParams.get('before')==='20');
  assert.equal(await plain.locator('.log-line').count(),20);
  await plain.getByRole('link',{name:'Later lines',exact:true}).click();
  await plain.waitForURL(url=>url.searchParams.get('after')==='20');
  assert.equal(await plain.locator('.log-line').first().getAttribute('id'),'L21');
  await plain.getByRole('link',{name:'Refresh',exact:true}).waitFor();
  await plain.getByLabel('Log source',{exact:true}).selectOption('application');
  await plain.getByLabel('Find in logs').fill('step output line 50');
  await plain.getByRole('button',{name:'Apply filters'}).click();
  await plain.waitForURL(url=>url.searchParams.get('source')==='application');
  assert.equal(await plain.locator('.log-line').count(),11);
  assert.match(await plain.getByRole('link',{name:'Download',exact:true}).getAttribute('href'),/source=application/);
 } finally {await noJS.close();}
});

// Resilience: what a person sees when the network or the server misbehaves.
// Requests are slowed, refused, dropped or left unanswered in the browser, so
// these scenarios exercise the real page code against each failure.

const wait = ms => new Promise(resolve => setTimeout(resolve, ms));
const isSearch = (query) => url => url.pathname === '/repositories/1' && url.searchParams.get('pr_q') === query;

test('slow searches show progress, apply only the newest results and stay responsive', async () => {
  await login();
  await page.goto(`${origin}/repositories/1`);
  await liveIdle();
  let searches = 0;
  await page.route(url => url.pathname === '/repositories/1' && url.searchParams.has('pr_q'), async route => {
    searches++;
    await wait(900);
    await route.continue().catch(() => {}); // the page may have cancelled it meanwhile
  });
  const search = page.locator('#pull-request-search');
  await search.fill('preview 1');
  await page.waitForFunction(() => document.getElementById('pull-request-results').getAttribute('aria-busy') === 'true');
  assert.match(await page.locator('#pull-request-filters [data-repository-feedback]').innerText(), /Updating/);
  // The field stays editable while that search is in flight; the newer one replaces it.
  await search.fill('preview 12');
  await page.waitForFunction(() => document.querySelectorAll('.pr-table tbody tr').length === 1 &&
    !document.getElementById('pull-request-results').hasAttribute('aria-busy'));
  assert.match(await page.locator('.pr-table').innerText(), /#12/);
  assert.equal(new URL(page.url()).searchParams.get('pr_q'), 'preview 12');
  assert(searches >= 2, 'both searches were sent');
  await wait(1200); // a late answer to the older search must never replace the newer results
  assert.equal(await page.locator('.pr-table tbody tr').count(), 1);
  assert.equal(await page.locator('#pull-request-filters [data-repository-feedback]').innerText(), '');
});

test('a slow build request disables its control so a double click sends one request', async () => {
  await login();
  await page.goto(`${origin}/repositories/1/pull-requests/7`);
  let builds = 0;
  await page.route('**/repositories/1/pull-requests/7/build', async route => {
    builds++;
    await wait(800);
    await route.continue().catch(() => {});
  });
  const build = page.getByRole('button', {name: 'Build latest commit', exact: true});
  await build.dblclick();
  assert(await page.locator('form[action$="/pull-requests/7/build"] button[type="submit"]').isDisabled(), 'the button is disabled while the request runs');
  await page.waitForFunction(() => /Queued/.test(document.querySelector('.pr-detail-grid')?.innerText || ''));
  assert.equal(builds, 1, 'one build request reached the server');
});

test('refused, failed and offline requests explain themselves and leave the page usable', async () => {
  await login();
  await page.goto(`${origin}/repositories/1/pull-requests/8`);
  const route = '**/repositories/1/pull-requests/8/build';
  const build = page.getByRole('button', {name: 'Build latest commit', exact: true});
  const feedback = page.locator('[data-detail-feedback]');
  const failedWith = async pattern => {
    await page.waitForFunction(text => new RegExp(text).test(document.querySelector('[data-detail-feedback]')?.textContent || ''), pattern.source);
    assert.equal(await feedback.getAttribute('data-error'), 'true');
    assert.equal(await page.locator('#repository-detail').getAttribute('aria-busy'), null, 'the panel is not left busy');
    assert(await build.isEnabled(), 'the control can be used again');
    assert.match(await page.locator('.pr-detail-grid').innerText(), /No build requested yet/, 'nothing changed');
  };
  // A refusal the server explains in a sentence is shown as it is.
  await page.route(route, r => r.fulfill({status: 409, contentType: 'text/plain; charset=utf-8', body: 'The pull request or its build changed meanwhile. Reload and try again.'}));
  await build.click();
  await failedWith(/changed meanwhile/);
  // An unexplained server error gets a plain message, never the error page.
  await page.unroute(route);
  await page.route(route, r => r.fulfill({status: 500, contentType: 'text/html', body: '<h1>internal stack trace</h1>'}));
  await build.click();
  await failedWith(/Could not update this view/);
  assert.doesNotMatch(await page.locator('body').innerText(), /stack trace/);
  // Offline: the request never reaches the server.
  await page.unroute(route);
  await page.route(route, r => r.abort('internetdisconnected'));
  await build.click();
  await failedWith(/Check your connection/);
  // Back online, the same control works and the message clears.
  await page.unroute(route);
  await build.click();
  await page.waitForFunction(() => /Queued/.test(document.querySelector('.pr-detail-grid')?.innerText || ''));
  assert.equal(await feedback.innerText(), '');
});

test('a request that never answers times out with a message and can be retried', async () => {
  await login();
  await page.goto(`${origin}/repositories/1`);
  await liveIdle();
  await page.route(isSearch('preview 9'), () => {}); // never answered
  await page.locator('#pull-request-search').fill('preview 9');
  await page.waitForFunction(() => /Could not update this view/.test(document.querySelector('#pull-request-filters [data-repository-feedback]')?.textContent || ''), null, {timeout: 15000});
  assert.equal(await page.locator('.pr-table tbody tr').count(), 10, 'the earlier results stay');
  assert.equal(await page.locator('#pull-request-results').getAttribute('aria-busy'), null);
  await page.unroute(isSearch('preview 9'));
  await page.locator('#pull-request-search').fill('preview 19');
  await page.waitForFunction(() => document.querySelectorAll('.pr-table tbody tr').length === 1);
  assert.match(await page.locator('.pr-table').innerText(), /#19/);
});

test('a forged CSRF token is refused and changes nothing', async () => {
  await login();
  await page.goto(`${origin}/repositories/1/pull-requests/11`);
  const response = page.waitForResponse(r => r.url().endsWith('/pull-requests/11/build'));
  // Submit the forged form atomically so a live refresh cannot replace it
  // between tampering with the token and dispatching the request.
  await page.locator('form[action$="/pull-requests/11/build"]').evaluate(form => {
    form.querySelector('input[name="csrf_token"]').value = 'forged';
    form.requestSubmit(form.querySelector('button[type="submit"]'));
  });
  assert.equal((await response).status(), 403);
  await page.waitForFunction(() => document.querySelector('[data-detail-feedback]')?.dataset.error === 'true');
  assert.match(await page.locator('[data-detail-feedback]').innerText(), /could not be verified/);
  await page.reload();
  assert.match(await page.locator('.pr-detail-grid').innerText(), /No build requested yet/);
});

test('the workspace loads within its page weight and stays usable on a slow network', async () => {
  await login();
  const cdp = await context.newCDPSession(page);
  await cdp.send('Network.enable');
  await cdp.send('Network.setCacheDisabled', {cacheDisabled: true});
  // About "slow 3G": 400 ms round trips and 400 kbit/s down.
  await cdp.send('Network.emulateNetworkConditions', {offline: false, latency: 400, downloadThroughput: 50 * 1024, uploadThroughput: 25 * 1024});
  const started = Date.now();
  await page.goto(`${origin}/repositories/1`, {waitUntil: 'load', timeout: 30000});
  const loadMs = Date.now() - started;
  const bytes = await page.evaluate(() => [...performance.getEntriesByType('navigation'), ...performance.getEntriesByType('resource')]
    .reduce((sum, entry) => sum + (entry.transferSize || 0), 0));
  assert(bytes > 0 && bytes < 512 * 1024, `page weight ${bytes} bytes`);
  assert(loadMs < 15000, `slow-network load took ${loadMs} ms`);
  await liveIdle(page, {timeout: 20000});
  await page.locator('#pull-request-search').fill('preview 25');
  await page.waitForFunction(() => document.querySelectorAll('.pr-table tbody tr').length === 1, null, {timeout: 15000});
  await page.locator('.pr-table tbody .pr-cell-text a').first().click();
  await page.waitForURL(url => url.pathname === '/repositories/1/pull-requests/25', {timeout: 15000});
  assert(await page.getByRole('button', {name: 'Build latest commit', exact: true}).isEnabled());
  console.log(`# slow network: ${Math.round(bytes / 1024)} KiB in ${loadMs} ms`);
});

test('every page passes basic accessibility checks', async () => {
  const audit = () => {
    const problems = [];
    const name = el => (el.getAttribute('aria-label') || el.getAttribute('aria-labelledby') && document.getElementById(el.getAttribute('aria-labelledby'))?.textContent ||
      el.getAttribute('title') || el.textContent || el.querySelector('img[alt]')?.getAttribute('alt') || el.getAttribute('value') || '').trim();
    const visible = el => !el.closest('[hidden], template') && el.getClientRects().length > 0;
    if (!document.documentElement.lang) problems.push('html has no lang');
    if (document.querySelectorAll('h1').length !== 1) problems.push(`${document.querySelectorAll('h1').length} h1 elements`);
    if (!document.querySelector('main')) problems.push('no main landmark');
    if (!document.title.trim()) problems.push('no title');
    for (const el of document.querySelectorAll('a[href], button, [role="button"], [role="switch"], [role="link"], summary')) {
      if (visible(el) && !name(el)) problems.push(`unnamed ${el.tagName.toLowerCase()} ${el.outerHTML.slice(0, 80)}`);
    }
    for (const el of document.querySelectorAll('input:not([type="hidden"]), select, textarea')) {
      const labelled = el.labels?.length || el.getAttribute('aria-label') || el.getAttribute('aria-labelledby') || el.getAttribute('title');
      if (visible(el) && !labelled) problems.push(`unlabelled ${el.tagName.toLowerCase()} ${el.name || el.id}`);
    }
    for (const img of document.querySelectorAll('img')) if (!img.hasAttribute('alt')) problems.push(`img without alt ${img.src}`);
    const ids = [...document.querySelectorAll('[id]')].map(el => el.id);
    for (const id of new Set(ids.filter((id, i) => ids.indexOf(id) !== i))) problems.push(`duplicate id ${id}`);
    return problems;
  };
  await page.goto(origin);
  assert.deepEqual(await page.evaluate(audit), [], 'home');
  await login();
  for (const path of ['/dashboard', '/repositories', '/repositories/1', '/repositories/1/pull-requests/3', '/repositories/1/pull-requests/9',
    '/repositories/1/secrets', '/repositories/1/outbound', '/repositories/1/pull-requests/3/logs', '/builds?fixture=rows', '/previews?fixture=rows', '/faq', '/developer-tools']) {
    await page.goto(`${origin}${path}`);
    assert.deepEqual(await page.evaluate(audit), [], path);
  }
});

// What a Download button posts: the file's text from the editor. The server
// echoes it back as an attachment (unit tested); browsers send CRLF breaks.
function postedContent(request) {
  return new URLSearchParams(request.postData()).get('content').replace(/\r\n/g, '\n');
}
// The kit is idle: no request in flight and the last swap has settled.
const kitIdle = target => target.waitForFunction(() => !document.querySelector('.htmx-request, .htmx-settling, .htmx-added'));

test('starter kits write both files for the chosen stack as editable tabs, and edits survive until reset',async()=>{
 await login();
 await page.locator('.app-sidebar a[href="/starter-kits"]').click();await page.waitForURL(`${origin}/starter-kits`);
 assert.equal(await page.locator('.app-sidebar a[href="/starter-kits"]').getAttribute('aria-current'),'page');
 assert.equal(await page.locator('h1').innerText(),'Starter kits');
 const file=id=>page.locator(`#kit-code-${id}`);
 const tab=name=>page.getByRole('tab',{name,exact:false});
 const status=()=>page.locator('.kit-check').innerText();
 // Two tabs; the Dockerfile shows first, highlighted, and the other panel is hidden.
 assert.equal(await page.getByRole('tab').count(),2);
 assert.equal(await tab('Dockerfile').getAttribute('aria-selected'),'true');
 assert(await page.locator('#kit-panel-previewrc').isHidden());
 assert.match(await file('dockerfile').inputValue(),/^FROM node:/m);
 assert(await page.locator('#kit-panel-dockerfile .kit-highlight .tk-kw').count()>3,'the Dockerfile is highlighted');
 assert.match(await status(),/Passes Previa's checks/);
 await tab('.previewrc.yaml').click();
 assert(await page.locator('#kit-panel-dockerfile').isHidden());
 assert.match(await file('previewrc').inputValue(),/^port: 3000/m);
 await tab('.previewrc.yaml').press('ArrowLeft');
 assert.equal(await tab('Dockerfile').getAttribute('aria-selected'),'true');
 await tab('Dockerfile').press('ArrowRight');
 // Choosing a stack swaps in its defaults, keeps the open tab and the address shareable.
 await page.locator('.kit-stack',{hasText:'Django'}).click();
 await page.waitForURL(url=>url.searchParams.get('stack')==='django');
 await page.waitForFunction(()=>/^port: 8000/m.test(document.querySelector('#kit-code-previewrc').value));
 assert.equal(await tab('.previewrc.yaml').getAttribute('aria-selected'),'true','the open tab stays open');
 assert.match(await page.locator('.kit-stack-about').innerText(),/Django · Full-stack · Python/);
 await kitIdle(page);await page.getByRole('switch',{name:/^Redis/}).check();
 await page.waitForFunction(()=>/^\s+redis:/m.test(document.querySelector('#kit-code-previewrc').value));
 assert.match(await page.locator('#kit-status').innerText(),/updated/i);
 await kitIdle(page);await page.getByRole('switch',{name:/^MySQL/}).check();
 await page.waitForFunction(()=>/^\s+mysql: 8\.4/m.test(document.querySelector('#kit-code-previewrc').value));
 assert.match(await status(),/Passes Previa's checks/);
 // A custom worker command with shell syntax runs through sh -c; typing keeps focus.
 await kitIdle(page);await page.getByRole('switch',{name:/^Background worker/}).check();
 await page.locator('#kit-worker-with').waitFor();await kitIdle(page);
 await page.locator('#kit-worker-with').selectOption('custom');
 const custom=page.locator('#kit-worker-cmd');await custom.waitFor();await kitIdle(page);
 await custom.fill('celery -A mysite worker && echo done');
 await page.waitForFunction(()=>/"sh", "-c", "celery -A mysite worker && echo done"/.test(document.querySelector('#kit-code-previewrc').value));
 assert(await page.locator('#kit-worker-cmd').evaluate(e=>e===document.activeElement),'typing keeps focus in the command');
 // Out-of-range choices are reported, never written.
 await kitIdle(page);await page.locator('.kit-advanced summary').click();
 await page.locator('#kit-port').fill('70000');
 await page.waitForFunction(()=>/1 to fix/.test(document.querySelector('.kit-check').innerText));
 assert.match(await page.locator('.kit-problems').innerText(),/Port: Use a port/);
 assert.match(await file('previewrc').inputValue(),/^port: 8000/m);
 // Wait until the page itself has refreshed for 70000 (the file check can answer first).
 await page.waitForURL(url=>url.searchParams.get('port')==='70000');await kitIdle(page);
 await page.locator('#kit-port').fill('8001');
 await page.waitForFunction(()=>/^port: 8001/m.test(document.querySelector('#kit-code-previewrc').value));
 // Editing marks the tab, is checked by the server as you type, and survives a change of choices.
 await kitIdle(page);const yaml=file('previewrc');
 await yaml.click();await page.keyboard.press('Control+End');
 await page.keyboard.type('\nenv:\n  - FEATURE_FLAGS=preview');
 assert(await page.locator('#kit-tab-previewrc[data-edited]').count()===1,'the tab shows the file was edited');
 assert(await page.locator('#kit-panel-previewrc [data-kit-edited]').isVisible());
 await page.waitForFunction(()=>/Your edited .previewrc.yaml passes/.test(document.querySelector('.kit-check').innerText));
 await yaml.evaluate(e=>{e.value=e.value.replace('port: 8001','port: eighty');e.dispatchEvent(new Event('input',{bubbles:true}));});
 await page.waitForFunction(()=>/1 to fix/.test(document.querySelector('.kit-check').innerText));
 assert.match(await page.locator('.kit-problems').innerText(),/Line 4 port:/);
 assert(await page.locator('#kit-panel-previewrc .kit-highlight').evaluate(e=>e.textContent.includes('FEATURE_FLAGS=preview')),'the highlight follows the text');
 await kitIdle(page);await page.getByRole('switch',{name:/^PostgreSQL/}).check();
 await page.waitForFunction(()=>document.querySelector('#kit-postgres').closest('.kit-row').classList.contains('is-on'));await kitIdle(page);
 assert(await page.locator('#kit-tab-previewrc[data-edited]').count()===1,'the new editor keeps the edit');
 assert.match(await yaml.inputValue(),/FEATURE_FLAGS=preview/,'edits survive a change of choices');
 assert.doesNotMatch(await yaml.inputValue(),/postgres:/,'an edited file no longer follows the choices');
 await page.waitForFunction(()=>/1 to fix/.test(document.querySelector('.kit-check').innerText));
 // Copy and Download use the editor's text.
 await context.grantPermissions(['clipboard-read','clipboard-write'],{origin});
 await page.getByRole('button',{name:'Copy .previewrc.yaml',exact:true}).click();
 assert.equal(await page.evaluate(()=>navigator.clipboard.readText()),await yaml.inputValue());
 const [request,download]=await Promise.all([page.waitForRequest(r=>r.url().endsWith('/starter-kits/download/previewrc')),page.waitForEvent('download'),page.getByRole('button',{name:'Download .previewrc.yaml',exact:true}).click()]);
 assert.match(download.suggestedFilename(),/^\.?previewrc\.yaml$/,'browsers may drop the leading dot');
 assert.equal(postedContent(request),await yaml.inputValue(),'Download sends the edited file');
 // Reset brings back the newest generated file and its checks.
 await page.locator('#kit-panel-previewrc [data-kit-reset]').click();
 assert.match(await yaml.inputValue(),/postgres: 17/);
 assert.equal(await page.locator('#kit-tab-previewrc[data-edited]').count(),0);
 assert.match(await status(),/Passes Previa's checks/);
 // The sticky editor stays beside the choices: it never covers the guidance below.
 await page.setViewportSize({width:1280,height:640});
 for(const card of await page.locator('.kit-guide .kit-card').all()){
  await card.scrollIntoViewIfNeeded();
  assert(await card.evaluate(e=>{const r=e.getBoundingClientRect();return e.contains(document.elementFromPoint(r.left+r.width/2,r.top+24));}),'a guidance card is covered by the editor');
 }
 await page.setViewportSize({width:1440,height:900});
 // A static site needs no services, setup or worker.
 await kitIdle(page);await page.locator('.kit-stack',{hasText:'React + Vite'}).click();
 await page.waitForFunction(()=>document.querySelector('.kit-quiet'));
 assert.doesNotMatch(await yaml.inputValue(),/services:|setup:|processes:/);
 for(const width of [390,820]){await page.setViewportSize({width,height:900});await assertNoOverflow();}
});

test('starter kits work without JavaScript and need a signed-in user',async()=>{
 assert.equal((await context.request.get(`${origin}/starter-kits`,{maxRedirects:0})).status(),303);
 const noJS=await browser.newContext({javaScriptEnabled:false,acceptDownloads:true});
 try {
  const plain=await noJS.newPage();
  await plain.goto(`${origin}/auth/github/login`);await plain.waitForURL(`${origin}/dashboard`);
  await plain.goto(`${origin}/starter-kits`);
  await plain.locator('.kit-stack',{hasText:'Ruby on Rails'}).click();
  await plain.getByRole('switch',{name:/^PostgreSQL/}).check();
  await plain.getByRole('button',{name:'Update files',exact:true}).click();
  await plain.waitForURL(url=>url.searchParams.get('stack')==='rails');
  // Without tabs, both files show as text areas, one under the other.
  assert(await plain.locator('[data-kit-tabs]').isHidden());
  assert(await plain.locator('#kit-code-dockerfile').isVisible()&&await plain.locator('#kit-code-previewrc').isVisible());
  const config=await plain.locator('#kit-code-previewrc').inputValue();
  assert.match(config,/^port: 3000/m);assert.match(config,/postgres: 17 /);assert.match(config,/health_check_path: \/up/);
  assert.match(await plain.locator('#kit-code-dockerfile').inputValue(),/libpq-dev/);
  await plain.locator('#kit-code-previewrc').fill(config.replace('resource_tier: standard','resource_tier: large'));
  const [request,download]=await Promise.all([plain.waitForRequest(r=>r.url().endsWith('/starter-kits/download/previewrc')),plain.waitForEvent('download'),plain.getByRole('button',{name:'Download .previewrc.yaml',exact:true}).click()]);
  assert.match(download.suggestedFilename(),/^\.?previewrc\.yaml$/,'browsers may drop the leading dot');
  assert.match(postedContent(request),/resource_tier: large/,'Download posts the edited text');
 } finally {await noJS.close();}
});
