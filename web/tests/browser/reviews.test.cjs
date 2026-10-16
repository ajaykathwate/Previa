const {test, before, after} = require('node:test');
const assert = require('node:assert/strict');
const {chromium} = require('playwright');
const {mkdirSync} = require('node:fs');
const path = require('node:path');

const origin = process.env.REVIEW_BROWSER_URL;
const reviewPath = process.env.REVIEW_PATH;
let browser;
before(async () => { browser = await chromium.launch({headless: true, ...(process.env.UI_BROWSER_EXECUTABLE ? {executablePath: process.env.UI_BROWSER_EXECUTABLE} : {})}); });
after(async () => { await browser?.close(); });

async function session(login, options = {}) {
  const context = await browser.newContext({viewport: {width: 1440, height: 1000}, ...options});
  const page = await context.newPage();
  page.setDefaultTimeout(10000);
  const errors = [];
  page.on('pageerror', e => errors.push(e.message));
  page.on('console', m => { if (/Content Security Policy|Refused to/.test(m.text())) errors.push(`${m.text()} at ${JSON.stringify(m.location())} on ${page.url()}`); });
  await context.request.get(`${origin}/__fixture/identity?login=${login}`);
  await page.goto(`${origin}/auth/github/login`);
  return {context, page, errors};
}

// The app scrolls inside its content panel, so a screenshot grows the
// viewport to the panel's height to show the whole page.
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
  await page.screenshot({animations: 'disabled', path: path.join(__dirname, 'artifacts', `review-${name}.png`)});
  if (height > size.height) await page.setViewportSize(size);
}

async function themes(page, name) {
  for (const theme of ['light', 'dark']) {
    await page.evaluate(t => { document.documentElement.dataset.theme = t; }, theme);
    await screenshot(page, `${name}-${theme}`);
  }
  await page.evaluate(() => { delete document.documentElement.dataset.theme; });
}

async function phone(page, name) {
  const size = page.viewportSize();
  await page.setViewportSize({width: 390, height: 844});
  assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `${name}: no sideways scrolling on a phone`);
  await screenshot(page, `${name}-mobile`);
  await page.setViewportSize(size);
}

const fixture = (page, name) => page.request.post(`${origin}/__fixture/${name}`).then(r => assert.equal(r.status(), 204, `fixture ${name}`));
const toast = (page, text) => page.locator('.toast', {hasText: text}).waitFor();
const live = page => page.locator('html[data-live-state="live"]').waitFor();

test('a developer writes the testing instructions in place', async () => {
  const {context, page, errors} = await session('dev');
  try {
    await page.goto(`${origin}${reviewPath}`);
    assert.equal(await page.locator('.pr-tabs a[aria-current="page"]').textContent().then(t => t.trim()), 'Review');
    assert.equal(await page.evaluate(() => window.injected), undefined, 'the GitHub description stays text');
    assert.match(await page.locator('.review-description').textContent(), /<script>window.injected=true<\/script>/);
    await screenshot(page, 'empty');
    const before = page.url();
    await page.locator('#instructions [data-edit-open]', {hasText: 'Write instructions'}).click();
    assert.equal(page.url(), before, 'editing opens in place, without navigating');
    assert.equal(await page.locator('#instructions-form').isVisible(), true);
    await page.locator('#instructions-summary').fill('Checkout validates the delivery address');
    await page.locator('#instructions-steps').fill('Open the checkout\nLeave the address empty\nChoose Pay');
    await page.locator('#instructions-expected').fill('An inline error asks for the address; nothing is charged.');
    await page.locator('#instructions-start').fill('/checkout');
    await screenshot(page, 'instructions-editing');
    await page.locator('#instructions-form button[type=submit]').click();
    await toast(page, 'Testing instructions saved');
    assert.deepEqual(await page.locator('.review-steps li').allTextContents(), ['Open the checkout', 'Leave the address empty', 'Choose Pay']);
    assert.match(await page.locator('.review-entry a').getAttribute('href'), /\/open\?path=%2Fcheckout$/);
    // Esc closes an edit without saving.
    await page.locator('#instructions .field-card-head [data-edit-open]').click();
    await page.locator('#instructions-summary').fill('Not saved');
    await page.keyboard.press('Escape');
    assert.equal(await page.locator('#instructions-form').isVisible(), false);
    assert.match(await page.locator('.review-summary').textContent(), /delivery address/);
    await themes(page, 'instructions');
    assert.deepEqual(errors, []);
  } finally { await context.close(); }
});

test('a reviewer records, discusses and resolves a finding inside Previa', async () => {
  const {context, page, errors} = await session('reviewer');
  try {
    await page.goto(`${origin}${reviewPath}`);
    assert.equal(await page.locator('.pr-tabs').count(), 0, 'Reviewers only have the review');
    assert.equal(await page.locator('#instructions [data-edit-open]').count(), 0, 'and cannot rewrite the instructions');
    assert.match(await page.locator('.review-recorded').textContent(), /Recorded against\s*Deployment 1/);
    await page.locator('#finding-body').fill('An empty address crashes the checkout.\nExpected an inline error.');
    await page.locator('#finding-path').fill('/checkout');
    await page.locator('.review-repro-toggle summary').click();
    await page.locator('#finding-details').fill('POST /api/orders {"address": ""}\n→ 500 Internal Server Error');
    await page.locator('#new-finding button[type=submit]').click();
    await toast(page, 'Finding added');
    const finding = page.locator('#finding-1');
    assert.match(await finding.locator('.review-thread-head').textContent(), /Deployment 1/);
    assert.match(await finding.locator('.review-repro').textContent(), /500 Internal Server Error/);
    // Anywhere on a finding's head opens the finding's own page.
    const head = finding.locator('.review-thread-head');
    const box = await head.boundingBox();
    await head.click({position: {x: box.width * 0.6, y: box.height / 2}});
    await page.waitForURL(/[?&]finding=1/);
    assert.match(await page.locator('#findings-title').textContent(), /Finding #1/);
    assert.equal(await page.locator('#new-finding').count(), 0, 'a finding\'s own page shows just that finding');
    await page.goto(`${origin}${reviewPath}`);

    // Reply with Ctrl+Enter, then correct and take it back.
    await page.locator('#reply-1').fill('Also on mobile Safari.');
    await page.locator('#reply-1').press('Control+Enter');
    await toast(page, 'Reply added');
    const reply = page.locator('#finding-1-2');
    await reply.locator('.review-menu summary').click();
    await reply.locator('[data-edit-open]').click();
    const edit = page.locator('[data-edit-form="1.2"]');
    await edit.locator('textarea').fill('Also on mobile Safari 17.');
    await edit.locator('button[type=submit]').click();
    await toast(page, 'Message updated');
    assert.match(await page.locator('#finding-1-2').textContent(), /Safari 17\.\s*/);
    assert.match(await page.locator('#finding-1-2 .review-message-meta').textContent(), /Edited/);
    await page.locator('#finding-1-2 .review-menu summary').click();
    await page.locator('#finding-1-2 [data-remove-open]').click();
    const dialog = page.locator('#review-remove');
    await dialog.waitFor();
    assert.match(await dialog.locator('h2').textContent(), /Remove your message\?/);
    assert.equal(await dialog.locator('[data-remove-reasons]').isVisible(), false, 'removing your own words needs no reason');
    await screenshot(page, 'remove-dialog');
    await dialog.locator('button[type=submit]').click();
    await toast(page, 'Message removed');
    assert.match(await page.locator('#finding-1-2').textContent(), /Message removed by its author/);

    // Resolve, find it under Resolved, reopen.
    await page.locator('#finding-1 .review-state-button').click();
    await toast(page, 'Finding resolved');
    assert.match(await page.locator('.review-states a', {hasText: 'Open'}).textContent(), /0/);
    await page.locator('.review-states a', {hasText: 'Resolved'}).click();
    assert.match(await page.locator('#finding-1 .review-resolution').textContent(), /Resolved by Ravi Reviewer on Deployment 1/);
    await themes(page, 'resolved');
    await page.locator('#finding-1 .review-state-button').click();
    await toast(page, 'Finding reopened');
    await page.goto(`${origin}${reviewPath}`);
    await themes(page, 'conversation');
    await phone(page, 'conversation');
    assert.deepEqual(errors, []);
  } finally { await context.close(); }
});

test('live activity keeps a draft, and a newer deployment is chosen on purpose', async () => {
  const {context, page, errors} = await session('reviewer');
  try {
    await page.goto(`${origin}${reviewPath}`);
    await live(page);
    await page.locator('#finding-body').fill('The total ignores the discount code.');
    // A teammate's reply arrives while typing: the page waits and says so.
    await fixture(page, 'teammate-reply');
    await page.locator('.review-live-note').waitFor();
    assert.equal(await page.locator('#finding-1', {hasText: 'Thanks, looking at it now.'}).count(), 0);
    await page.locator('.review-findings-head h2').click();
    await page.locator('#finding-1', {hasText: 'Thanks, looking at it now.'}).waitFor();
    assert.equal(await page.locator('#finding-body').inputValue(), 'The total ignores the discount code.', 'the draft survives the refresh');
    assert.equal(await page.locator('.review-live-note').count(), 0);

    // Deployment 2 goes live while the draft is unsent.
    await live(page);
    await fixture(page, 'publish');
    await toast(page, 'Deployment 2 is now the newest');
    assert.equal(await page.locator('#finding-body').inputValue(), 'The total ignores the discount code.');
    assert.match(await page.locator('.review-preview').textContent(), /Deployment 2/);
    await page.locator('#new-finding button[type=submit]').click();
    await page.locator('.review-conflict', {hasText: 'A newer deployment was published while you wrote this.'}).waitFor();
    assert.equal(await page.locator('#finding-body').inputValue(), 'The total ignores the discount code.');
    await screenshot(page, 'version-conflict');
    await page.locator('.review-conflict button', {hasText: 'Keep Deployment 1'}).click();
    await toast(page, 'Finding added');
    const kept = page.locator('#finding-2');
    assert.match(await kept.locator('.review-thread-head').textContent(), /Deployment 1/);
    assert.match(await kept.locator('.review-thread-head').textContent(), /Earlier deployment/);
    assert.deepEqual(errors, []);
  } finally { await context.close(); }
});

test('a developer moderates with a reason, from the pull request page', async () => {
  const {context, page, errors} = await session('dev');
  try {
    await page.goto(`${origin}${reviewPath.replace(/\/review$/, '')}`);
    const tab = page.locator('.pr-tabs a[href$="/review"]');
    assert.match(await tab.locator('.tab-count').textContent(), /2/, 'open findings beside the Review tab');
    await tab.click();
    await page.locator('#finding-1').waitFor();
    await page.locator('#finding-1-1 .review-menu summary').click();
    await page.locator('#finding-1-1 [data-remove-open]', {hasText: 'Remove as moderator'}).click();
    const dialog = page.locator('#review-remove');
    await dialog.waitFor();
    assert.equal(await dialog.locator('[data-remove-reasons]').isVisible(), true);
    await dialog.locator('button[type=submit]').click();
    assert.equal(await dialog.evaluate(d => d.open), true, 'a reason is required before removing');
    await dialog.locator('label', {hasText: 'Spam or off-topic'}).click();
    await screenshot(page, 'moderation-dialog');
    await dialog.locator('button[type=submit]').click();
    await toast(page, 'Message removed');
    assert.match(await page.locator('#finding-1-1').textContent(), /Message removed by Maya Developer · Spam or off-topic/);
    await phone(page, 'moderated');
    assert.deepEqual(errors, []);
  } finally { await context.close(); }
});

test('without JavaScript every review action still works', async () => {
  // Reduced motion: the site scrolls smoothly to a finding after each form,
  // which never lets the test robot's own scrolling settle without scripts.
  const {context, page, errors} = await session('reviewer', {javaScriptEnabled: false, reducedMotion: 'reduce'});
  try {
    await page.goto(`${origin}${reviewPath}`);
    await page.locator('#finding-body').fill('No-script finding');
    await page.locator('#new-finding button[type=submit]').click();
    await page.locator('#finding-3').waitFor();
    await page.locator('#reply-3').fill('No-script reply');
    await page.locator('#finding-3 .review-reply button[type=submit]').click();
    await page.locator('#finding-3-2').waitFor();
    await page.locator('#finding-3-2 .review-menu summary').click();
    await page.locator('#finding-3-2 [data-edit-open]').click();
    await page.locator('[data-edit-form="3.2"] textarea').fill('No-script reply, edited');
    await page.locator('[data-edit-form="3.2"] button[type=submit]').click();
    assert.match(await page.locator('#finding-3-2').textContent(), /edited/);
    await page.locator('#finding-3-2 .review-menu summary').click();
    await page.locator('#finding-3-2 [data-remove-open]').click();
    const confirm = page.locator('.review-confirm');
    assert.match(await confirm.textContent(), /Remove your message\?/);
    await confirm.locator('button[type=submit]').click();
    assert.match(await page.locator('#finding-3-2').textContent(), /Message removed by its author/);
    await page.locator('#finding-3 .review-state-button').click();
    assert.equal(await page.locator('#finding-3').count(), 0, 'resolved findings leave the open list');
    assert.deepEqual(errors, []);
  } finally { await context.close(); }
});
