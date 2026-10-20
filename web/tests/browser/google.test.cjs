const {test,before,after}=require('node:test');
const assert=require('node:assert/strict');
const {chromium}=require('playwright');
const {mkdirSync}=require('node:fs');
const path=require('node:path');
const origin=process.env.GOOGLE_BROWSER_URL,workspace=process.env.GOOGLE_WORKSPACE;
let browser;
before(async()=>{browser=await chromium.launch({headless:true,...(process.env.UI_BROWSER_EXECUTABLE?{executablePath:process.env.UI_BROWSER_EXECUTABLE}:{})});});
after(async()=>{await browser?.close();});
async function context(js,code){const ctx=await browser.newContext({javaScriptEnabled:js,reducedMotion:'reduce',viewport:{width:1440,height:1000}});if(code)await ctx.addCookies([{name:'fixture_google',value:code,url:origin}]);const page=await ctx.newPage();page.setDefaultTimeout(12000);const errors=[];page.on('pageerror',e=>errors.push(e.message));page.on('console',m=>{if(/Content Security Policy|Refused to/.test(m.text()))errors.push(m.text());});return {ctx,page,errors};}
async function shots(page,name){if(process.env.UI_BROWSER_SCREENSHOTS!=='1')return;const dir=path.join(__dirname,'artifacts');mkdirSync(dir,{recursive:true});for(const width of [1440,390])for(const theme of ['light','dark']){await page.setViewportSize({width,height:1000});await page.evaluate(t=>document.documentElement.dataset.theme=t,theme);assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));await page.screenshot({fullPage:true,animations:'disabled',path:path.join(dir,`google-${name}-${width}-${theme}.png`)});if(width===390&&['google-profile','linked-profile'].includes(name)){await page.locator('#google-account-heading').scrollIntoViewIfNeeded();await page.screenshot({animations:'disabled',path:path.join(dir,`google-${name}-connections-${width}-${theme}.png`)});await page.locator('h1').first().scrollIntoViewIfNeeded();}}}
for(const js of [true,false])test(`a stakeholder joins by verified email and reviews without GitHub (JS ${js})`,async()=>{
 const code=js?'reviewer-js':'reviewer-native';const owner=await context(js);const client=await context(js,code);
 try{
 await client.page.goto(`${origin}/auth/login`);assert.equal(await client.page.getByRole('link',{name:'Continue with Google',exact:true}).count(),1);if(js)await shots(client.page,'login');
 await client.page.getByRole('link',{name:'Continue with Google',exact:true}).click();await client.page.waitForURL(/\/workspaces$/);assert.match(await client.page.locator('main').innerText(),/Welcome to Previa/);if(js)await shots(client.page,'welcome');
 await client.page.goto(`${origin}/protected`);assert.match(await client.page.locator('main').innerText(),/GitHub is not connected/);if(js)await shots(client.page,'google-profile');
 const csrf=await client.page.locator('input[name=csrf_token]').first().getAttribute('value');
 assert.equal((await client.ctx.request.post(`${origin}/workspaces`,{form:{csrf_token:csrf,name:'Forbidden owner'},maxRedirects:0})).status(),403);
 assert.equal((await client.ctx.request.post(`${origin}/auth/github/link`,{form:{csrf_token:'forged'},maxRedirects:0})).status(),403);
 await owner.page.goto(`${origin}/auth/github/login`);await owner.page.goto(`${origin}/workspaces/${workspace}/members`);
 await owner.page.getByRole('button',{name:'Invite teammate',exact:true}).first().click();const dialog=owner.page.getByRole('dialog',{name:'Invite a teammate',exact:true});
 await dialog.getByLabel('GitHub username or reviewer email').fill(`${code}@example.test`);await dialog.locator('input[name=role][value=reviewer]').check();
 await dialog.locator('input[type=checkbox][name=repository]').first().check();if(js)await shots(owner.page,'email-invite');await dialog.getByRole('button',{name:'Create invitation',exact:true}).click();
 const link=await owner.page.locator('#invitation-link').inputValue();
 await client.page.goto(link);assert.match(await client.page.locator('main').innerText(),/reviewer/i);if(js)await shots(client.page,'consent');
 await client.page.getByRole('button',{name:/Accept invitation|Join workspace/}).click();
 await client.page.goto(`${origin}${process.env.GOOGLE_REVIEW_PATH}`);await client.page.locator('#finding-body').fill(`Google reviewer finding ${code}`);await client.page.locator('#new-finding button[type=submit]').click();assert.match(await client.page.locator('main').innerText(),new RegExp(`Google reviewer finding ${code}`));if(js)await shots(client.page,'review');
 const denied=await client.ctx.request.post(`${origin}${process.env.GOOGLE_REVIEW_PATH.replace(/\/review$/,'/rebuild')}`,{form:{csrf_token:csrf},maxRedirects:0});assert.ok([403,404].includes(denied.status()));
 await client.page.goto(`${origin}/protected`);await client.page.getByRole('button',{name:'Connect GitHub',exact:true}).click();assert.match(await client.page.locator('main').innerText(),/could not be connected/); // The owner's existing GitHub identity cannot be stolen.
 assert.deepEqual(client.errors,[]);assert.deepEqual(owner.errors,[]);
 }finally{await client.ctx.close();await owner.ctx.close();}
});
test('an existing GitHub account explicitly connects Google and can return through it',async()=>{
 const u=await context(true,'linked-owner');try{
 await u.page.goto(`${origin}/auth/github/login`);await u.page.goto(`${origin}/protected`);
 await u.page.getByRole('button',{name:'Connect Google',exact:true}).click();assert.match(await u.page.locator('main').innerText(),/linked-owner@example.test/);await shots(u.page,'linked-profile');
 const csrf=await u.page.locator('input[name=csrf_token]').first().getAttribute('value');await u.ctx.request.post(`${origin}/auth/logout`,{form:{csrf_token:csrf}});
 await u.page.goto(`${origin}/auth/google/login`);await u.page.goto(`${origin}/workspaces/${workspace}/members`);assert.equal(await u.page.getByRole('button',{name:'Invite teammate',exact:true}).count(),1);assert.deepEqual(u.errors,[]);
 }finally{await u.ctx.close();}
});
