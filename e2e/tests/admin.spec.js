import { test, expect } from '@playwright/test';
import { login, watch, a11y } from './helpers.js';

test('write, autosave and publish a post', async ({ page, request }) => {
  const done = watch(page);
  await login(page);
  await page.goto('/admin/content/post/');
  await page.click('button:has-text("New post")');
  await page.waitForSelector('.ed-title');
  const title = `E2E post ${Date.now()}`;
  await page.fill('.ed-title', title);
  await page.click('.ProseMirror >> nth=0');
  await page.keyboard.type('Written by the end-to-end suite.');
  await expect(page.locator('.ed-status')).toHaveText(/saved/i, { timeout: 10_000 });
  await page.click('button:has-text("Publish")');
  await expect(page.locator('.ed-card-head .badge')).toHaveText('Published', { timeout: 10_000 });
  const slug = title.toLowerCase().replace(/[^a-z0-9]+/g, '-');
  await expect.poll(async () => (await request.get(`/blog/${slug}/`)).status(), { timeout: 10_000 }).toBe(200);
  done();
});

test('trashing asks first, in a dialog, and Cancel keeps the entry', async ({ page }) => {
  const done = watch(page);
  await login(page);
  await page.goto('/admin/content/post/');
  await page.click('button:has-text("New post")');
  await page.waitForSelector('.ed-title');
  await page.fill('.ed-title', `E2E trash ${Date.now()}`);
  const trash = page.getByRole('button', { name: 'Trash' });
  await trash.click();
  const dialog = page.getByRole('dialog', { name: 'Move this to the trash?' });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole('button', { name: 'Cancel' })).toBeFocused();
  await a11y(page);
  await dialog.getByRole('button', { name: 'Cancel' }).click();
  await expect(dialog).toHaveCount(0);
  await trash.click();
  await page.getByRole('dialog').getByRole('button', { name: 'Move to trash' }).click();
  await page.waitForURL('**/admin/content/post/**');
  done();
});

test('the file picker is a modal dialog: Escape closes it and focus comes back', async ({ page }) => {
  const done = watch(page);
  await login(page);
  await page.goto('/admin/content/post/');
  await page.click('button:has-text("New post")');
  await page.waitForSelector('.ed-title');
  const choose = page.getByRole('button', { name: 'Choose image' });
  await choose.click();
  const dialog = page.getByRole('dialog', { name: 'Choose a file' });
  await expect(dialog).toBeVisible();
  await a11y(page);
  await page.keyboard.press('Escape');
  await expect(dialog).toHaveCount(0);
  await expect(choose).toBeFocused();
  done();
});

for (const path of ['/admin/', '/admin/content/post/', '/admin/media/', '/admin/users/', '/admin/settings/', '/admin/account', '/admin/security/']) {
  test(`admin ${path} has no CSP errors and passes axe`, async ({ page }) => {
    const done = watch(page);
    await login(page);
    await page.goto(path);
    await a11y(page);
    done();
  });
}
