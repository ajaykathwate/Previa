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
});

test('responsive dashboard sticky navigation and honest placeholders', async () => {
  await login();
  await page.goto(`${origin}/repositories`);
  for (const width of [320, 390, 768, 1024, 1440]) {
    await page.setViewportSize({width, height: 844});
    await assertNoOverflow();
    await page.evaluate(() => scrollTo({top: 500, behavior: 'instant'}));
    assert.equal(await page.evaluate(() => scrollY), 0);
    const list = page.locator('#repository-results .repository-table-scroll');
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
  await page.locator('#repository-results .repository-table-scroll').evaluate(e => {e.scrollTop = 300;});
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

test('PR workspace live search pagination modal and disabled build', async () => {
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
  await page.getByRole('switch',{name:'Automatic',exact:true}).click();
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
  assert(await page.getByRole('button',{name:'Build latest commit'}).isDisabled());
  assert.match(await page.locator('.pr-detail-grid').innerText(),/Not requested/);
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
    await p.getByRole('link',{name:'Back to pull requests'}).click();assert.equal(await p.locator('#pull-request-search').inputValue(),'preview 25');
    await p.getByRole('switch',{name:'Automatic',exact:true}).click();
    assert(await p.locator('#build-policy').evaluate(e=>e.matches(':modal')));
    await p.locator('#build-policy').getByRole('button',{name:'Enable automatic',exact:true}).click();
    assert.equal(await p.locator('#pull-request-search').inputValue(),'preview 25');assert.match(await p.locator('.pr-panel-heading').innerText(),/Automatic/);
  } finally {await noJS.close();}
});



test('Automatic confirms before enabling saves in place and sync polling respects modal focus',async()=>{
 await login();await page.goto(`${origin}/repositories`);
 await page.locator('[data-repo-link="1"]').click();await readyPanel('1');
 await page.evaluate(()=>window.originalDocument=document);
 await page.locator('#pull-request-search').fill('preview 25');await page.waitForFunction(()=>document.querySelectorAll('.pr-table tbody tr').length===1);
 let policyRequests=0;
 page.on('request',request=>{if(request.method()==='POST' && request.url().endsWith('/repositories/1/policy')) policyRequests++;});
 const mode=page.getByRole('switch',{name:'Automatic',exact:true});
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
 assert(await page.getByRole('switch',{name:'Automatic',exact:true}).isDisabled());
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
 assert.match(await page.locator('.pr-summary').innerText(),/Last synced.*4 Oct 2026, 10:00 UTC/s);
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
 assert(await page.getByRole('button',{name:'Build latest commit'}).isDisabled());
 const github=page.getByRole('link',{name:'View on GitHub',exact:true});
 assert.equal(await github.locator('svg').count(),1);assert.equal(await github.getAttribute('target'),'_blank');
 await page.getByRole('link',{name:'Back to pull requests'}).click();await page.waitForURL(url=>url.pathname==='/repositories/1');
});

test('failed PR searches preserve records and query bookmarks redirect to dedicated pages',async()=>{
 await login();await page.goto(`${origin}/repositories?repo_id=1&pr=7&pr_q=preview`);
 await page.waitForURL(url=>url.pathname==='/repositories/1/pull-requests/7');
 assert.match(await page.locator('.pr-detail-grid').innerText(),/Not requested/);
 await page.getByRole('link',{name:'Back to pull requests'}).click();await page.waitForURL(url=>url.pathname==='/repositories/1');
 await page.route('**/repositories/1?*',route=>route.fulfill({status:503,body:'fixture unavailable'}));
 await page.locator('#pull-request-search').fill('preview 25');
 await page.locator('#pull-request-filters [data-repository-feedback][data-error]').waitFor();
 assert.equal(await page.locator('.pr-table tbody tr').count(),10);assert.equal(new URL(page.url()).searchParams.get('pr_q'),'preview');
 await page.unroute('**/repositories/1?*');
 await page.locator('#pull-request-search').press('Enter');await page.waitForFunction(()=>document.querySelectorAll('.pr-table tbody tr').length===1);
 assert.equal(new URL(page.url()).searchParams.get('pr_q'),'preview 25');
});

test('policy and sync on PR detail keep the dedicated URL and selected PR',async()=>{
 await login();await page.goto(`${origin}/repositories/1/pull-requests/7?pr_q=preview&q=fixture&page=2`);
 await page.getByRole('switch',{name:'Automatic'}).click();await page.waitForFunction(()=>document.querySelector('#build-policy').matches(':modal'));
 await page.getByRole('button',{name:'Enable automatic',exact:true}).click();await readyPanel('1');
 assert.equal(new URL(page.url()).pathname,'/repositories/1/pull-requests/7');assert.equal(await page.locator('.automatic-switch').getAttribute('aria-checked'),'true');
 assert.match(await page.locator('.pr-current-crumb').innerText(),/#7/);
 await page.getByRole('button',{name:'Sync Pull Requests'}).click();await page.locator('.sync-state').filter({hasText:'Syncing'}).waitFor();
 await page.waitForFunction(()=>!document.querySelector('.pr-summary').textContent.includes('Syncing'));await readyPanel('1');
 assert.equal(new URL(page.url()).pathname,'/repositories/1/pull-requests/7');assert.equal(new URL(page.url()).searchParams.get('page'),'2');
 assert.match(await page.locator('.pr-current-crumb').innerText(),/#7/);
});
