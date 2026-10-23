import { test, expect } from '@playwright/test';
import { watch, a11y } from './helpers.js';

for (const path of ['/', '/blog/', '/blog/why-this-site-is-fast/', '/about/', '/category/engineering/', '/search/?q=site', '/does-not-exist/']) {
  test(`public page ${path} renders cleanly and accessibly`, async ({ page }) => {
    const done = watch(page);
    const res = await page.goto(path);
    expect(res.status()).toBe(path === '/does-not-exist/' ? 404 : 200);
    expect(res.headers()['content-security-policy']).toContain("script-src 'self'");
    await a11y(page);
    done();
  });
}

test('feeds, sitemap and robots are valid', async ({ request }) => {
  for (const p of ['/feed.xml', '/sitemap.xml', '/sitemap-post.xml', '/robots.txt']) {
    const r = await request.get(p);
    expect(r.status(), p).toBe(200);
  }
});
