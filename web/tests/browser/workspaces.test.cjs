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
 await page.waitForTimeout(350); // Allow cross-document view transitions to start and settle before capture.
 await page.waitForFunction(()=>document.getAnimations().every(a=>a.playState!=='running'||a.effect?.getTiming().iterations===Infinity));
 await page.screenshot({animations:'disabled',path:path.join(__dirname,'artifacts',`workspace-${name}.png`),fullPage:true});
}
test('workspace members and access confirmations work in both themes and on mobile',async () => {
 const {context,page,errors}=await session();
 try {
  await page.locator('.team-member-row').filter({has:page.locator('strong', {hasText:/^dev$/})}).getByText('Manage access',{exact:true}).click();
  await page.locator(`#role-${dev}`).selectOption('reviewer');
  const dialog=page.locator(`#access-member-${dev}`);
  await dialog.waitFor({state:'visible'});
  assert.equal(await dialog.evaluate(d=>d.matches(':modal')),true);
  await screenshot(page,'access-dialog');
  await dialog.getByRole('button',{name:'Save access',exact:true}).click();
  await page.getByRole('status').filter({hasText:'Workspace updated.'}).waitFor();
  assert.equal(await page.locator('.team-member-row').filter({has:page.locator('strong',{hasText:/^dev$/})}).locator('[data-workspace-role]').getAttribute('data-workspace-role'),'reviewer');
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
  await page.getByRole('heading',{name:'Members',exact:true}).waitFor();
  assert.ok((await page.locator('.team-context').textContent()).includes('Browser second team'));
  assert.equal(await page.locator('.team-member-row').count(),1);
  await page.locator('.app-sidebar .workspace-switcher summary').click();
  await page.locator(`.app-sidebar form[action='/workspaces/${team}/select'] button`).click();
  await page.getByRole('heading',{name:'Members',exact:true}).waitFor();
  assert.ok((await page.locator('.team-context').textContent()).includes("owner's workspace"));
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
  assert.ok(!(await reviewer.page.locator('main').textContent()).includes('acme/private'));
  assert.equal(await reviewer.page.locator('main a[href^="/repositories/"]').count(),0);
  await screenshot(reviewer.page,'reviewer-previews');
  assert.equal(await reviewer.page.locator('.sidebar-nav a[href="/builds"]').count(),0);
  assert.equal(await reviewer.page.locator('.sidebar-nav a[href="/repositories"]').count(),0);
  assert.deepEqual(owner.errors,[]);
 } finally { await reviewer?.context.close();await owner.context.close(); }
});

test('direct navigation, search, role capsules and aligned switcher remain usable',async()=>{
 const {context,page,errors}=await session();
 try {
  const nav=page.locator('.app-sidebar .sidebar-nav');
  assert.equal(await nav.getByRole('link',{name:'Members',exact:true}).getAttribute('aria-current'),'page');
  assert.equal(await page.locator('main a').filter({hasText:/^Previews$/}).count(),0);
  await nav.getByRole('link',{name:'Workspace settings',exact:true}).click();
  await page.getByRole('heading',{name:'Workspace settings',exact:true}).waitFor();
  assert.equal(await page.locator('main .team-member-row').count(),0);
  assert.equal(await page.locator('main').getByRole('link',{name:'Previews',exact:true}).count(),0);
  await screenshot(page,'settings-light');
  await page.evaluate(()=>document.documentElement.dataset.theme='dark');await screenshot(page,'settings-dark');
  await nav.getByRole('link',{name:'Members',exact:true}).click();
  await page.getByLabel('Search by GitHub username').fill('REVIEWER');await page.getByRole('button',{name:'Search people'}).click();
  assert.equal(await page.locator('.team-member-row').count(),1);
  await page.getByLabel('Search by GitHub username').fill('does-not-exist');await page.getByRole('button',{name:'Search people'}).click();
  await page.getByRole('heading',{name:'No matching people'}).waitFor();await screenshot(page,'members-empty');
  const summary=page.locator('.app-sidebar .workspace-switcher summary');
  const alignment=await summary.evaluate(e=>{const a=e.querySelector('.workspace-avatar').getBoundingClientRect(),b=e.querySelector('.workspace-switcher-chevron').getBoundingClientRect();return Math.abs(a.y+a.height/2-b.y-b.height/2)});
  assert.ok(alignment<=1,`chevron misaligned by ${alignment}px`);
  await summary.click();
  const menu=await page.locator('.app-sidebar .workspace-switcher-menu').boundingBox();assert.ok(menu.x>=0&&menu.x+menu.width<=1440);
  await screenshot(page,'switcher');
  assert.ok(await page.locator('.app-sidebar .workspace-switcher-menu').evaluate(e=>{const r=e.getBoundingClientRect();return e.contains(document.elementFromPoint(r.right-15,r.top+25))}), 'switcher must appear above page content');
  await summary.click();
  for(const width of [1024,820,390]){
   await page.setViewportSize({width,height:900});await page.goto(`${origin}/workspaces/${team}/settings`);
   assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),`overflow at ${width}`);
   await screenshot(page,`settings-${width}`);
  }
  assert.deepEqual(errors,[]);
 }finally{await context.close()}
});
test('profile details and logos persist, reject stale edits, and support no-JS forms',async()=>{
 const {context,page,errors}=await session();let second,noJS;
 try{
  await page.goto(`${origin}/workspaces/${team}/settings`);
  const save=page.getByRole('button',{name:'Save changes',exact:true});assert.ok(await save.isDisabled());
  second=await context.newPage();await second.goto(page.url());
  const name='Orion engineering workspace for a very long but valid name';
  await page.getByLabel('Workspace name',{exact:true}).fill(name);
  await page.getByLabel('Description Optional').fill('Preview environments for the Orion team.');
  assert.ok(await save.isEnabled());await save.click();
  await page.getByRole('status').filter({hasText:'Workspace details saved.'}).waitFor();
  assert.equal(await page.getByLabel('Workspace name',{exact:true}).inputValue(),name);
  assert.equal(await page.locator('.app-sidebar .workspace-switcher summary').getAttribute('title'),name);
  await screenshot(page,'long-name');
  await second.getByLabel('Workspace name',{exact:true}).fill('Stale name');
  const stale=second.waitForResponse(r=>r.request().method()==='POST');await second.getByRole('button',{name:'Save changes',exact:true}).click();
  assert.equal((await stale).status(),409);await second.getByRole('alert').filter({hasText:'another window'}).waitFor();
  const png=Buffer.from(process.env.WORKSPACE_LOGO_PNG,'base64');
  await page.locator('#workspace-logo').setInputFiles({name:'team.png',mimeType:'image/png',buffer:png});
  await page.getByRole('button',{name:'Upload logo',exact:true}).click();
  await page.getByRole('status').filter({hasText:'Workspace logo updated.'}).waitFor();
  await page.locator('.team-logo-preview img').evaluate(img=>img.decode());
  await page.reload();assert.equal(await page.locator('.app-sidebar .workspace-switcher summary .workspace-avatar img').count(),1);
  await screenshot(page,'logo');
  await page.getByRole('button',{name:'Remove logo and use initial'}).click();
  await page.getByRole('status').filter({hasText:'Workspace logo removed.'}).waitFor();
  assert.equal(await page.locator('.team-logo-preview img').count(),0);
  noJS=await browser.newContext({javaScriptEnabled:false});const plain=await noJS.newPage();
  await noJS.request.get(`${origin}/__fixture/identity?login=owner`);await plain.goto(`${origin}/auth/github/login`);await plain.goto(`${origin}/workspaces/${team}/settings`);
  await plain.getByLabel('Workspace name',{exact:true}).fill("owner's workspace");
  await plain.getByLabel('Description Optional').fill('');await plain.getByRole('button',{name:'Save changes',exact:true}).click();
  await plain.getByRole('status').filter({hasText:'Workspace details saved.'}).waitFor();
  assert.deepEqual(errors,[]);
 }finally{await noJS?.close();await second?.close();await context.close()}
});
test('invitations have their own list and explain role-specific repository access',async()=>{
 const {context,page,errors}=await session();
 try{
  await page.getByRole('link',{name:/^Invitations /}).click();
  await page.getByRole('heading',{name:'No pending invitations'}).waitFor();
  await page.getByRole('button',{name:'Invite teammate',exact:true}).first().click();
  const dialog=page.locator('#invite-member');await dialog.locator('#invite-role').selectOption('admin');
  assert.ok(await dialog.locator('fieldset').isHidden());
  await dialog.locator('#invite-role').selectOption('developer');assert.ok(await dialog.locator('fieldset').isVisible());
  assert.equal(await dialog.locator('input[name=repository]').count(),2);
  await dialog.getByRole('button',{name:'Cancel',exact:true}).click();
  await screenshot(page,'invitations-empty');assert.deepEqual(errors,[]);
 }finally{await context.close()}
});
test('workspace ownership handover requires the nominated admin to accept',async () => {
 const owner=await session();let admin;
 try {
  await owner.page.goto(`${origin}/workspaces/${team}/settings`);
  await owner.page.getByRole('button',{name:'Request handover',exact:true}).click();
  await owner.page.locator('#ownership-target').selectOption({label:'admin'});
  await owner.page.locator('#nominate-owner').getByRole('button',{name:'Request transfer',exact:true}).click();
  await owner.page.getByRole('heading',{name:'Ownership transfer pending',exact:true}).waitFor();
  admin=await session('admin');
  await admin.page.goto(`${origin}/workspaces/${team}/settings`);
  await admin.page.getByRole('button',{name:'Accept ownership',exact:true}).first().click();
  await admin.page.locator('#accept-owner').getByRole('button',{name:'Accept ownership',exact:true}).click();
  await admin.page.getByRole('heading',{name:'Transfer ownership',exact:true}).waitFor();
  await owner.page.reload();
  assert.equal(await owner.page.getByRole('heading',{name:'Transfer ownership',exact:true}).count(),0);
  assert.equal(await owner.page.locator('.team-context [data-workspace-role]').getAttribute('data-workspace-role'),'admin');
  assert.deepEqual(owner.errors,[]);assert.deepEqual(admin.errors,[]);
 } finally { await admin?.context.close();await owner.context.close(); }
});
