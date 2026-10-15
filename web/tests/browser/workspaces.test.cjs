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
// Captures transient states (an open in-place editor, a toast) without waiting for animations.
async function snap(page,name) {
 if (process.env.UI_BROWSER_SCREENSHOTS !== '1') return;
 mkdirSync(path.join(__dirname,'artifacts'),{recursive:true});
 await page.screenshot({path:path.join(__dirname,'artifacts',`workspace-${name}.png`)});
}
test('workspace members and access confirmations work in both themes and on mobile',async () => {
 const {context,page,errors}=await session();
 try {
  await page.locator('.team-member-row').filter({has:page.locator('strong', {hasText:/^dev$/})}).getByText('Manage access',{exact:true}).click();
  await page.locator(`#access-member-${dev}`).getByRole('radio',{name:/Reviewer/}).check();
  const dialog=page.locator(`#access-member-${dev}`);
  await dialog.waitFor({state:'visible'});
  assert.equal(await dialog.evaluate(d=>d.matches(':modal')),true);
  await screenshot(page,'access-dialog');
  await dialog.getByRole('button',{name:'Save access',exact:true}).click();
  await page.locator('.toast').filter({hasText:'Workspace updated'}).waitFor();
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
  await page.goto(`${origin}/workspaces`);await screenshot(page,'directory');
  await page.getByRole('button',{name:'Create workspace',exact:true}).first().click();
  await page.locator('#workspace-name').fill('Browser second team');
  await page.locator('#create-workspace').getByRole('button',{name:'Create workspace',exact:true}).click();
  await page.getByRole('heading',{name:'Members',exact:true}).waitFor();
  assert.ok((await page.locator('.app-sidebar [data-current-workspace-name]').textContent()).includes('Browser second team'));
  assert.equal(await page.locator('.team-member-row').count(),1);
  await page.locator('.app-sidebar .workspace-switcher summary').click();
  await page.locator(`.app-sidebar form[action='/workspaces/${team}/select'] button`).click();
  await page.getByRole('heading',{name:'Members',exact:true}).waitFor();
  assert.ok((await page.locator('.app-sidebar [data-current-workspace-name]').textContent()).includes("owner's workspace"));
  assert.equal(await page.locator('.team-member-row').count(),4);
  assert.deepEqual(errors,[]);
 } finally { await context.close(); }
});
test('workspace invitation consent and reviewer scope work without JavaScript',async () => {
 const owner=await session();let reviewer;
 try {
  await owner.page.getByRole('button',{name:'Invite teammate',exact:true}).click();
  await owner.page.locator('#invite-login').fill('newbie');
  await owner.page.locator('#invite-member').getByRole('radio',{name:/Reviewer/}).check();
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
  await page.getByLabel('Search by name or GitHub username').fill('REVIEWER');await page.getByRole('button',{name:'Search people'}).click();
  assert.equal(await page.locator('.team-member-row').count(),1);
  await page.getByLabel('Search by name or GitHub username').fill('does-not-exist');await page.getByRole('button',{name:'Search people'}).click();
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
test('workspace details save in place with a toast, reject stale edits, and support no-JS forms',async()=>{
 const {context,page,errors}=await session();let second,noJS;
 try{
  await page.goto(`${origin}/workspaces/${team}/settings`);
  second=await context.newPage();await second.goto(page.url());
  const name='Orion engineering workspace for a very long but valid name';
  const field=page.getByLabel('Name',{exact:true});
  await field.click();assert.ok(await page.getByRole('button',{name:'Save workspace name',exact:true}).isVisible());
  await field.fill(name);await snap(page,'inline-edit');
  const saved=page.waitForResponse(r=>r.request().method()==='POST');await field.press('Enter');
  assert.equal((await saved).status(),200);
  await page.locator('.toast').filter({hasText:'Workspace details saved'}).waitFor();await snap(page,'toast');
  assert.ok(!page.url().includes('saved='),'in-place saves stay on the page');
  assert.equal((await page.locator('.app-sidebar [data-current-workspace-name]').textContent()).trim(),name);
  const description=page.getByLabel('Description',{exact:true});
  await description.click();await description.fill('Preview environments for the Orion team.');
  const blurred=page.waitForResponse(r=>r.request().method()==='POST');await page.locator('h1').click();
  assert.equal((await blurred).status(),200);
  await field.click();await field.fill('Discard me');await field.press('Escape');
  assert.equal(await field.inputValue(),name);
  await page.reload();
  assert.equal(await page.getByLabel('Name',{exact:true}).inputValue(),name);
  assert.equal(await page.getByLabel('Description',{exact:true}).inputValue(),'Preview environments for the Orion team.');
  assert.equal(await page.locator('.app-sidebar .workspace-switcher summary').getAttribute('title'),name);
  await screenshot(page,'long-name');
  const staleField=second.getByLabel('Name',{exact:true});await staleField.click();await staleField.fill('Stale name');
  const stale=second.waitForResponse(r=>r.request().method()==='POST');await staleField.press('Enter');
  assert.equal((await stale).status(),409);await second.locator('#profile-error').filter({hasText:'another window'}).waitFor();
  assert.equal(await staleField.getAttribute('aria-invalid'),'true');
  const png=Buffer.from(process.env.WORKSPACE_LOGO_PNG,'base64');
  await page.locator('#workspace-logo').setInputFiles({name:'team.png',mimeType:'image/png',buffer:png});
  await page.locator('.toast').filter({hasText:'Workspace logo updated'}).waitFor();
  await page.locator('[data-logo-preview] img').evaluate(img=>img.decode());
  assert.equal(await page.locator('.app-sidebar .workspace-switcher summary .workspace-avatar img').count(),1);
  await page.reload();assert.equal(await page.locator('.app-sidebar .workspace-switcher summary .workspace-avatar img').count(),1);
  await screenshot(page,'logo');
  await page.getByRole('button',{name:'Remove',exact:true}).click();
  await page.locator('.toast').filter({hasText:'Workspace logo removed'}).waitFor();
  assert.equal(await page.locator('[data-logo-preview] img').count(),0);
  assert.ok(await page.getByRole('button',{name:'Remove',exact:true}).isHidden());
  noJS=await browser.newContext({javaScriptEnabled:false});const plain=await noJS.newPage();
  await noJS.request.get(`${origin}/__fixture/identity?login=owner`);await plain.goto(`${origin}/auth/github/login`);await plain.goto(`${origin}/workspaces/${team}/settings`);
  await plain.getByLabel('Description',{exact:true}).fill('');
  await plain.getByLabel('Name',{exact:true}).fill("owner's workspace");await plain.getByLabel('Name',{exact:true}).press('Enter');
  await plain.getByRole('status').filter({hasText:'Workspace details saved'}).waitFor();
  assert.equal(await plain.getByLabel('Name',{exact:true}).inputValue(),"owner's workspace");
  await plain.locator('#workspace-logo').setInputFiles({name:'team.png',mimeType:'image/png',buffer:png});
  await plain.getByRole('button',{name:'Upload',exact:true}).click();
  await plain.getByRole('status').filter({hasText:'Workspace logo updated'}).waitFor();
  assert.deepEqual(errors,[]);
 }finally{await noJS?.close();await second?.close();await context.close()}
});
test('invitations have their own list and explain role-specific repository access',async()=>{
 const {context,page,errors}=await session();
 try{
  await page.getByRole('link',{name:/^Invitations /}).click();
  await page.getByRole('heading',{name:'No pending invitations'}).waitFor();
  await page.getByRole('button',{name:'Invite teammate',exact:true}).first().click();
  const dialog=page.locator('#invite-member');await dialog.getByRole('radio',{name:/Admin/}).check();
  assert.ok(await dialog.locator('[data-repository-picker]').isHidden());
  await dialog.getByRole('radio',{name:/Developer/}).check();assert.ok(await dialog.locator('[data-repository-picker]').isVisible());
  assert.equal(await dialog.locator('input[name=repository]').count(),Number(process.env.WORKSPACE_REPO_COUNT||2));
  await dialog.getByRole('button',{name:'Cancel',exact:true}).click();
  await screenshot(page,'invitations-empty');assert.deepEqual(errors,[]);
 }finally{await context.close()}
});
test('thirty repository grants stay searchable, bounded and compact on desktop and mobile',async()=>{
 const {context,page,errors}=await session();
 try {
  const row=page.locator('.team-member-row').filter({has:page.locator('strong',{hasText:/^dev$/})});
  await row.getByRole('button',{name:'Manage access',exact:true}).click();
  const dialog=page.locator(`#access-member-${dev}`),picker=dialog.locator('[data-repository-picker]');
  assert.equal(await picker.locator('input[name=repository]').count(),30);
  assert.ok(await picker.locator('[data-repository-list]').evaluate(e=>e.scrollHeight>e.clientHeight&&e.clientHeight<=240));
  await picker.getByRole('button',{name:'Select visible',exact:true}).click();
  assert.equal(await picker.locator('[data-repository-count]').textContent(),'30 of 30 selected');
  await picker.getByRole('searchbox',{name:'Search repositories'}).fill('service-28');
  assert.equal(await picker.locator('[data-repository-option]:visible').count(),1);
  await picker.getByRole('button',{name:'Clear visible',exact:true}).click();
  assert.equal(await picker.locator('input:checked').count(),29);
  await picker.getByRole('searchbox',{name:'Search repositories'}).fill('no-matches');
  assert.ok(await picker.locator('[data-repository-empty]').isVisible());
  await picker.getByRole('searchbox',{name:'Search repositories'}).fill('');
  await picker.getByRole('button',{name:'Select visible',exact:true}).click();
  await screenshot(page,'thirty-repositories');
  await dialog.getByRole('button',{name:'Save access',exact:true}).click();
  await page.locator('.toast').filter({hasText:'Workspace updated'}).waitFor();
  assert.equal(await row.locator('.team-repository-chip').count(),2);
  await row.getByRole('button',{name:/View all 30 repositories/}).click();
  assert.equal(await page.locator(`#access-member-${dev} input[name=repository]:checked`).count(),30);
  await page.setViewportSize({width:390,height:844});
  assert.ok(await page.locator(`#access-member-${dev}`).evaluate(e=>e.getBoundingClientRect().width<=innerWidth));
  assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
  await screenshot(page,'thirty-repositories-mobile');
  for(const attr of ['href','action']) {
   const values=await page.locator(`[${attr}]`).evaluateAll((nodes,a)=>nodes.map(n=>n.getAttribute(a)),attr);
   assert.ok(values.every(v=>!/^\/(?:workspaces|repositories)\/\d+(?:\/|\?|$)/.test(v)));
  }
  assert.deepEqual(errors,[]);
 }finally{await context.close()}
});
test('invitation mistakes reopen the same form with a precise field error and retained choices',async()=>{
 const {context,page,errors}=await session();let plain;
 try {
  await page.getByRole('button',{name:'Invite teammate',exact:true}).first().click();
  await page.locator('#invite-member').getByRole('radio',{name:/Reviewer/}).check();
  await page.locator('#invite-member input[name=repository]').nth(5).check();
  for(const [login,message] of [['@wrong','valid GitHub username'],['missing-user','no personal GitHub account'],['dev','already a workspace member'],['provider-down','could not check this account']]) {
   const dialog=page.locator('#invite-member');
   await dialog.locator('#invite-login').fill(login);
   await dialog.getByRole('button',{name:'Create invitation',exact:true}).click();
   await dialog.getByRole('alert').filter({hasText:message}).waitFor();
   assert.ok(await dialog.evaluate(e=>e.matches(':modal')));
   assert.equal(await dialog.locator('#invite-login').inputValue(),login);
   assert.ok(await dialog.getByRole('radio',{name:/Reviewer/}).isChecked());
   assert.equal(await dialog.locator('input[name=repository]:checked').count(),1);
   assert.equal(await page.getByRole('heading',{name:'Action needs attention',exact:true}).count(),0);
  }
  await screenshot(page,'invite-inline-error');
  await page.locator('#invite-login').fill('bulk-invite');
  await page.locator('#invite-member').getByRole('button',{name:'Create invitation',exact:true}).click();
  await page.locator('#invitation-link').waitFor();
  await page.goto(`${origin}/workspaces/${team}/members`);
  await page.getByRole('button',{name:'Invite teammate',exact:true}).first().click();
  await page.locator('#invite-login').fill('bulk-invite');
  await page.locator('#invite-member').getByRole('button',{name:'Create invitation',exact:true}).click();
  await page.locator('#invite-login-error').filter({hasText:'already has a pending invitation'}).waitFor();
  plain=await session('owner',{javaScriptEnabled:false});
  await plain.page.getByRole('button',{name:'Invite teammate',exact:true}).first().click();
  await plain.page.locator('#invite-login').fill('missing-user');
  await plain.page.locator('#invite-member').getByRole('button',{name:'Create invitation',exact:true}).click();
  await plain.page.locator('[data-invite-recovery] #invite-login-error').waitFor();
  assert.equal(await plain.page.locator('[data-invite-recovery] #invite-login').inputValue(),'missing-user');
  assert.deepEqual(errors,[]);
 }finally{await plain?.context.close();await context.close()}
});
test('profile edits names and photo in place and keeps GitHub details in their own section',async()=>{
 const {context,page,errors}=await session();let stale,plain,admin,outsider;
 try {
  await page.goto(`${origin}/protected`);
  await page.getByRole('heading',{name:'Your profile',exact:true}).waitFor();
  const previa=page.locator('section').filter({has:page.getByRole('heading',{name:'Previa profile'})});
  const github=page.locator('section').filter({has:page.getByRole('heading',{name:'GitHub account'})});
  assert.ok((await previa.textContent()).includes('owner@example.com'));
  assert.ok(!(await github.textContent()).includes('owner@example.com'),'email is shown once');
  for (const fact of ['@owner','Remote','Fixture company','github.com/owner']) assert.ok((await github.textContent()).includes(fact),fact);
  assert.equal(await page.getByRole('tab').count(),0);
  const picture=page.locator('.photo-field [data-viewer-avatar] img');
  assert.ok(await picture.evaluate(img=>img.decode().then(()=>img.naturalWidth>0)),'the profile picture loads');
  const box=await previa.boundingBox(),other=await github.boundingBox();
  assert.ok(Math.abs(box.y-other.y)<2&&other.x>box.x+box.width,'sections sit side by side');
  stale=await context.newPage();await stale.goto(`${origin}/protected`);
  const first=page.getByLabel('First name',{exact:true}),last=page.getByLabel('Last name',{exact:true});
  await first.click();await first.fill('Ada');
  let saved=page.waitForResponse(r=>r.request().method()==='POST');await first.press('Enter');assert.equal((await saved).status(),200);
  await page.locator('.toast').filter({hasText:'Name saved'}).waitFor();
  await last.click();await last.fill('Lovelace');
  saved=page.waitForResponse(r=>r.request().method()==='POST');await page.locator('h1').click();assert.equal((await saved).status(),200);
  await page.locator('.toast').filter({hasText:'Name saved'}).waitFor();
  assert.ok((await page.locator('.app-header').textContent()).includes('Ada Lovelace'));
  const staleFirst=stale.getByLabel('First name',{exact:true});await staleFirst.click();await staleFirst.fill('Stale');await staleFirst.press('Enter');
  await stale.locator('#name-error').filter({hasText:'another window'}).waitFor();
  await page.getByRole('link',{name:'Refresh',exact:true}).click();
  await page.getByRole('heading',{name:'Your profile',exact:true}).waitFor();
  assert.equal(await first.inputValue(),'Ada');
  const png=Buffer.from(process.env.WORKSPACE_LOGO_PNG,'base64');
  await page.locator('#avatar-file').setInputFiles({name:'me.png',mimeType:'image/png',buffer:png});
  await page.locator('.toast').filter({hasText:'Profile photo updated'}).waitFor();
  const src=await picture.getAttribute('src');
  assert.match(src,/^\/avatars\/[0-9a-f-]{36}\?v=\d+$/);
  assert.equal(await page.locator('.app-header .account-menu summary [data-viewer-avatar] img').getAttribute('src'),src);
  assert.ok(await picture.evaluate(img=>img.decode().then(()=>img.naturalWidth===64)));
  await screenshot(page,'member-profile');
  await page.evaluate(()=>document.documentElement.dataset.theme='dark');await screenshot(page,'member-profile-dark');
  await page.evaluate(()=>delete document.documentElement.dataset.theme);
  await page.setViewportSize({width:390,height:844});
  assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
  await screenshot(page,'member-profile-mobile');
  await page.setViewportSize({width:1440,height:1000});
  admin=await session('admin');
  const teammate=admin.page.locator('.team-member-row').filter({hasText:'Ada Lovelace'});
  assert.equal(await teammate.locator('.user-avatar img').getAttribute('src'),src);
  assert.equal((await admin.context.request.get(origin+src)).status(),200);
  outsider=await session('outsider');
  assert.equal((await outsider.context.request.get(origin+src)).status(),404);
  await page.getByRole('button',{name:'Use GitHub picture',exact:true}).click();
  await page.locator('.toast').filter({hasText:'Profile photo removed'}).waitFor();
  assert.equal(await picture.getAttribute('src'),'/static/brand/favicon.svg');
  assert.equal((await admin.context.request.get(origin+src)).status(),404);
  await page.goto(`${origin}/workspaces/${team}/members`);
  assert.ok((await page.locator('.team-member-row').filter({hasText:'Ada Lovelace'}).textContent()).includes('@owner'));
  await page.getByPlaceholder('Search name or GitHub username').fill('Ada');
  await page.locator('.team-search').getByRole('button').click();
  assert.equal(await page.locator('.team-member-row').count(),1);
  await page.goto(`${origin}/protected`);
  for (const field of [first,last]) { await field.click();await field.fill('');saved=page.waitForResponse(r=>r.request().method()==='POST');await field.press('Enter');await saved; }
  assert.equal((await page.locator('.app-header .account-name').textContent()).trim(),'owner');
  plain=await session('owner',{javaScriptEnabled:false});
  await plain.page.goto(`${origin}/protected`);
  await plain.page.getByLabel('First name',{exact:true}).fill('Grace');await plain.page.getByLabel('First name',{exact:true}).press('Enter');
  await plain.page.getByRole('status').filter({hasText:'Name saved'}).waitFor();
  await plain.page.locator('#avatar-file').setInputFiles({name:'me.png',mimeType:'image/png',buffer:png});
  await plain.page.getByRole('button',{name:'Upload',exact:true}).click();
  await plain.page.getByRole('status').filter({hasText:'Profile photo updated'}).waitFor();
  assert.deepEqual(errors,[]);assert.deepEqual(admin.errors,[]);
 }finally{await plain?.context.close();await outsider?.context.close();await admin?.context.close();await stale?.close();await context.close()}
});
test('UUID PR detail shows its description and a failed build stops publishing in the UI',async()=>{
 const {context,page,errors}=await session();
 try {
  await page.goto(origin+process.env.WORKSPACE_PR_PATH);
  await page.getByRole('heading',{name:'Pull request description',exact:true}).waitFor();
  assert.ok((await page.locator('.pr-description-content').textContent()).includes('Why this change'));
  assert.equal(await page.evaluate(()=>Boolean(window.descriptionExecuted)),false);
  await screenshot(page,'pr-description');
  const response=await context.request.post(`${origin}/__fixture/failed-build`);assert.equal(response.status(),204);
  await page.reload();
  assert.ok((await page.locator('main').textContent()).includes('Build failed'));
  assert.ok((await page.locator('main').textContent()).includes('retries have ended'));
  await screenshot(page,'pr-failed-build');
  await page.setViewportSize({width:390,height:844});
  assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
  await screenshot(page,'pr-failed-build-mobile');
  assert.deepEqual(errors,[]);
 }finally{await context.close()}
});
test('workspace ownership handover requires the nominated admin to accept',async () => {
 const owner=await session();let admin;
 try {
  await owner.page.goto(`${origin}/workspaces/${team}/settings`);
  await owner.page.getByRole('button',{name:'Request handover',exact:true}).click();
  await owner.page.locator('#ownership-target').selectOption({label:'admin'});
  await owner.page.locator('#nominate-owner').getByRole('button',{name:'Request transfer',exact:true}).click();
  await owner.page.locator('#ownership').filter({hasText:'must accept before'}).waitFor();
  admin=await session('admin');
  await admin.page.goto(`${origin}/workspaces/${team}/settings`);
  await admin.page.getByRole('button',{name:'Accept ownership',exact:true}).first().click();
  await admin.page.locator('#accept-owner').getByRole('button',{name:'Accept ownership',exact:true}).click();
  await admin.page.locator('#ownership').filter({hasText:'You own this workspace'}).waitFor();
  await owner.page.reload();
  assert.ok(!(await owner.page.locator('#ownership').textContent()).includes('You own this workspace'));
  assert.equal(await owner.page.locator('.app-sidebar .workspace-switcher summary [data-workspace-role]').getAttribute('data-workspace-role'),'admin');
  assert.deepEqual(owner.errors,[]);assert.deepEqual(admin.errors,[]);
 } finally { await admin?.context.close();await owner.context.close(); }
});
