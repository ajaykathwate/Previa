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
  assert.match(await page.locator('.dashboard-heading').innerText(), /design-review/);
}
async function readyPanel(id) {
  await page.locator(`[data-selected-repo="${id}"]`).waitFor();
  await page.waitForFunction(()=>!document.getElementById('repository-detail').hasAttribute('aria-busy'));
}
async function openLogout() {
  // Real pointer clicks avoid automation scrolling sticky-header elements into view.
  for (const locator of [page.locator('.account-menu summary'), page.getByRole('button', {name: 'Log out', exact: true})]) {
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
  for (const route of ['/dashboard','/repositories','/builds','/environments','/developer-tools']) {
    assert.equal((await context.request.get(`${origin}${route}`, {maxRedirects:0})).status(),303);
  }
  await page.goto(origin);
  assert.match(await page.locator('h1').innerText(), /Review the branch/);
  assert.equal(await page.locator('[data-queue-form]').count(), 0);
  await page.getByRole('link', {name: 'The workflow', exact: true}).click();
  assert.equal(new URL(page.url()).hash, '#workflow');
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

test('home reveals once pauses offscreen and respects reduced motion', async () => {
  await page.goto(origin);
  for (const width of [320, 390, 768, 1024, 1440]) {
    await page.setViewportSize({width, height: 844});
    await assertNoOverflow();
  }
  assert(await page.locator('.scroll-pending').count() > 0);
  await page.locator('#principles').scrollIntoViewIfNeeded();
  await page.waitForFunction(() => document.querySelector('#principles [data-scroll-reveal]').classList.contains('is-visible'));
  await page.waitForFunction(() => getComputedStyle(document.querySelector('.preview-window')).animationPlayState === 'paused');
  await page.evaluate(() => scrollTo({top: 0, behavior: 'instant'}));
  await page.waitForFunction(() => getComputedStyle(document.querySelector('.preview-window')).animationPlayState === 'running');
  assert(await page.locator('#principles .is-visible').count() > 0);
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
  assert.match(await story.locator('#story-discover').innerText(), /signed webhook/);
  assert.match(await story.locator('[data-story-stage][aria-current="step"]').innerText(), /^Discover/);
  assert.match(await next.innerText(), /when does it actually build/);
  assert(await page.evaluate(() => document.activeElement.matches('[data-story-next]')));
  // A stage reveals every turn up to it and moves focus there.
  await story.locator('[data-story-stage]', {hasText: 'Route'}).click();
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
  assert.match(await item.locator('.faq-answer .chat-bubble').innerText(), /rootless BuildKit/);
  // Topics and search narrow the questions; a status line counts them.
  const visible = faq.locator('[data-faq-item]:not([hidden])');
  await faq.getByRole('button', {name: /^Security/}).click();
  assert.equal(await visible.count(), 5);
  assert.deepEqual([...new Set(await visible.evaluateAll(items => items.map(i => i.dataset.topic)))], ['security']);
  await faq.getByRole('button', {name: /^All topics/}).click();
  await faq.locator('[data-faq-search]').fill('kernel');
  assert.equal(await visible.count(), 1);
  assert.equal(await faq.locator('[data-faq-status]').innerText(), '1 of 22 questions shown.');
  await faq.locator('[data-faq-search]').fill('zzz-nothing');
  assert(await faq.locator('[data-faq-empty]').isVisible());
  await faq.getByRole('button', {name: 'show every question', exact: true}).click();
  assert.equal(await visible.count(), 22);
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
    assert.equal(await nativePage.locator('#principles .principles-grid').evaluate(e => getComputedStyle(e).opacity), '1');
    // How it works is a full transcript, and FAQ questions open natively.
    assert.equal(await nativePage.locator('[data-story-turn]').count(), 7);
    assert.equal(await nativePage.locator('[data-story-outro]').isVisible(), true);
    assert.equal(await nativePage.locator('[data-story-composer]').isVisible(), false);
    assert.equal(await nativePage.locator('[data-faq-tools]').isVisible(), false);
    await nativePage.locator('#faq-secrets summary').click();
    assert.match(await nativePage.locator('#faq-secrets .faq-answer').innerText(), /AES-256-GCM/);
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
  assert.equal(await page.getByRole('link', {name: 'Open preview', exact: true}).getAttribute('href'), 'https://pr-3-1-fixture.preview.example.com');
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

test('unavailable repository PRs offer no actions and say why', async () => {
  await login();
  await page.goto(`${origin}/repositories/2/pull-requests/1`);
  assert.match(await page.locator('[data-build-unavailable]').innerText(), /GitHub access to this repository is unavailable[\s\S]*Manage connections/);
  assert(await page.getByRole('button', {name: 'Build latest commit', exact: true}).isDisabled(), 'the action is shown, disabled');
  assert.equal(await page.getByRole('button', {name: /Retry|Rebuild|Tear down|Cancel build/}).count(), 0);
  assert.equal(await page.locator('form[action$="/build"]').count(), 0, 'nothing can be submitted');
});

test('a blocked build stays visible, disabled, with the reason and the fix', async () => {
  await login();
  await page.goto(`${origin}/repositories/1/pull-requests/9`);
  const blocked = page.getByRole('button', {name: 'Build latest commit', exact: true});
  assert(await blocked.isDisabled());
  assert.equal(await blocked.getAttribute('aria-describedby'), 'build-unavailable');
  assert.match(await page.locator('#build-unavailable').innerText(), /no preview domain[\s\S]*PREVIEW_BASE_DOMAIN/);
  assert.equal(await page.locator('form[action$="/build"]').count(), 0, 'nothing can be submitted');
  assert.match(await page.locator('[data-pr-status] h2').innerText(), /No preview yet/);
  for (const width of [320, 390, 1440]) {
    await page.setViewportSize({width, height: 844});
    await assertNoOverflow();
  }
});

test('Environments and Builds show addresses, waits and failures, and teardown confirms per row', async () => {
  await login();
  await page.goto(`${origin}/environments?fixture=rows`);
  const preview = page.getByRole('link', {name: 'Open preview'});
  assert.equal(await preview.getAttribute('href'), 'https://pr-3-1-fixture.preview.example.com');
  assert.match(await page.locator('.activity-table').innerText(), /Earlier than the latest commit/);
  assert.match(await page.locator('.activity-table').innerText(), /The app did not become ready/);
  for (const width of [320, 390, 768, 1440]) {
    await page.setViewportSize({width, height: 844});
    await assertNoOverflow();
  }
  await page.setViewportSize({width: 1440, height: 900});
  await page.getByRole('button', {name: 'Tear down the preview of fixture-owner/repository-1 #3'}).click();
  await page.waitForFunction(() => document.querySelector('#teardown-1-3').matches(':modal'));
  await page.locator('#teardown-1-3').getByRole('button', {name: 'Tear down preview', exact: true}).click();
  await page.waitForURL(url => url.pathname === '/environments' && url.searchParams.get('page') === '1');
  await page.goto(`${origin}/builds?fixture=rows`);
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
  const manage = page.getByRole('button', {name: 'Manage connections'});
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
  await page.locator('.pr-table tbody a').first().click();
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
    await p.locator('.pr-table tbody a').click();assert.equal(new URL(p.url()).pathname,'/repositories/1/pull-requests/25');
    await p.locator('.pr-directory-link').click();assert.equal(await p.locator('#pull-request-search').inputValue(),'preview 25');
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
 assert(await page.getByRole('button',{name:'Sync Pull Requests',exact:true}).isDisabled());
 await mode.click();await page.waitForFunction(()=>document.querySelector('#build-policy').matches(':modal'));
 await page.waitForTimeout(3500);assert(await page.locator('#build-policy').evaluate(e=>e.matches(':modal')));assert.match(await page.locator('.pr-summary').innerText(),/Syncing/);
 const requestsBeforeEscape=policyRequests;await page.keyboard.press('Escape');assert.equal(policyRequests,requestsBeforeEscape);
 await page.waitForFunction(()=>!document.querySelector('.pr-summary').textContent.includes('Syncing'));
 assert(await page.getByRole('button',{name:'Sync Pull Requests',exact:true}).isEnabled());
 assert.equal(await mode.getAttribute('aria-checked'),'false');
 assert.equal(await page.locator('#pull-request-search').inputValue(),'preview 25');assert.equal(await page.locator('.pr-table tbody tr').count(),1);
 assert(await page.evaluate(()=>document===window.originalDocument));
});




test('unavailable repositories and empty PR states remain clear and cannot request mutations',async()=>{
 await login();await page.goto(`${origin}/repositories/2?access=all&pr_state=closed`);
 assert.match(await page.locator('.pr-summary').innerText(),/Repository unavailable/);
 assert(await page.getByRole('button',{name:'Sync Pull Requests',exact:true}).isDisabled());
 assert(await page.getByRole('switch',{name:'Automatic builds',exact:true}).isDisabled());
 assert.equal(await page.locator('.pr-table').count(),0);assert.match(await page.locator('#pull-request-results').innerText(),/No pull requests/);
 assert.equal(await page.locator('#repository-filters').count(),0);
});

test('expired sessions during panel selection navigate the whole document to sign-in',async()=>{
 await login();await page.goto(`${origin}/repositories/1`);
 const token=await page.locator('#logout-confirmation input[name=csrf_token]').inputValue();
 assert.equal((await context.request.post(`${origin}/auth/logout`,{form:{csrf_token:token},maxRedirects:0})).status(),303);
 await page.getByRole('link',{name:'Repositories',exact:true}).last().click();await page.waitForURL(`${origin}/dashboard`);
 assert.equal(await page.locator('#repository-detail').count(),0);assert.match(await page.locator('.dashboard-heading').innerText(),/design-review/);
});







test('dedicated repository and PR pages preserve filters through breadcrumbs and history',async()=>{
 await login();await page.goto(`${origin}/repositories?q=fixture&page=2`);
 assert.equal(await page.locator('#repository-detail').count(),0);
 await page.evaluate(()=>window.directoryDocument=document);
 await page.locator('[data-repo-link="25"]').click();await page.waitForURL(url=>url.pathname==='/repositories/25');
 assert.equal(await page.locator('#repository-filters').count(),0);
 assert.equal(await page.evaluate(()=>document===window.directoryDocument),false);
 assert.match(await page.locator('.pr-breadcrumb').innerText(),/fixture-owner.*repository-25/s);
 const synced=page.locator('.pr-summary time');
 assert.equal(await synced.getAttribute('title'),'4 Oct 2026, 10:00 UTC');
 assert.notEqual(await synced.innerText(),'4 Oct 2026, 10:00 UTC','shown relative, with the exact time as its title');
 await page.locator('#pull-request-search').fill('preview 25');await page.waitForFunction(()=>document.querySelectorAll('.pr-table tbody tr').length===1);
 await page.locator('.pr-table tbody a').click();await page.waitForURL(url=>url.pathname==='/repositories/25/pull-requests/25');
 assert.match(await page.locator('.pr-breadcrumb [aria-current=page]').innerText(),/#25.*A careful change to preview 25/);
 assert.equal(await page.locator('#pull-request-search').count(),0);
 await page.locator('.pr-breadcrumb').getByRole('link',{name:'repository-25',exact:true}).click();await page.waitForURL(url=>url.pathname==='/repositories/25');
 assert.equal(await page.locator('#pull-request-search').inputValue(),'preview 25');assert.equal(await page.locator('.pr-table tbody tr').count(),1);
 await page.locator('.pr-directory-link').click();await page.waitForURL(url=>url.pathname==='/repositories');
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
 await page.locator('.pr-directory-link').click();await page.waitForURL(url=>url.pathname==='/repositories/1');
});

test('failed PR searches preserve records and query bookmarks redirect to dedicated pages',async()=>{
 await login();await page.goto(`${origin}/repositories?repo_id=1&pr=7&pr_q=preview`);
 await page.waitForURL(url=>url.pathname==='/repositories/1/pull-requests/7');
 assert.match(await page.locator('.pr-detail-grid').innerText(),/No build requested yet/);
 await page.locator('.pr-directory-link').click();await page.waitForURL(url=>url.pathname==='/repositories/1');
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
 assert.equal(await page.locator('.automatic-switch').getAttribute('aria-checked'),'true');
 assert.notEqual(await page.locator('.automatic-switch').evaluate(e=>getComputedStyle(e).backgroundColor),'rgba(0, 0, 0, 0)');
 await page.getByRole('button',{name:'Sync Pull Requests'}).click();await page.locator('.sync-state').filter({hasText:'Syncing'}).waitFor();
 await page.waitForFunction(()=>!document.querySelector('.pr-summary').textContent.includes('Syncing'));await readyPanel('1');
 await page.locator('.pr-table tbody a').first().click();await page.waitForURL(url=>url.pathname.includes('/pull-requests/'));
 assert.equal(await page.getByRole('switch',{name:'Automatic builds'}).count(),0);
 assert.equal(await page.getByRole('button',{name:'Sync Pull Requests'}).count(),0);assert.equal(await page.locator('#build-policy').count(),0);
 const sizes=await page.locator('.pr-breadcrumb').evaluate(e=>Array.from(e.children).map(child=>getComputedStyle(child).fontSize));
 assert(sizes.length>2 && sizes.every(size=>size===sizes[0]), 'breadcrumb text and separators share one size');
 await page.locator('.pr-directory-link').click();await page.waitForURL(url=>url.pathname==='/repositories/1');
 assert.equal(await page.locator('.automatic-switch').getAttribute('aria-checked'),'true');
 assert.equal(new URL(page.url()).searchParams.get('page'),'2');
 await page.locator('.pr-directory-link').click();await page.waitForURL(url=>url.pathname==='/repositories');
 assert.equal(await page.getByRole('switch',{name:'Automatic builds'}).count(),0);assert.equal(await page.getByRole('button',{name:'Sync Pull Requests'}).count(),0);
});

test('small repository collections use compact cards with natural height and no pagination',async()=>{
 await login();
 for(const count of [1,2,3,5]) {
  await page.goto(`${origin}/repositories?access=all&fixture_count=${count}`);
  assert.equal(await page.locator('.repository-row').count(),count);
  assert.equal(await page.getByRole('navigation',{name:'Repository pages'}).count(),0);
  assert.match(await page.locator('.repository-count').innerText(),new RegExp(`${count} repositor`));
  assert.equal(await page.locator('.repository-select').first().locator(':scope > :not(.repository-card-facts) svg').count(),2);
  assert.equal(await page.locator('.repository-select').first().locator('.repository-card-facts li').count(),4,'open PRs, live previews, build mode and last sync');
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
    // Cards include a facts row (open PRs, previews, mode, sync); a 960px screen leaves room to spare.
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
 assert.match(await page.locator('[data-command-item][aria-selected=true]').innerText(),/Environments/);
 await page.keyboard.press('Enter');await page.waitForURL(`${origin}/environments`);
 assert.equal(await page.locator('h1').innerText(),'Environments');
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
 assert.match(await page.locator('.pr-breadcrumb').innerText(),/repository-1/);
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
 await page.waitForURL(`${origin}/repositories/1/pull-requests/3/logs`);
 assert.match(await page.locator('.pr-breadcrumb').innerText(),/repository-1[\s\S]*#3[\s\S]*Build logs/);
 await page.waitForFunction(()=>document.querySelector('[data-log-connection]')?.dataset.state==='live');
 assert.equal(await page.locator('.log-line').count(),500,'the newest 500 lines render first');
 assert.equal(await page.evaluate(()=>window.__pwned),undefined,'script-looking output must stay inert');
 assert.equal(await page.locator('.log-lines script, .log-lines img').count(),0);
 assert.equal(await page.locator('#L519 .log-text').innerText(),'<img src=x onerror="window.__pwned = true">');
 assert(await page.locator('.log-lines').evaluate(e=>e.scrollHeight-e.scrollTop-e.clientHeight<24),'the newest output is in view');
 let documents=0;page.on('request',request=>{if(request.resourceType()==='document') documents++;});
 await context.request.post(`${origin}/__fixture/logs/append`,{form:{text:'Step 3/3 : RUN <b>npm test</b>'}});
 await page.locator('#L521').waitFor();
 assert.equal(await page.locator('#L521 .log-text').innerText(),'Step 3/3 : RUN <b>npm test</b>');
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
 assert.equal(await page.locator('#L1 .log-text').innerText(),'Retry attempt output');
 assert.equal(documents,0,'live logs never reload the page');
 for(const width of [320,390,768,1440]) {await page.setViewportSize({width,height:844});await assertNoOverflow();}
});

test('build logs paginate and stay readable without JavaScript',async()=>{
 await context.request.post(`${origin}/__fixture/logs/seed`);
 const noJS=await browser.newContext({javaScriptEnabled:false});
 try {
  const plain=await noJS.newPage();
  await plain.goto(`${origin}/auth/github/login`);await plain.waitForURL(`${origin}/dashboard`);
  await plain.goto(`${origin}/repositories/1/pull-requests/3/logs`);
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
 } finally {await noJS.close();}
});
