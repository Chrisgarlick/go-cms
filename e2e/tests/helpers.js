import fs from 'node:fs';
import AxeBuilder from '@axe-core/playwright';
import { expect } from '@playwright/test';

export function credentials() {
  const raw = fs.readFileSync(process.env.CMS_CREDENTIALS, 'utf8');
  return { email: raw.match(/email: (\S+)/)[1], password: raw.match(/password: (\S+)/)[1] };
}

// watch fails the test on any CSP violation or uncaught error.
export function watch(page) {
  const errors = [];
  page.on('console', (m) => {
    const t = m.text();
    if (/status of 404/.test(t)) return; // the 404 page itself
    if (m.type() === 'error' || /Content Security Policy/.test(t)) errors.push(t);
  });
  page.on('pageerror', (e) => errors.push(e.message));
  return () => expect(errors, errors.join('\n')).toEqual([]);
}

export async function login(page) {
  const c = credentials();
  await page.goto('/admin/login');
  await page.fill('input[name=email]', c.email);
  await page.fill('input[name=password]', c.password);
  await page.click('button[type=submit]');
  await page.waitForURL('**/admin/');
}

export async function a11y(page) {
  const res = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag22aa']).analyze();
  const serious = res.violations.filter((v) => ['serious', 'critical'].includes(v.impact));
  expect(serious.map((v) => `${v.id}: ${v.help} (${v.nodes.length})`)).toEqual([]);
}
