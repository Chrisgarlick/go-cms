// Screenshots of the admin for design review: `./scripts/e2e.sh shots`
// signs in to a fresh blog starter and saves full-page PNGs (desktop, dark,
// and a 390 px phone) to SHOTS_DIR (default ../bin/shots), with weights.json:
// each screen's gzipped weight by kind and its lab LCP and CLS.
import { chromium } from '@playwright/test';
import fs from 'node:fs';
import zlib from 'node:zlib';

const base = process.env.CMS_BASE_URL;
const raw = fs.readFileSync(process.env.CMS_CREDENTIALS, 'utf8');
const email = raw.match(/email: (\S+)/)[1], password = raw.match(/password: (\S+)/)[1];
const out = process.env.SHOTS_DIR || '../bin/shots';
fs.mkdirSync(out, { recursive: true });

const browser = await chromium.launch();
const ctx = await browser.newContext({ viewport: { width: 1360, height: 900 } });
const page = await ctx.newPage();
const phone = await browser.newContext({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true });
let loaded = [];
page.on('response', async (r) => {
  const kind = r.request().resourceType();
  if (!['document', 'stylesheet', 'script', 'font'].includes(kind) || r.status() >= 300) return;
  try { const b = await r.body(); loaded.push({ kind, bytes: kind === 'font' ? b.length : zlib.gzipSync(b, { level: 6 }).length }); } catch { /* gone */ }
});
page.on('framenavigated', (f) => { if (f === page.mainFrame()) loaded = []; });
await ctx.addInitScript(() => {
  window.__vitals = { lcp: 0, cls: 0 };
  new PerformanceObserver((l) => { for (const e of l.getEntries()) window.__vitals.lcp = e.startTime; }).observe({ type: 'largest-contentful-paint', buffered: true });
  new PerformanceObserver((l) => { for (const e of l.getEntries()) if (!e.hadRecentInput) window.__vitals.cls += e.value; }).observe({ type: 'layout-shift', buffered: true });
});
const weights = {};
const shot = async (name) => {
  await page.waitForLoadState('networkidle').catch(() => {});
  await page.waitForTimeout(300);
  const by = {};
  for (const l of loaded) by[l.kind] = (by[l.kind] || 0) + l.bytes;
  const v = await page.evaluate(() => window.__vitals).catch(() => ({}));
  weights[name] = { ...by, total: Object.values(by).reduce((a, b) => a + b, 0), lcp: Math.round(v.lcp || 0), cls: +(v.cls || 0).toFixed(3) };
  await page.screenshot({ path: `${out}/${name}.png`, fullPage: true });
  await page.emulateMedia({ colorScheme: 'dark' }); await page.waitForTimeout(250);
  await page.screenshot({ path: `${out}/${name}-dark.png`, fullPage: true });
  await page.emulateMedia({ colorScheme: 'light' });
  await phone.addCookies(await ctx.cookies());
  const p = await phone.newPage(); await p.goto(page.url()); await p.waitForTimeout(300);
  const wide = await p.evaluate(() => document.documentElement.scrollWidth > window.innerWidth + 1);
  await p.screenshot({ path: `${out}/${name}-phone.png`, fullPage: true }); await p.close();
  console.log('shot', name, wide ? 'PHONE SCROLLS SIDEWAYS' : '');
};
const go = async (name, path) => { await page.goto(base + path); await shot(name); };

await go('01-login', '/admin/login');
await page.fill('input[name=email]', email); await page.fill('input[name=password]', password);
await page.click('button[type=submit]'); await page.waitForURL('**/admin/');
await shot('02-dashboard');
// Content types, menus and options from the admin's own links.
const hrefs = await page.$$eval('a[href^="/admin/"]', (as) => [...new Set(as.map((a) => a.getAttribute('href')))]);
const first = (re) => hrefs.find((h) => re.test(h));
const list = first(/^\/admin\/content\/[^/]+\/$/);
if (list) {
  await go('03-content-list', list);
  const entry = await page.$$eval('a[href^="/admin/entries/"]', (as) => as.map((a) => a.getAttribute('href'))[0]);
  if (entry) await go('04-editor', entry);
}
await go('05-media', '/admin/media/');
await go('06-menus', '/admin/menus/');
const menu = await page.$$eval('a[href^="/admin/menus/"]', (as) => as.map((a) => a.getAttribute('href')).find((h) => h !== '/admin/menus/'));
if (menu) await go('07-menu-edit', menu);
const terms = first(/^\/admin\/terms\//); if (terms) await go('08-terms', terms);
const options = first(/^\/admin\/options\//); if (options) await go('09-options', options);
await go('10-forms', '/admin/forms/');
await go('11-redirects', '/admin/redirects/');
await go('12-users', '/admin/users/');
await go('13-settings', '/admin/settings/');
await go('14-security', '/admin/security/');
await go('15-system', '/admin/system/');
await go('16-account', '/admin/account');
fs.writeFileSync(`${out}/weights.json`, JSON.stringify(weights, null, 2) + '\n');
console.table(Object.fromEntries(Object.entries(weights).map(([k, w]) => [k, { kB: +(w.total / 1024).toFixed(1), css: +((w.stylesheet || 0) / 1024).toFixed(1), js: +((w.script || 0) / 1024).toFixed(1), lcp: w.lcp, cls: w.cls }])));
await browser.close();
