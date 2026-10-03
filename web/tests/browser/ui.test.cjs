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
  for (const route of ['/dashboard','/repositories','/builds','/environments','/developer-tools']) {
    assert.equal((await context.request.get(`${origin}${route}`, {maxRedirects:0})).status(),303);
  }
  await page.goto(origin);
  assert.match(await page.locator('h1').innerText(), /Review the branch/);
  assert.equal(await page.locator('[data-queue-form]').count(), 0);
  await page.getByRole('link', {name: 'The workflow', exact: true}).click();
  assert.equal(new URL(page.url()).hash, '#workflow');
});

test('responsive dashboard sticky navigation and honest placeholders', async () => {
  await login();
  await page.goto(`${origin}/repositories`);
  for (const width of [320, 390, 768, 1024, 1440]) {
    await page.setViewportSize({width, height: 844});
    await assertNoOverflow();
    await page.evaluate(() => scrollTo({top: 500, behavior: 'instant'}));
    assert.equal(await page.evaluate(() => scrollY), 0);
    const list = page.locator('.repository-table-scroll');
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
  await page.locator('.repository-table-scroll').evaluate(e => {e.scrollTop = 300;});
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

test('content and native confirmation remain usable without JavaScript', async () => {
  const noJS = await browser.newContext({javaScriptEnabled: false});
  try {
    const nativePage = await noJS.newPage();
    await nativePage.goto(origin);
    assert.equal(await nativePage.locator('#principles .principles-grid').evaluate(e => getComputedStyle(e).opacity), '1');
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
  } finally { await noJS.close(); }
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
