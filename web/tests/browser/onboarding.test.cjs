const {test, before, after} = require('node:test');
const assert = require('node:assert/strict');
const {chromium} = require('playwright');
const {mkdirSync} = require('node:fs');
const path = require('node:path');

const origin = process.env.SETUP_BROWSER_URL;
const setupPath = process.env.SETUP_PATH;
const pr42 = process.env.SETUP_PR42;
const pr43 = process.env.SETUP_PR43;
let browser;
before(async () => { browser = await chromium.launch({headless: true, ...(process.env.UI_BROWSER_EXECUTABLE ? {executablePath: process.env.UI_BROWSER_EXECUTABLE} : {})}); });
after(async () => { await browser?.close(); });

async function session(login = 'owner', options = {}) {
  const context = await browser.newContext({viewport: {width: 1440, height: 1000}, ...options});
  const page = await context.newPage();
  page.setDefaultTimeout(10000);
  const errors = [];
  page.on('pageerror', e => errors.push(e.message));
  page.on('console', m => { if (/Content Security Policy|Refused to/.test(m.text())) errors.push(m.text()); });
  await context.request.get(`${origin}/__fixture/identity?login=${login}`);
  await page.goto(`${origin}/auth/github/login`);
  return {context, page, errors};
}

async function screenshot(page, name) {
  if (process.env.UI_BROWSER_SCREENSHOTS !== '1') return;
  mkdirSync(path.join(__dirname, 'artifacts'), {recursive: true});
  await page.waitForTimeout(350); // Let view transitions and the toast settle.
  await page.screenshot({animations: 'disabled', path: path.join(__dirname, 'artifacts', `setup-${name}.png`), fullPage: true});
}

const stepState = (page, id) => page.locator(`#setup-${id}`).getAttribute('data-state');
const waitForStep = (page, id, state) => page.locator(`#setup-${id}[data-state="${state}"]`).waitFor();
const fixture = (page, name) => page.request.post(`${origin}/__fixture/${name}`).then(r => assert.equal(r.status(), 204, `fixture ${name}`));

test('the Overview points each role at its next step', async () => {
  const {context, page, errors} = await session('owner');
  try {
    await page.goto(`${origin}/dashboard`);
    const steps = page.locator('.ov-setup .ov-step');
    assert.equal(await steps.count(), 4);
    assert.match(await steps.nth(0).textContent(), /Connect a repository/);
    assert.equal(await steps.nth(0).getAttribute('class').then(c => c.includes('is-done')), true);
    assert.match(await steps.nth(1).locator('.ov-step-action').getAttribute('href'), /\/repositories\/[0-9a-f-]{36}\/setup$/);
    await screenshot(page, 'overview-owner');
    assert.deepEqual(errors, []);
  } finally { await context.close(); }
  const newcomer = await session('newdev');
  try {
    await newcomer.page.goto(`${origin}/dashboard`);
    const first = newcomer.page.locator('.ov-setup .ov-step').first();
    assert.match(await first.textContent(), /Get access to a repository/);
    assert.match(await first.textContent(), /Olive Owner \(@owner\)/);
    assert.equal(await first.locator('.ov-step-action').count(), 0, 'a Developer cannot connect repositories');
    assert.deepEqual(newcomer.errors, []);
  } finally { await newcomer.context.close(); }
});

test('the checklist takes an owner from a check to an opened preview, live', async () => {
  const {context, page, errors} = await session('owner');
  try {
    await page.goto(`${origin}${setupPath}?pr=${pr42}`);
    await page.locator('html[data-live-state="live"]').waitFor();
    assert.equal(await page.locator('.setup-step-row').count(), 5);
    assert.equal(await stepState(page, 'connection'), 'done');
    assert.equal(await stepState(page, 'configuration'), 'current');
    assert.equal(await page.locator('.repo-tabs a[aria-current="page"]').textContent().then(t => t.trim()), 'Setup');
    await screenshot(page, 'start');

    // Ask for a check: it is on its way, then the worker finds no configuration.
    await page.locator('#setup-configuration button', {hasText: 'Check setup'}).click();
    await page.locator('.toast', {hasText: 'Setup check started'}).waitFor();
    assert.equal(await stepState(page, 'configuration'), 'working');
    assert.equal(new URL(page.url()).searchParams.has('saved'), false, 'the confirmation does not repeat on reload');
    await page.locator('html[data-live-state="live"]').waitFor();
    await fixture(page, 'check?tree=missing');
    await waitForStep(page, 'configuration', 'problem');
    const kit = page.locator('#setup-configuration a', {hasText: 'Create it with a Starter kit'});
    assert.match(await kit.getAttribute('href'), /\/starter-kits\?stack=custom$/);
    assert.equal(await stepState(page, 'requirements'), 'waiting');
    await screenshot(page, 'problem');

    // A fix is pushed: the new commit is unchecked; checking it passes.
    await fixture(page, 'push');
    await page.locator('#setup-configuration .setup-finding', {hasText: 'earlier commit'}).waitFor();
    assert.equal(await stepState(page, 'configuration'), 'current');
    await page.locator('#setup-configuration button', {hasText: 'Check setup'}).click();
    await waitForStep(page, 'configuration', 'working');
    await page.locator('html[data-live-state="live"]').waitFor();
    await fixture(page, 'check?tree=passed');
    await waitForStep(page, 'requirements', 'current');
    assert.equal(await stepState(page, 'configuration'), 'done');
    const facts = page.locator('.setup-facts-list');
    assert.match(await facts.textContent(), /Port\s*8080/);
    assert.match(await facts.textContent(), /PostgreSQL 17/);
    assert.match(await facts.textContent(), /LOG_LEVEL/);
    assert.doesNotMatch(await facts.textContent(), /debug/, 'configuration values are never shown');
    assert.match(await page.locator('.setup-secret-names').textContent(), /API_KEY/);
    await screenshot(page, 'ready-to-build');

    // Build: the request goes through the pull request's Build action.
    await page.locator('#setup-requirements button', {hasText: 'Build preview'}).click();
    await page.locator('.toast', {hasText: 'Build requested'}).waitFor();
    assert.equal(await stepState(page, 'preview'), 'working');
    await page.locator('html[data-live-state="live"]').waitFor();
    await fixture(page, 'building');
    await page.locator('#setup-preview a', {hasText: 'Follow the build log'}).waitFor();
    await fixture(page, 'deploy');
    await waitForStep(page, 'opened', 'current');
    assert.equal(await stepState(page, 'preview'), 'done');

    // Opening it from Previa finishes setup, in this tab too.
    const open = page.locator('#setup-opened a', {hasText: 'Open preview'});
    assert.equal(await open.getAttribute('target'), '_blank');
    // The new tab asks Previa, which records the open and sends it on to the
    // preview (whose fixture host does not exist, so it stops there).
    const redirected = context.waitForEvent('response', r => new URL(r.url()).pathname.endsWith('/open'));
    const [preview] = await Promise.all([context.waitForEvent('page'), open.click()]);
    const response = await redirected;
    assert.equal(response.status(), 303);
    assert.equal(response.headers()['location'], 'https://pr-42-fixture.preview.test');
    assert.equal(response.headers()['referrer-policy'], 'no-referrer');
    await preview.close();
    await waitForStep(page, 'opened', 'done');
    assert.match(await page.locator('#setup-title').textContent(), /Ready for review/);
    assert.match(await page.locator('#setup-opened').textContent(), /Opened by you/);
    const review = page.locator('.setup-next button', {hasText: 'Request review'});
    assert.equal(await review.isDisabled(), true, 'review requests are labelled as not available yet');
    for (const theme of ['light', 'dark']) {
      await page.evaluate(t => { document.documentElement.dataset.theme = t; }, theme);
      await screenshot(page, `done-${theme}`);
    }
    await page.setViewportSize({width: 390, height: 844});
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'no sideways scrolling on a phone');
    await screenshot(page, 'done-mobile');
    assert.deepEqual(errors, []);
  } finally { await context.close(); }
});

test('the pull request picker is a menu of links that closes before navigating', async () => {
  const {context, page, errors} = await session('owner');
  try {
    await page.goto(`${origin}${setupPath}?pr=${pr42}`);
    const picker = page.locator('details.setup-picker');
    await picker.locator('summary').click();
    assert.equal(await picker.evaluate(d => d.open), true);
    await screenshot(page, 'picker');
    await page.keyboard.press('Escape');
    assert.equal(await picker.evaluate(d => d.open), false, 'Escape closes the menu');
    await picker.locator('summary').click();
    await Promise.all([page.waitForURL(u => u.searchParams.get('pr') === pr43), picker.locator('a', {hasText: '#43'}).click()]);
    assert.equal(await page.locator('details.setup-picker').evaluate(d => d.open), false);
    assert.match(await page.locator('.setup-picker-current').textContent(), /#43/);
    await page.setViewportSize({width: 390, height: 844});
    await page.locator('details.setup-picker summary').click();
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'the open menu fits a phone');
    await screenshot(page, 'picker-mobile');
    assert.deepEqual(errors, []);
  } finally { await context.close(); }
});

test('a Developer sees who manages secrets; a Reviewer reviews previews instead', async () => {
  const dev = await session('dev');
  try {
    await dev.page.goto(`${origin}${setupPath}?pr=${pr43}`);
    const secrets = dev.page.locator('.setup-secrets');
    assert.match(await secrets.textContent(), /Secrets are managed by Owners and Admins/);
    assert.match(await secrets.textContent(), /Olive Owner \(@owner\) or admin \(@admin\)/);
    assert.equal(await dev.page.locator('.repo-tabs a', {hasText: 'Secrets'}).count(), 0);
    await screenshot(dev.page, 'developer');
    assert.deepEqual(dev.errors, []);
  } finally { await dev.context.close(); }
  const reviewer = await session('reviewer');
  try {
    await reviewer.page.goto(`${origin}${setupPath}`);
    assert.equal(new URL(reviewer.page.url()).pathname, '/previews');
  } finally { await reviewer.context.close(); }
});

test('checking works without JavaScript', async () => {
  const {context, page} = await session('owner', {javaScriptEnabled: false});
  try {
    await page.goto(`${origin}${setupPath}?pr=${pr43}`);
    await page.locator('#setup-configuration button', {hasText: 'Check setup'}).click();
    await page.locator('.toast', {hasText: 'Setup check started'}).waitFor();
    assert.equal(await stepState(page, 'configuration'), 'working');
    await page.locator('details.setup-picker summary').click();
    assert.equal(await page.locator('.setup-picker-menu a', {hasText: '#42'}).isVisible(), true, 'the picker opens natively');
  } finally { await context.close(); }
});
