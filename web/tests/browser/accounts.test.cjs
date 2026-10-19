const {test,before,after}=require('node:test');
const assert=require('node:assert/strict');
const {chromium}=require('playwright');
const {mkdirSync}=require('node:fs');
const path=require('node:path');
const origin=process.env.ACCOUNTS_BROWSER_URL;
let browser;
before(async()=>{browser=await chromium.launch({headless:true,...(process.env.UI_BROWSER_EXECUTABLE?{executablePath:process.env.UI_BROWSER_EXECUTABLE}:{})});});
after(async()=>{await browser?.close();});
async function session(js=true){
 const context=await browser.newContext({javaScriptEnabled:js,viewport:{width:1440,height:1000}});
 const page=await context.newPage();page.setDefaultTimeout(10000);const errors=[];
 page.on('pageerror',e=>errors.push(e.message));page.on('console',m=>{if(/Content Security Policy|Refused to/.test(m.text()))errors.push(m.text());});
 await page.goto(`${origin}/auth/github/login`);return {context,page,errors};
}
async function shots(page,name){
 for(const width of [1440,390])for(const theme of ['light','dark']){
  await page.setViewportSize({width,height:1000});await page.evaluate(t=>document.documentElement.dataset.theme=t,theme);
  assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),`${name}: no horizontal overflow at ${width}`);
  if(process.env.UI_BROWSER_SCREENSHOTS==='1'){const dir=path.join(__dirname,'artifacts');mkdirSync(dir,{recursive:true});await page.screenshot({fullPage:true,animations:'disabled',path:path.join(dir,`accounts-${name}-${width}-${theme}.png`)});}
 }
}
for(const js of [true,false]){
 test(`support persists, operator access and public status/policies work (JS ${js})`,async()=>{
  const {context,page,errors}=await session(js);
  try{
   await page.goto(`${origin}/help`);await page.getByLabel('What happened?').fill('Build failed <script>window.bad=true</script>');await page.getByRole('button',{name:'Send request',exact:true}).click();
   assert.match(new URL(page.url()).pathname,/^\/help\/requests\//);assert.equal(await page.locator('.support-message').innerText(),'Build failed <script>window.bad=true</script>');
   assert.match(await page.locator('main').innerText(),/Diagnostic reference:/);if(js)await shots(page,'support');
   await page.goto(`${origin}/operator/support`);if(js)await shots(page,'operator');await page.getByLabel('Public update').first().fill('Investigating slow build starts');await page.getByRole('button',{name:'Publish update',exact:true}).first().click();
   await page.goto(`${origin}/status`);assert.match(await page.locator('main').innerText(),/unknown/);assert.match(await page.locator('main').innerText(),/Investigating slow build starts/);if(js)await shots(page,'status');
   for(const kind of ['privacy','terms','acceptable-use']){const r=await page.goto(`${origin}/policies/${kind}`);assert.equal(r.status(),200);if(js)await shots(page,kind);assert.ok((await page.locator('main').innerText()).length>300);}
   assert.deepEqual(errors,[]);
  }finally{await context.close();}
 });
 test(`disconnect uses an accessible modal and honest durable progress (JS ${js})`,async()=>{
  const {context,page,errors}=await session(js);
  try{
   await page.goto(`${origin}/account`);if(js)await shots(page,'settings');
   const repo=js?'acme/js':'acme/native';const row=page.locator('.field-row').filter({has:page.locator('code').filter({hasText:repo})});
   await row.getByRole('button',{name:'Disconnect repository',exact:true}).click();const dialog=page.getByRole('dialog',{name:`Disconnect ${repo}?`,exact:true});assert.equal(await dialog.isVisible(),true);if(js)await shots(page,'disconnect-dialog');
   await dialog.getByRole('button',{name:'Cancel',exact:true}).click();assert.equal(await dialog.isVisible(),false);
   await row.getByRole('button',{name:'Disconnect repository',exact:true}).click();await dialog.getByLabel('Type DISCONNECT to confirm').fill('DISCONNECT');await dialog.getByRole('button',{name:'Confirm',exact:true}).click();
   assert.match(new URL(page.url()).pathname,/^\/account\/operations\//);assert.match(await page.locator('main').innerText(),/not yet complete/);if(js)await shots(page,'progress-pending');
   assert.equal((await context.request.post(`${origin}/__fixture/cleanup`)).status(),204);await page.reload();assert.match(await page.locator('main').innerText(),/Complete ·/);if(js)await shots(page,'progress-complete');
   await page.goto(`${origin}/account`);assert.equal(await page.locator('.field-row code').filter({hasText:repo}).count(),0);assert.deepEqual(errors,[]);
  }finally{await context.close();}
 });
}
test('ending other sessions preserves this browser and revokes another signed-in browser',async()=>{
 const current=await session(),other=await session();
 try{await current.page.goto(`${origin}/protected`);await current.page.getByRole('button',{name:'End other sessions',exact:true}).first().click();await shots(current.page,'session-dialog');await current.page.getByRole('dialog',{name:'End other sessions?',exact:true}).getByRole('button',{name:'End other sessions',exact:true}).click();assert.equal(new URL(current.page.url()).pathname,'/protected');assert.equal((await other.context.request.get(`${origin}/account`,{maxRedirects:0})).status(),303);assert.deepEqual(current.errors,[]);}finally{await current.context.close();await other.context.close();}
});
for(const js of [true,false]){
 test(`early access needs an operator approval and stays usable without scripts (JS ${js})`,async()=>{
  const {context,page,errors}=await session(js);
  try{
   await page.goto(`${origin}/workspaces`);
   await page.locator('main').getByRole('button',{name:'Create workspace',exact:true}).first().click();
   await page.locator('#workspace-name').fill(`Approval ${js?'JS':'native'}`);
   await page.locator('#create-workspace').getByRole('button',{name:'Create workspace',exact:true}).click();
   await page.waitForURL(/\/early-access$/);
   assert.match(await page.locator('main').innerText(),/awaiting approval/);
   // A hard reload also verifies the pending state survives navigation; it avoids
   // capturing the old workspace page during the create form's view transition.
   if(js){await page.reload();await shots(page,'early-access-pending');}
   const publicID=await page.locator('main code').first().innerText();
   const csrf=await page.locator('input[name=csrf_token]').first().getAttribute('value');
   assert.equal((await context.request.post(`${origin}/installations/start`,{form:{csrf_token:csrf},maxRedirects:0})).status(),403);
   await page.goto(`${origin}/help`);assert.equal(await page.getByRole('button',{name:'Send request',exact:true}).count(),1);
   const nonoperator=await browser.newContext({javaScriptEnabled:js});
   try{
    await nonoperator.addCookies([{name:'fixture_login',value:'member',url:origin}]);
    const member=await nonoperator.newPage();await member.goto(`${origin}/auth/github/login`);
    assert.equal((await nonoperator.request.get(`${origin}/operator/early-access`,{maxRedirects:0})).status(),404);
   }finally{await nonoperator.close();}
   await page.goto(`${origin}/operator/early-access`);if(js)await shots(page,'early-access-operator');
   const card=page.locator('section').filter({has:page.locator('code').filter({hasText:publicID})});
   await card.getByRole('button',{name:'Approve workspace',exact:true}).click();
   await page.goto(`${origin}/early-access`);assert.match(await page.locator('main').innerText(),/is approved/);
   if(js)await shots(page,'early-access-approved');assert.deepEqual(errors,[]);
  }finally{await context.close();}
 });
}
