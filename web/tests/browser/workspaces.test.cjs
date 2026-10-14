const {test, before, after} = require('node:test');
const assert = require('node:assert/strict');
const {chromium} = require('playwright');
const {mkdirSync} = require('node:fs');
const path = require('node:path');
const origin = process.env.WORKSPACE_BROWSER_URL;
const team = process.env.WORKSPACE_TEST_ID;
const dev = process.env.WORKSPACE_DEV_ID;
let browser;
before(async () => { browser = await chromium.launch({headless: true, ...(process.env.UI_BROWSER_EXECUTABLE ? {executablePath:process.env.UI_BROWSER_EXECUTABLE} : {})}); });
after(async () => { await browser?.close(); });
async function session(login='owner', options={}) {
 const context = await browser.newContext({viewport:{width:1440,height:1000}, ...options});
 const page = await context.newPage();
 page.setDefaultTimeout(10000);
 const errors = [];
 page.on('pageerror', e => errors.push(e.message));
 page.on('console', m => { if (/Content Security Policy|Refused to/.test(m.text())) errors.push(m.text()); });
 await context.request.get(`${origin}/__fixture/identity?login=${login}`);
 await page.goto(`${origin}/auth/github/login`);
 await page.goto(`${origin}/workspaces/${team}/members`);
 return {context,page,errors};
}
async function screenshot(page,name) {
 if (process.env.UI_BROWSER_SCREENSHOTS !== '1') return;
 mkdirSync(path.join(__dirname,'artifacts'),{recursive:true});
 await page.screenshot({path:path.join(__dirname,'artifacts',`workspace-${name}.png`),fullPage:true});
}
test('workspace members and access confirmations work in both themes and on mobile',async () => {
 const {context,page,errors}=await session();
 try {
  await page.locator('.team-member-row').filter({has:page.locator('strong', {hasText:/^dev$/})}).getByText('Manage access',{exact:true}).click();
  await page.locator(`#role-${dev}`).selectOption('reviewer');
  await page.getByRole('button',{name:'Save access',exact:true}).click();
  const dialog=page.locator(`#save-member-${dev}`);
  await dialog.waitFor({state:'visible'});
  assert.equal(await dialog.evaluate(d=>d.matches(':modal')),true);
  await dialog.getByRole('button',{name:'Update access',exact:true}).click();
  await page.getByRole('status').filter({hasText:'Workspace updated.'}).waitFor();
  assert.equal(await page.locator('.team-member-row').filter({has:page.locator('strong',{hasText:/^dev$/})}).locator('.badge').textContent(),'reviewer');
  for (const theme of ['light','dark']) {
   await page.evaluate(t=>document.documentElement.dataset.theme=t,theme);
   await screenshot(page,`members-${theme}`);
   assert.ok(await page.locator('.primary-action').first().evaluate(e=>getComputedStyle(e).backgroundColor!=='rgba(0, 0, 0, 0)'));
  }
  await page.setViewportSize({width:390,height:844});
  await screenshot(page,'members-mobile');
  assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
  assert.deepEqual(errors,[]);
 } finally { await context.close(); }
});
test('workspace creation and switching preserve separate teams',async () => {
 const {context,page,errors}=await session();
 try {
  await page.goto(`${origin}/workspaces`);
  await page.getByRole('button',{name:'Create workspace',exact:true}).first().click();
  await page.locator('#workspace-name').fill('Browser second team');
  await page.locator('#create-workspace').getByRole('button',{name:'Create workspace',exact:true}).click();
  await page.getByRole('heading',{name:'Browser second team',exact:true}).waitFor();
  assert.equal(await page.locator('.team-member-row').count(),1);
  await page.locator('.app-sidebar .workspace-switcher summary').click();
  await page.locator(`.app-sidebar form[action='/workspaces/${team}/select'] button`).click();
  await page.getByRole('heading',{name:"owner's workspace",exact:true}).waitFor();
  assert.equal(await page.locator('.team-member-row').count(),4);
  assert.deepEqual(errors,[]);
 } finally { await context.close(); }
});
test('workspace invitation consent and reviewer scope work without JavaScript',async () => {
 const owner=await session();let reviewer;
 try {
  await owner.page.getByRole('button',{name:'Invite teammate',exact:true}).click();
  await owner.page.locator('#invite-login').fill('newbie');
  await owner.page.locator('#invite-role').selectOption('reviewer');
  await owner.page.locator('#invite-member input[name=repository]').first().check();
  await owner.page.locator('#invite-member').getByRole('button',{name:'Create invitation',exact:true}).click();
  const input=owner.page.locator('#invitation-link');await input.waitFor();
  const invite=await input.inputValue();
  const ctx=await browser.newContext({javaScriptEnabled:false});
  reviewer={context:ctx,page:await ctx.newPage()};
  await ctx.request.get(`${origin}/__fixture/identity?login=newbie`);
  await reviewer.page.goto(`${origin}/auth/github/login`);
  await reviewer.page.goto(invite.replace('http://localhost',origin));
  assert.ok((await reviewer.page.locator('main').textContent()).includes('acme/api'));
  assert.ok(!(await reviewer.page.locator('main').textContent()).includes('acme/private'));
  await reviewer.page.getByRole('button',{name:'Join workspace',exact:true}).click();
  await reviewer.page.getByRole('link',{name:'Previews',exact:true}).click();
  assert.ok((await reviewer.page.locator('main').textContent()).includes('acme/api'));
  assert.equal(await reviewer.page.locator('.sidebar-nav a[href="/builds"]').count(),0);
  assert.equal(await reviewer.page.locator('.sidebar-nav a[href="/repositories"]').count(),0);
  assert.deepEqual(owner.errors,[]);
 } finally { await reviewer?.context.close();await owner.context.close(); }
});
test('workspace ownership handover requires the nominated admin to accept',async () => {
 const owner=await session();let admin;
 try {
  await owner.page.goto(`${origin}/workspaces/${team}/settings`);
  await owner.page.locator('#ownership-target').selectOption({label:'admin'});
  await owner.page.getByRole('button',{name:'Request handover',exact:true}).click();
  await owner.page.locator('#nominate-owner').getByRole('button',{name:'Request transfer',exact:true}).click();
  await owner.page.getByRole('heading',{name:'Ownership transfer pending',exact:true}).waitFor();
  admin=await session('admin');
  await admin.page.goto(`${origin}/workspaces/${team}/settings`);
  await admin.page.getByRole('button',{name:'Accept ownership',exact:true}).first().click();
  await admin.page.locator('#accept-owner').getByRole('button',{name:'Accept ownership',exact:true}).click();
  await admin.page.getByRole('heading',{name:'Transfer ownership',exact:true}).waitFor();
  await owner.page.reload();
  assert.equal(await owner.page.getByRole('heading',{name:'Transfer ownership',exact:true}).count(),0);
  assert.ok((await owner.page.locator('.team-heading').textContent()).includes('admin'));
  assert.deepEqual(owner.errors,[]);assert.deepEqual(admin.errors,[]);
 } finally { await admin?.context.close();await owner.context.close(); }
});
