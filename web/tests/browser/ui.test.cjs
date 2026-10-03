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
  assert.equal((await context.request.get(`${origin}/dashboard`, {maxRedirects: 0})).status(), 303);
  await page.goto(origin);
  assert.match(await page.locator('h1').innerText(), /Review the branch/);
  assert.equal(await page.locator('[data-queue-form]').count(), 0);
  await page.getByRole('link', {name: 'The workflow', exact: true}).click();
  assert.equal(new URL(page.url()).hash, '#workflow');
});

test('responsive dashboard sticky navigation and honest placeholders', async () => {
  await login();
  assert(await page.getByRole('button', {name: 'Connect a repository'}).isDisabled());
  for (const width of [320, 390, 768, 1024, 1440]) {
    await page.setViewportSize({width, height: 844});
    await assertNoOverflow();
    await page.evaluate(() => scrollTo({top: 500, behavior: 'instant'}));
    assert(await page.evaluate(() => scrollY > 0));
    assert.equal(await page.locator('.app-header').evaluate(e => Math.round(e.getBoundingClientRect().top)), 0);
    await page.evaluate(() => scrollTo({top: 0, behavior: 'instant'}));
  }
  for (const id of ['repositories', 'environments', 'builds']) {
    assert.equal(await page.locator(`#${id}`).count(), 1);
  }
  assert.equal(await page.locator('.tools-disclosure').getAttribute('open'), null);
});

test('mobile navigation closes and reveals the selected section', async () => {
  await login();
  await page.setViewportSize({width: 390, height: 844});
  await page.emulateMedia({reducedMotion: 'reduce'});
  await page.locator('.mobile-workspace-nav summary').click();
  await page.locator('.mobile-workspace-nav a[href="/dashboard#builds"]').click();
  assert.equal(await page.locator('.mobile-workspace-nav').getAttribute('open'), null);
  assert(await page.locator('#builds').evaluate(e => e.getBoundingClientRect().top >= 118));
  await page.locator('.mobile-workspace-nav summary').click();
  await page.locator('.mobile-workspace-nav a[href="/dashboard#queue-check"]').click();
  await page.waitForFunction(() => document.querySelector('.tools-disclosure').open);
  assert.notEqual(await page.locator('.tools-disclosure').getAttribute('open'), null);
});

test('logout modal preserves page scroll and traps and restores focus', async () => {
  await login();
  await page.evaluate(() => scrollTo({top: 450, behavior: 'instant'}));
  const scroll = await page.evaluate(() => scrollY);
  let logoutRequests = 0;
  page.on('request', request => { if (request.method() === 'POST' && request.url().endsWith('/auth/logout')) logoutRequests++; });
  await openLogout();
  assert.equal(page.url(), `${origin}/dashboard`);
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
  await page.locator('.tools-disclosure summary').click();
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

test('content and native confirmation remain usable without JavaScript', async () => {
  const noJS = await browser.newContext({javaScriptEnabled: false});
  try {
    const nativePage = await noJS.newPage();
    await nativePage.goto(origin);
    assert.equal(await nativePage.locator('#principles .principles-grid').evaluate(e => getComputedStyle(e).opacity), '1');
    await nativePage.goto(`${origin}/auth/github/login`);
    await nativePage.waitForURL(`${origin}/dashboard`);
    await nativePage.locator('.account-menu summary').click();
    await nativePage.getByRole('button', {name: 'Log out', exact: true}).click();
    assert(await nativePage.locator('#logout-confirmation').evaluate(e => e.matches(':modal')));
    await nativePage.getByRole('button', {name: 'Cancel', exact: true}).click();
    assert.equal(await nativePage.locator('#logout-confirmation').evaluate(e => e.open), false);
  } finally { await noJS.close(); }
});

test('configured installation dashboard renders records and remains responsive', async () => {
  await login();
  await page.goto(`${origin}/fixture-installations`);
  await page.emulateMedia({reducedMotion: 'reduce'});
  assert.match(await page.locator('.installation-summary').innerText(), /fixture-owner/);
  assert.equal(await page.getByText('Access granted', {exact: true}).count(), 1);
  assert.equal(await page.getByText('Access unavailable', {exact: true}).count(), 1);
  const form = page.locator('form[action="/installations/start"]');
  assert.equal(await form.getAttribute('method'), 'post');
  assert(await form.locator('[name="csrf_token"]').inputValue());
  assert(await page.getByRole('button', {name: 'Install on a repo'}).isEnabled());
  for (const width of [320, 390, 768, 1440]) {
    await page.setViewportSize({width, height: 844});
    await assertNoOverflow();
  }
});
