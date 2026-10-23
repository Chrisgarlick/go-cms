// Behaviour for server-rendered admin pages (no inline scripts: strict CSP).
import { api, toast, h } from './editor/ui.js';

document.addEventListener('submit', (e) => {
  const f = e.target.closest('form[data-confirm]');
  if (f && !confirm(f.dataset.confirm)) e.preventDefault();
});

document.addEventListener('change', (e) => {
  const f = e.target.closest('form[data-autosubmit]');
  if (f) f.requestSubmit();
});

document.querySelectorAll('[data-select]').forEach((el) => el.addEventListener('focus', () => el.select()));

document.querySelector('[data-copy-codes]')?.addEventListener('click', async (e) => {
  const codes = [...document.querySelectorAll('.recovery-codes code')].map((c) => c.textContent).join('\n');
  try { await navigator.clipboard.writeText(codes); e.target.textContent = 'Copied'; } catch { /* clipboard blocked */ }
});

// Media uploads: file input and drag-and-drop.
async function upload(files) {
  const progress = document.querySelector('[data-progress]');
  let done = 0;
  const errors = [];
  for (const file of files) {
    if (progress) progress.textContent = `Uploading ${file.name} (${done + 1} of ${files.length})…`;
    const fd = new FormData();
    fd.append('file', file, file.name);
    try {
      const res = await api('POST', '/admin/api/media', fd);
      if (res.errors?.length) errors.push(...res.errors);
    } catch (err) {
      errors.push(`${file.name}: ${err.message}`);
    }
    done++;
  }
  if (errors.length) {
    if (progress) progress.replaceChildren(...errors.map((m) => h('div', { class: 'error-text' }, m)));
    toast(`${errors.length} file(s) failed`, 'error');
    if (done > errors.length) setTimeout(() => location.reload(), 2500);
  } else {
    location.href = location.pathname + '?ok=uploaded';
  }
}

document.querySelector('[data-upload]')?.addEventListener('change', (e) => upload([...e.target.files]));
const dz = document.querySelector('[data-dropzone]');
if (dz) {
  const on = (e) => { e.preventDefault(); dz.classList.add('is-over'); };
  const off = () => dz.classList.remove('is-over');
  document.addEventListener('dragover', on);
  document.addEventListener('dragleave', (e) => { if (!e.relatedTarget) off(); });
  document.addEventListener('drop', (e) => {
    e.preventDefault(); off();
    if (e.dataTransfer?.files?.length) upload([...e.dataTransfer.files]);
  });
}

// Focal point picker.
const focal = document.querySelector('[data-focal]');
if (focal) {
  const img = focal.querySelector('[data-focal-img]');
  const dot = focal.querySelector('[data-focal-dot]');
  const fx = document.querySelector('[data-focal-x]');
  const fy = document.querySelector('[data-focal-y]');
  const place = () => {
    const r = img.getBoundingClientRect(), p = focal.getBoundingClientRect();
    dot.style.left = (r.left - p.left + r.width * parseFloat(fx.value || 0.5)) + 'px';
    dot.style.top = (r.top - p.top + r.height * parseFloat(fy.value || 0.5)) + 'px';
  };
  img.addEventListener('load', place);
  window.addEventListener('resize', place);
  if (img.complete) place();
  img.addEventListener('click', (e) => {
    const r = img.getBoundingClientRect();
    fx.value = ((e.clientX - r.left) / r.width).toFixed(3);
    fy.value = ((e.clientY - r.top) / r.height).toFixed(3);
    place();
  });
}

// Character counters for textareas with data-count.
document.querySelectorAll('textarea[data-count]').forEach((ta) => {
  const c = h('div', { class: 'counter' });
  ta.after(c);
  const upd = () => { c.textContent = `${ta.value.length} characters`; };
  ta.addEventListener('input', upd); upd();
});

// Passkeys (WebAuthn). The server sends options with base64url fields; the
// browser API wants ArrayBuffers, and the server wants base64url back.
const b64u = {
  dec: (s) => Uint8Array.from(atob(s.replace(/-/g, '+').replace(/_/g, '/').padEnd(Math.ceil(s.length / 4) * 4, '=')), (c) => c.charCodeAt(0)).buffer,
  enc: (buf) => btoa(String.fromCharCode(...new Uint8Array(buf))).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, ''),
};

function credentialJSON(c) {
  const r = c.response;
  const out = { id: c.id, rawId: b64u.enc(c.rawId), type: c.type, clientExtensionResults: c.getClientExtensionResults?.() || {}, response: {} };
  for (const k of ['clientDataJSON', 'attestationObject', 'authenticatorData', 'signature', 'userHandle']) {
    if (r[k]) out.response[k] = b64u.enc(r[k]);
  }
  if (r.getTransports) out.response.transports = r.getTransports();
  if (c.authenticatorAttachment) out.authenticatorAttachment = c.authenticatorAttachment;
  return out;
}

async function postJSON(url, body) {
  const res = await fetch(url, { method: 'POST', credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json', 'X-CSRF-Token': document.querySelector('input[name=csrf]')?.value || '' },
    body: body === undefined ? undefined : JSON.stringify(body) });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) { const e = new Error(data.error || 'Request failed'); e.data = data; throw e; }
  return data;
}

document.querySelector('[data-passkey-login]')?.addEventListener('click', async (e) => {
  const next = e.currentTarget.dataset.next || '/admin/'; // currentTarget is null after the first await
  const err = document.querySelector('[data-passkey-error]');
  err.textContent = '';
  try {
    const opts = await postJSON('/admin/login/passkey/begin');
    const pk = opts.publicKey;
    pk.challenge = b64u.dec(pk.challenge);
    (pk.allowCredentials || []).forEach((c) => { c.id = b64u.dec(c.id); });
    const cred = await navigator.credentials.get({ publicKey: pk });
    const res = await postJSON('/admin/login/passkey/finish?next=' + encodeURIComponent(next), credentialJSON(cred));
    location.href = res.redirect || '/admin/';
  } catch (ex) { err.textContent = ex.name === 'NotAllowedError' ? 'Passkey sign-in was cancelled.' : ex.message; }
});

document.querySelector('[data-passkey-register]')?.addEventListener('click', async () => {
  const err = document.querySelector('[data-passkey-error]');
  err.textContent = '';
  try {
    const opts = await postJSON('/admin/api/passkeys/begin');
    const pk = opts.publicKey;
    pk.challenge = b64u.dec(pk.challenge);
    pk.user.id = b64u.dec(pk.user.id);
    (pk.excludeCredentials || []).forEach((c) => { c.id = b64u.dec(c.id); });
    const cred = await navigator.credentials.create({ publicKey: pk });
    const name = prompt('Name this passkey (e.g. "MacBook Touch ID")', 'Passkey') || 'Passkey';
    await postJSON('/admin/api/passkeys/finish?name=' + encodeURIComponent(name), credentialJSON(cred));
    location.href = '/admin/account?ok=saved';
  } catch (ex) {
    if (ex.data?.sudo) { location.href = '/admin/sudo?next=/admin/account'; return; }
    err.textContent = ex.name === 'NotAllowedError' ? 'Adding the passkey was cancelled.' : ex.message;
  }
});
