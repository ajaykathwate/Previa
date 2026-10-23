const {test, before, after} = require('node:test');
const assert = require('node:assert/strict');
const {chromium} = require('playwright');
const {mkdirSync} = require('node:fs');
const path = require('node:path');

const origin = process.env.TEST_DATA_BROWSER_URL;
const tabPath = process.env.TEST_DATA_PATH;
let browser;
before(async () => { browser = await chromium.launch({headless: true, ...(process.env.UI_BROWSER_EXECUTABLE ? {executablePath: process.env.UI_BROWSER_EXECUTABLE} : {})}); });
after(async () => { await browser?.close(); });

async function session(login = 'owner') {
  const context = await browser.newContext({viewport: {width: 1440, height: 1000}});
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
  await page.waitForTimeout(350); // Let the toast and any transition settle.
  const size = page.viewportSize();
  await page.evaluate(() => document.querySelector('.app-content')?.scrollTo(0, 0)).catch(() => {});
  const height = await page.evaluate(() => {
    const panel = document.querySelector('.app-content');
    return panel ? panel.scrollHeight + panel.getBoundingClientRect().top : document.documentElement.scrollHeight;
  }).catch(() => size.height);
  if (height > size.height) await page.setViewportSize({width: size.width, height: Math.min(Math.ceil(height), 6000)});
  await page.screenshot({animations: 'disabled', path: path.join(__dirname, 'artifacts', `test-data-${name}.png`)});
  if (height > size.height) await page.setViewportSize(size);
}

// shots takes light and dark screenshots at 1440 and 390 pixels, checking
// there is no sideways scrolling on a phone.
async function shots(page, name) {
  const size = page.viewportSize();
  for (const width of [1440, 390]) {
    await page.setViewportSize({width, height: width === 390 ? 844 : 1000});
    if (width === 390) {
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `${name}: no sideways scrolling on a phone`);
    }
    for (const theme of ['light', 'dark']) {
      await page.evaluate(t => { document.documentElement.dataset.theme = t; }, theme);
      await screenshot(page, `${name}-${width}-${theme}`);
    }
  }
  await page.evaluate(() => { delete document.documentElement.dataset.theme; });
  await page.setViewportSize(size);
}

const fixture = (page, name) => page.request.post(`${origin}/__fixture/${name}`).then(r => assert.equal(r.status(), 204, `fixture ${name}`));
const toast = (page, text) => page.locator('.toast', {hasText: text}).waitFor();
const live = page => page.locator('html[data-live-state="live"]').waitFor();
const status = page => page.locator('[data-baseline-status]').textContent().then(t => t.trim());

test('an owner turns Test data on in a modal and watches the first baseline, live', async () => {
  const {context, page, errors} = await session('owner');
  try {
    await page.goto(`${origin}${tabPath}`);
    assert.equal(await page.locator('.repo-tabs a[aria-current="page"]').textContent().then(t => t.trim()), 'Test data');
    assert.equal(await status(page), 'Off');
    await page.locator('.test-data-empty', {hasText: 'Test data is off'}).waitFor();
    await shots(page, 'off');

    // The switch opens a confirmation over the page; Cancel changes nothing.
    const before = page.url();
    await page.locator('button[data-open-dialog="turn-on-test-data"]').click();
    await page.locator('#turn-on-test-data[open]').waitFor();
    assert.equal(page.url(), before, 'the confirmation opens in place');
    assert.match(await page.locator('#turn-on-test-data').textContent(), /without repository secrets/);
    await screenshot(page, 'turn-on-dialog');
    await page.locator('#turn-on-test-data button', {hasText: 'Cancel'}).click();
    assert.equal(await page.locator('#turn-on-test-data[open]').count(), 0);
    assert.equal(await status(page), 'Off');

    // Confirming turns it on and asks for the first baseline.
    await page.locator('button[data-open-dialog="turn-on-test-data"]').click();
    await page.locator('#turn-on-test-data button[type=submit]').click();
    await toast(page, 'Test data is on');
    assert.equal(new URL(page.url()).searchParams.has('saved'), false, 'the confirmation does not repeat on reload');
    assert.equal(await status(page), 'Waiting');
    await page.locator('.test-data-refresh', {hasText: 'Test data turned on'}).waitFor();

    // The worker builds it; the tab follows without a reload.
    await live(page);
    await fixture(page, 'refresh?to=running');
    await page.locator('[data-baseline-status]', {hasText: 'Building'}).waitFor();
    await fixture(page, 'refresh?to=ready');
    await page.locator('[data-baseline-status]', {hasText: 'Ready'}).waitFor();
    const table = await page.locator('.test-data-table').textContent();
    assert.match(table, /PostgreSQL 17/);
    assert.match(table, /main@1111111/);
    assert.match(table, /4\.0 MiB/);
    await shots(page, 'ready');

    // A switch saves in place with a toast.
    await page.locator('form:has(input[name=csrf_token]) button[aria-labelledby="test-data-keep_data-label"]').click();
    await toast(page, 'Test data settings saved');
    assert.equal(await page.locator('button[aria-labelledby="test-data-keep_data-label"]').getAttribute('aria-checked'), 'false');

    // The baseline branch saves in place; a bad name is explained beside it.
    await live(page);
    const branch = page.locator('#test-data-branch');
    await branch.fill('two words');
    await branch.press('Enter');
    await page.locator('#test-data-branch-error', {hasText: 'Branch names cannot contain spaces'}).waitFor();
    await branch.fill('develop');
    await branch.press('Enter');
    await toast(page, 'Test data settings saved');
    await page.locator('.test-data-card-head code', {hasText: 'develop'}).waitFor();

    // Refresh baseline confirms in a modal that says it uses build minutes.
    await page.locator('button[data-open-dialog="refresh-baseline"]').click();
    await page.locator('#refresh-baseline[open]').waitFor();
    assert.match(await page.locator('#refresh-baseline').textContent(), /uses build minutes/);
    await page.locator('#refresh-baseline button[type=submit]').click();
    await toast(page, 'Baseline refresh requested');

    // A refresh's build log.
    await page.locator('.test-data-refresh a', {hasText: 'Build log'}).first().click();
    await page.locator('h1', {hasText: 'Baseline refresh #'}).waitFor();
    await page.locator('.log-line', {hasText: 'shop: seeded 2 orders'}).waitFor();
    await shots(page, 'log');
    assert.deepEqual(errors, []);
  } finally { await context.close(); }
});

test('a developer reads the tab without its settings; a reviewer is sent away', async () => {
  const dev = await session('dev');
  try {
    await dev.page.goto(`${origin}${tabPath}`);
    const settings = dev.page.locator('.test-data-settings');
    await settings.waitFor();
    assert.match(await settings.innerText(), /Refresh automatically\s+(On|Off)/, 'a Developer reads each setting as a value');
    assert.equal(await settings.locator('[role="switch"], form').count(), 0, 'no switches or forms for a Developer');
    assert.equal(await dev.page.locator('main [disabled]').count(), 0, 'no disabled controls');
    assert.equal(await dev.page.locator('#turn-off-test-data').count(), 0, 'no Turn off for a Developer');
    assert.equal(await dev.page.locator('#delete-baseline').count(), 0, 'no Delete for a Developer');
    assert.equal(await dev.page.locator('button[data-open-dialog="refresh-baseline"]').count(), 1, 'a Developer may refresh');
    await shots(dev.page, 'developer');
    assert.deepEqual(dev.errors, []);
  } finally { await dev.context.close(); }
  const reviewer = await session('reviewer');
  try {
    await reviewer.page.goto(`${origin}${tabPath}`);
    assert.match(new URL(reviewer.page.url()).pathname, /^\/previews$/, 'a Reviewer is sent to Previews');
  } finally { await reviewer.context.close(); }
});

test('an owner turns Test data off in a modal', async () => {
  const {context, page, errors} = await session('owner');
  try {
    await page.goto(`${origin}${tabPath}`);
    await page.locator('button[data-open-dialog="turn-off-test-data"]').click();
    await page.locator('#turn-off-test-data[open]').waitFor();
    assert.match(await page.locator('#turn-off-test-data').textContent(), /baselines are deleted/);
    await screenshot(page, 'turn-off-dialog');
    await page.locator('#turn-off-test-data button[type=submit]').click();
    await toast(page, 'Test data is off and its baselines are deleted');
    assert.equal(await status(page), 'Off');
    assert.equal(await page.locator('.test-data-table').count(), 0);
    assert.deepEqual(errors, []);
  } finally { await context.close(); }
});
