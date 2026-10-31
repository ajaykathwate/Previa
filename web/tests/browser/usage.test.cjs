const {test,before,after}=require('node:test');
const assert=require('node:assert/strict');
const {chromium}=require('playwright');
const {mkdirSync}=require('node:fs');
const path=require('node:path');
const origin=process.env.USAGE_BROWSER_URL;
const usagePath=process.env.USAGE_PATH;
let browser;
before(async()=>{browser=await chromium.launch({headless:true,...(process.env.UI_BROWSER_EXECUTABLE?{executablePath:process.env.UI_BROWSER_EXECUTABLE}:{})});});
after(async()=>{await browser?.close();});
async function session(login,javaScriptEnabled=true){
  const context=await browser.newContext({javaScriptEnabled,viewport:{width:1440,height:1000}});
  const page=await context.newPage();page.setDefaultTimeout(10000);
  const errors=[];
  page.on('pageerror',e=>errors.push(e.message));
  page.on('console',m=>{if(/Content Security Policy|Refused to/.test(m.text()))errors.push(m.text());});
  await context.request.get(`${origin}/__fixture/identity?login=${login}`);
  await page.goto(`${origin}/auth/github/login`);
  return {context,page,errors};
}
async function shots(page,role='owner'){
  for(const width of [1440,390]){
    await page.setViewportSize({width,height:1000});
    assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),'no horizontal page overflow');
    for(const theme of ['light','dark']){
      await page.evaluate(t=>{document.documentElement.dataset.theme=t;},theme);
      if(process.env.UI_BROWSER_SCREENSHOTS==='1'){
        const dir=path.join(__dirname,'artifacts');mkdirSync(dir,{recursive:true});
        const height=await page.evaluate(()=>{const p=document.querySelector('.app-content');return Math.ceil(p.scrollHeight+p.getBoundingClientRect().top);});
        await page.setViewportSize({width,height:Math.min(height,6000)});
        await page.screenshot({animations:'disabled',path:path.join(dir,`usage-${role}-${width}-${theme}.png`)});
        await page.setViewportSize({width,height:1000});
      }
    }
  }
}
for(const js of [true,false]){
  test(`owner sees measured usage, finite limits and actionable blocked work (JS ${js})`,async()=>{
    const {context,page,errors}=await session('owner',js);
    try{
      const response=await page.goto(`${origin}${usagePath}`);assert.equal(response.status(),200);
      assert.equal(await page.locator('h1').innerText(),'Usage');
      const body=await page.locator('main').innerText();
      assert.match(body,/Free early access/);assert.match(body,/Consumed minutes\s+0\.70/);
      assert.match(body,/Build minutes included\s+300\.00/);
      assert.match(body,/acme\/private/);assert.match(body,/Next check:/);assert.match(body,/Measured/);
      assert.match(body,/Historical technical counters are legacy/);
      assert.doesNotMatch(body,/@components|@layouts/,'template instructions never reach the page');
      assert.ok(await page.locator('main time[datetime]').count()>=5,'measurement and reset dates render as real timestamps');
      assert.doesNotMatch(body,/Upgrade|Purchase|Buy now|Checkout/);
      assert.equal(await page.locator('.app-sidebar .sidebar-nav a[aria-current=page]').innerText(),'Usage');
      await page.getByRole('link',{name:'Refresh usage',exact:true}).click();assert.equal(new URL(page.url()).pathname,usagePath);
      if(js)await shots(page);
      assert.deepEqual(errors,[]);
    }finally{await context.close();}
  });
  test(`developer sees only granted technical usage and reviewer is denied (JS ${js})`,async()=>{
    const {context,page,errors}=await session('developer',js);
    try{
      const response=await page.goto(`${origin}${usagePath}`);assert.equal(response.status(),200);
      const body=await page.locator('main').innerText();
      assert.match(body,/acme\/api/);assert.doesNotMatch(body,/acme\/private|Consumed minutes|Build minutes included/);
      assert.match(body,/Owners and Admins can see workspace totals/);assert.deepEqual(errors,[]);
      if(js)await shots(page,'developer');
    }finally{await context.close();}
    const reviewer=await session('reviewer',js);
    try{const response=await reviewer.page.goto(`${origin}${usagePath}`);assert.equal(response.status(),404);assert.deepEqual(reviewer.errors,[]);}finally{await reviewer.context.close();}
  });
}
