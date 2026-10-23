// Runs last: it enrols TOTP for the dev user, which ends dev mode's 2FA skip.
import { test, expect } from '@playwright/test';
import crypto from 'node:crypto';
import { credentials, watch } from './helpers.js';

function totp(secret) {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
  let bits = '';
  for (const c of secret.replace(/=+$/, '')) bits += alphabet.indexOf(c).toString(2).padStart(5, '0');
  const key = Buffer.from(bits.match(/.{8}/g).map((b) => parseInt(b, 2)));
  const ctr = Buffer.alloc(8);
  ctr.writeBigUInt64BE(BigInt(Math.floor(Date.now() / 30000)));
  const h = crypto.createHmac('sha1', key).update(ctr).digest();
  const o = h[19] & 15;
  return String(((h.readUInt32BE(o) & 0x7fffffff) % 1e6)).padStart(6, '0');
}

test('enrol TOTP, add a passkey, then sign in with it', async ({ page, context }) => {
  const done = watch(page);
  const { email, password } = credentials();
  const cdp = await context.newCDPSession(page);
  await cdp.send('WebAuthn.enable');
  await cdp.send('WebAuthn.addVirtualAuthenticator', { options: { protocol: 'ctap2', transport: 'internal', hasResidentKey: true, hasUserVerification: true, isUserVerified: true } });
  page.on('dialog', (d) => d.accept('E2E key'));
  const signIn = async () => {
    await page.goto('/admin/login');
    await page.fill('input[name=email]', email);
    await page.fill('input[name=password]', password);
    await page.click('button[type=submit]');
  };
  const sudo = async (next) => {
    await page.goto('/admin/sudo?next=' + next);
    await page.fill('input[name=password]', password);
    await page.click('button[type=submit]');
    await page.waitForURL('**' + next);
  };
  await signIn();
  await page.waitForURL('**/admin/');
  await sudo('/admin/enroll');
  const secret = (await page.textContent('.secret')).trim();
  await page.fill('input[name=code]', totp(secret));
  await page.click('button[type=submit]');
  await expect(page.locator('.recovery-codes li')).toHaveCount(10);
  await sudo('/admin/account');
  await page.click('[data-passkey-register]');
  await page.waitForURL(/ok=saved/);
  await expect(page.locator('text=E2E key')).toBeVisible();
  await page.click('button[aria-label="Sign out"]');
  await signIn();
  await page.waitForURL('**/admin/login/2fa**');
  await page.click('[data-passkey-login]');
  await page.waitForURL('**/admin/');
  done();
});
