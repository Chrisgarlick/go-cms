// Tiny DOM helpers shared by the admin scripts. No framework.
// fill replaces an element's children, skipping null/false (unlike replaceChildren).
const clean = (list) => list.flat(Infinity).filter((c) => c != null && c !== false);
export function fill(el, ...children) { el.replaceChildren(...clean(children)); return el; }

export function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'dataset') Object.assign(el.dataset, v);
    else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2).toLowerCase(), v);
    else if (k === 'value') el.value = v;
    else if (k === 'checked') el.checked = !!v;
    else if (v === true) el.setAttribute(k, '');
    else el.setAttribute(k, v);
  }
  for (const c of children.flat(Infinity)) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

const SVGNS = 'http://www.w3.org/2000/svg';
export function icon(name) {
  const svg = document.createElementNS(SVGNS, 'svg');
  svg.setAttribute('class', 'icon');
  svg.setAttribute('aria-hidden', 'true');
  const use = document.createElementNS(SVGNS, 'use');
  use.setAttribute('href', `/admin/assets/icons.svg#${name}`);
  svg.append(use);
  return svg;
}

export function iconButton(name, label, onclick, extra = '') {
  return h('button', { type: 'button', class: `icon-btn ${extra}`, title: label, 'aria-label': label, onclick }, icon(name));
}

let toastBox;
export function toast(msg, kind = 'ok', ms = 3500) {
  if (!toastBox) {
    toastBox = h('div', { class: 'toasts', role: 'status', 'aria-live': 'polite' });
    document.body.append(toastBox);
  }
  const t = h('div', { class: `toast toast-${kind}` }, msg);
  toastBox.append(t);
  setTimeout(() => t.remove(), ms);
}

export function csrf() {
  const d = document.getElementById('editor-data') || document.getElementById('options-data') || document.getElementById('menu-data');
  if (window.__csrf) return window.__csrf;
  try { window.__csrf = JSON.parse(d.textContent).csrf; } catch { /* ignore */ }
  if (!window.__csrf) {
    const input = document.querySelector('input[name=csrf]');
    window.__csrf = input ? input.value : '';
  }
  return window.__csrf;
}

// api wraps fetch with CSRF, JSON and friendly errors.
export async function api(method, url, body) {
  const opts = { method, headers: { Accept: 'application/json', 'X-CSRF-Token': csrf() }, credentials: 'same-origin' };
  if (body instanceof FormData) opts.body = body;
  else if (body !== undefined) { opts.body = JSON.stringify(body); opts.headers['Content-Type'] = 'application/json'; }
  let res;
  try { res = await fetch(url, opts); } catch {
    const e = new Error('You appear to be offline. Changes will be retried.'); e.offline = true; throw e;
  }
  let data = {};
  try { data = await res.json(); } catch { /* not json */ }
  if (!res.ok) {
    const e = new Error(data.error || `Request failed (${res.status})`);
    e.status = res.status; e.data = data;
    if (data.sudo) { window.location.href = '/admin/sudo?next=' + encodeURIComponent(location.pathname); }
    if (res.status === 401) { window.location.href = '/admin/login?next=' + encodeURIComponent(location.pathname); }
    throw e;
  }
  return data;
}

export function modal(title, body, foot) {
  const close = () => { box.remove(); document.removeEventListener('keydown', onKey); last?.focus(); };
  const onKey = (e) => { if (e.key === 'Escape') close(); };
  const last = document.activeElement;
  const box = h('div', { class: 'modal', role: 'dialog', 'aria-modal': 'true', 'aria-label': title },
    h('div', { class: 'modal-box' },
      h('div', { class: 'modal-head' }, h('h2', {}, title), iconButton('x', 'Close', () => close())),
      h('div', { class: 'modal-body' }, body),
      foot ? h('div', { class: 'modal-foot' }, foot) : null));
  box.addEventListener('mousedown', (e) => { if (e.target === box) close(); });
  document.addEventListener('keydown', onKey);
  document.body.append(box);
  setTimeout(() => (box.querySelector('input,button.picker-item,button') || box).focus(), 0);
  return { close, el: box };
}

export function debounce(fn, ms) {
  let t;
  const d = (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); };
  d.flush = (...a) => { clearTimeout(t); return fn(...a); };
  d.cancel = () => clearTimeout(t);
  return d;
}

export function autosize(ta) {
  const fit = () => { ta.style.height = 'auto'; ta.style.height = ta.scrollHeight + 'px'; };
  ta.addEventListener('input', fit);
  requestAnimationFrame(fit);
  return fit;
}

export function slugify(s) {
  return (s || '').toLowerCase().normalize('NFKD').replace(/[̀-ͯ]/g, '').replace(/&/g, ' and ')
    .replace(/['’]/g, '').replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 96);
}

export function uid() {
  const a = new Uint8Array(6); crypto.getRandomValues(a);
  return Array.from(a, (b) => b.toString(16).padStart(2, '0')).join('');
}
