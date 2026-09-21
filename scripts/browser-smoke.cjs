// Optional UI smoke test. Install Playwright outside the production image:
// npm install --prefix /tmp/torproxy-browser playwright
// PLAYWRIGHT_MODULE=/tmp/torproxy-browser/node_modules/playwright \
// BASE_URL=http://localhost:8080 node scripts/browser-smoke.cjs
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');

(async () => {
  const browser = await chromium.launch({ headless: true });
  try {
    const page = await browser.newPage({ viewport: { width: 1440, height: 1080 } });
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    await page.goto(process.env.BASE_URL || 'http://localhost:8080');
    await page.waitForFunction(() => document.getElementById('connection').textContent === 'Live');
    assert.equal(await page.title(), 'TorRotatingHttpProxy');
    assert.ok(await page.locator('#instances tr').count() > 0);
    await page.locator('#test-url').fill(process.env.TEST_URL || 'https://api.ipify.org');
    await page.locator('#test-rps').fill('1');
    await page.locator('#test-start').click();
    await page.waitForFunction(() => document.getElementById('test-start').disabled);
    await page.waitForFunction(() => {
      const count = id => Number(document.getElementById(id).textContent.replace(/[^0-9]/g, ''));
      return count('test-success') + count('test-failed') >= 2;
    }, { }, { timeout: 70000 });
    await page.locator('#test-stop').click();
    await page.waitForFunction(() => document.getElementById('test-state').textContent === 'Stopped');
    assert.equal(await page.locator('#test-start').isEnabled(), true);
    assert.equal(await page.locator('#test-stop').isDisabled(), true);
    await page.locator('#ip-search').fill('no-match');
    await page.waitForFunction(() => document.querySelector('#ip-history').textContent.includes('No matching'));
    await page.locator('#ip-search').fill('');
    await page.locator('#ip-filter').selectOption('true');
    await page.waitForTimeout(500);
    await page.locator('#ip-filter').selectOption('');
    await page.waitForTimeout(500);
    if (process.env.SCREENSHOT) await page.screenshot({path: process.env.SCREENSHOT, fullPage: true});
    await page.setViewportSize({width: 390, height: 844});
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true, 'mobile layout overflows');
    await page.reload();
    await page.waitForFunction(() => document.getElementById('connection').textContent === 'Live');
    assert.equal(await page.locator('#test-state').textContent(), 'Stopped');
    assert.deepEqual(errors, []);
    console.log('Dashboard passed: SSE, test Start/Stop, metrics, IP filter, reconnect, mobile layout; no JavaScript errors.');
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
