// Renders forms from field definitions (internal/fields) and reads values back
// in exactly the JSON shapes the server validates.
import { h, fill, icon, iconButton, uid } from './ui.js';
import { richText } from './richtext.js';
import { pickMedia, pickEntry, mediaCache, mediaThumb } from './pickers.js';

const human = (s) => (s || '').replace(/[_-]+/g, ' ').replace(/^./, (c) => c.toUpperCase());

// renderObject renders a list of field definitions bound to an object value.
export function renderObject(defs, value, ctx) {
  value = value && typeof value === 'object' ? value : {};
  const box = h('div', { class: 'fgrid' });
  const items = [];
  for (const def of defs || []) {
    if (def.capability && ctx.can && !ctx.can(def.capability)) {
      box.append(h('div', { class: 'field' }, h('span', { class: 'label' }, def.label || human(def.name)),
        h('p', { class: 'hint' }, 'Only owners can edit this field.')));
      continue;
    }
    const f = renderField(def, value[def.name], ctx);
    const wrap = h('div', { class: `field${def.width >= 25 && def.width <= 50 ? ' w-' + (def.width <= 34 ? '33' : '50') : ''}`, dataset: { field: def.name } });
    if (def.type !== 'boolean' && def.type !== 'message') {
      wrap.append(h('span', { class: 'label', id: `l-${f.id}` }, def.label || human(def.name), def.required ? h('span', { class: 'req', 'aria-hidden': 'true' }, '*') : null));
    }
    wrap.append(f.el);
    if (def.help && def.type !== 'message') wrap.append(h('span', { class: 'hint' }, def.help));
    const err = h('span', { class: 'error-text', role: 'alert' });
    wrap.append(err);
    items.push({ def, f, wrap, err });
    box.append(wrap);
  }
  const evalConditions = () => {
    const cur = get();
    for (const it of items) {
      const c = it.def.condition;
      if (!c) continue;
      const v = cur[c.field];
      const show = c.not_empty ? v != null && v !== '' && !(Array.isArray(v) && !v.length) : String(v ?? '') === String(c.equals ?? '');
      it.wrap.hidden = !show;
    }
  };
  const get = () => {
    const out = {};
    for (const it of items) {
      if (it.wrap.hidden) continue;
      const v = it.f.get();
      if (v !== undefined && v !== null && v !== '' && !(Array.isArray(v) && v.length === 0)) out[it.def.name] = v;
    }
    return out;
  };
  box.addEventListener('input', evalConditions);
  box.addEventListener('change', evalConditions);
  requestAnimationFrame(evalConditions);
  return {
    el: box,
    get,
    setErrors(errors, prefix) {
      for (const it of items) {
        const msg = errors[prefix + it.def.name];
        it.err.textContent = msg ? human(it.def.label || it.def.name) + ' ' + msg : '';
        it.wrap.classList.toggle('field-error', !!msg);
        if (it.f.setErrors) it.f.setErrors(errors, prefix + it.def.name + '.');
      }
    },
    firstText() {
      for (const it of items) {
        const v = it.f.get();
        if (typeof v === 'string' && v.trim()) return v.trim();
        if (it.f.text) { const t = it.f.text(); if (t) return t; }
      }
      return '';
    },
  };
}

let counter = 0;

function renderField(def, value, ctx) {
  const id = 'f' + ++counter;
  const changed = () => ctx.onChange && ctx.onChange();
  const input = (attrs) => h('input', { id, 'aria-labelledby': `l-${id}`, oninput: changed, ...attrs });
  switch (def.type || 'text') {
    case 'text': case 'email': case 'url': case 'slug': case 'oembed': {
      const type = { email: 'email', url: 'url', oembed: 'url' }[def.type] || 'text';
      const el = input({ type, value: value ?? '', placeholder: def.placeholder || (def.type === 'oembed' ? 'https://www.youtube.com/watch?v=…' : null),
        maxlength: def.max_length || null, required: def.required && !ctx.partial ? null : null });
      return { id, el, get: () => el.value.trim() };
    }
    case 'textarea': case 'markdown': case 'code': case 'html': {
      const el = h('textarea', { id, 'aria-labelledby': `l-${id}`, rows: def.rows || (def.type === 'textarea' ? 3 : 8), class: def.type === 'textarea' ? null : 'mono',
        placeholder: def.placeholder || null, maxlength: def.max_length || null, oninput: changed, spellcheck: def.type === 'code' || def.type === 'html' ? 'false' : null }, value ?? '');
      return { id, el, get: () => el.value };
    }
    case 'number': case 'range': {
      const el = input({ type: 'number', value: value ?? '', min: def.min ?? null, max: def.max ?? null, step: def.step || 'any' });
      return { id, el, get: () => (el.value === '' ? null : Number(el.value)) };
    }
    case 'color': {
      const txt = input({ type: 'text', value: value ?? '', placeholder: '#000000', class: 'mono' });
      const pick = h('input', { type: 'color', value: /^#[0-9a-f]{6}$/i.test(value || '') ? value : '#000000', 'aria-label': 'Pick colour',
        oninput: (e) => { txt.value = e.target.value; changed(); } });
      return { id, el: h('div', { class: 'row' }, pick, txt), get: () => txt.value.trim() };
    }
    case 'richtext': {
      const rt = richText(value, { onChange: changed, placeholder: def.placeholder || 'Write something…' });
      rt.el.id = id;
      return { id, el: rt.el, get: rt.get, text: rt.text };
    }
    case 'select': case 'radio': {
      if (def.type === 'radio' || (def.options || []).length <= 3) {
        const name = id + '-r';
        const opts = (def.options || []).map((o) => h('label', { class: 'check' },
          h('input', { type: 'radio', name, value: o.value, checked: String(value ?? def.default ?? '') === o.value, onchange: changed }), o.label));
        const el = h('div', { class: 'choice-list', role: 'radiogroup', id }, opts);
        return { id, el, get: () => el.querySelector('input:checked')?.value ?? '' };
      }
      const el = h('select', { id, 'aria-labelledby': `l-${id}`, onchange: changed },
        def.required ? null : h('option', { value: '' }, '—'),
        (def.options || []).map((o) => h('option', { value: o.value, selected: String(value ?? def.default ?? '') === o.value }, o.label)));
      return { id, el, get: () => el.value };
    }
    case 'multiselect': case 'checkbox': {
      const cur = new Set(Array.isArray(value) ? value : []);
      const el = h('div', { class: 'choice-list', id }, (def.options || []).map((o) => h('label', { class: 'check' },
        h('input', { type: 'checkbox', value: o.value, checked: cur.has(o.value), onchange: changed }), o.label)));
      return { id, el, get: () => [...el.querySelectorAll('input:checked')].map((x) => x.value) };
    }
    case 'boolean': {
      const cb = h('input', { type: 'checkbox', id, checked: value ?? def.default ?? false, onchange: changed });
      return { id, el: h('label', { class: 'check' }, cb, def.label || human(def.name)), get: () => cb.checked };
    }
    case 'date': {
      const el = input({ type: 'date', value: value ?? '' });
      return { id, el, get: () => el.value };
    }
    case 'time': {
      const el = input({ type: 'time', value: value ?? '' });
      return { id, el, get: () => el.value };
    }
    case 'datetime': {
      const local = value ? toLocalInput(value) : '';
      const el = input({ type: 'datetime-local', value: local });
      return { id, el, get: () => (el.value ? new Date(el.value).toISOString().replace('.000Z', 'Z') : '') };
    }
    case 'image': case 'file': return mediaField(def, value ? [value] : [], false, id, changed);
    case 'gallery': return mediaField(def, Array.isArray(value) ? value : [], true, id, changed);
    case 'entry': case 'entries': return entryField(def, value, id, changed, ctx);
    case 'term': case 'terms': return termField(def, value, id, changed, ctx);
    case 'user': {
      const el = h('select', { id, 'aria-labelledby': `l-${id}`, onchange: changed }, h('option', { value: '' }, '—'),
        (ctx.authors || []).map((a) => h('option', { value: a.id, selected: a.id === value }, a.label)));
      return { id, el, get: () => el.value };
    }
    case 'link': {
      const v = value || {};
      const url = h('input', { type: 'text', value: v.url || '', placeholder: 'https:// or /page', 'aria-label': 'Link address', oninput: changed });
      const label = h('input', { type: 'text', value: v.label || '', placeholder: 'Link text', 'aria-label': 'Link text', oninput: changed });
      const blank = h('input', { type: 'checkbox', checked: v.target === '_blank', onchange: changed });
      return { id, el: h('div', { class: 'link-field', id }, url, label, h('label', { class: 'check' }, blank, 'New tab')),
        get: () => (url.value.trim() ? { url: url.value.trim(), label: label.value.trim(), ...(blank.checked ? { target: '_blank' } : {}) } : null) };
    }
    case 'group': {
      const obj = renderObject(def.fields, value, ctx);
      return { id, el: h('div', { class: 'group-box', id }, obj.el), get: () => obj.get(), setErrors: (e, p) => obj.setErrors(e, p), text: obj.firstText };
    }
    case 'repeater': return repeaterField(def, value, id, ctx, changed);
    case 'keyvalue': return keyValueField(value, id, changed);
    case 'message': return { id, el: h('p', { class: 'hint', id }, def.help || def.label || ''), get: () => undefined };
  }
  const el = input({ type: 'text', value: value ?? '' });
  return { id, el, get: () => el.value };
}

function toLocalInput(iso) {
  const d = new Date(iso);
  if (isNaN(d)) return '';
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}

function mediaField(def, ids, multiple, id, changed) {
  let cur = [...ids];
  const box = h('div', { class: 'media-field', id });
  const kind = def.type === 'file' ? '' : 'image/';
  const render = () => {
    fill(box, 
      ...cur.map((mid, i) => {
        const m = mediaCache.get(mid);
        return h('div', { class: `media-chip${!multiple && kind ? ' media-wide' : ''}` }, mediaThumb(m, !multiple),
          h('div', { class: 'media-chip-name' }, m ? m.filename : 'Unknown file'),
          iconButton('x', 'Remove', () => { cur.splice(i, 1); render(); changed(); }));
      }),
      (multiple || !cur.length) ? h('button', { type: 'button', class: 'btn', onclick: async () => {
        const picked = await pickMedia({ multiple, kind });
        if (!picked.length) return;
        cur = multiple ? [...cur, ...picked.map((p) => p.id).filter((x) => !cur.includes(x))] : [picked[0].id];
        render(); changed();
      } }, icon(kind ? 'image' : 'file'), multiple ? 'Add images' : kind ? 'Choose image' : 'Choose file') : null,
      !multiple && cur.length ? h('button', { type: 'button', class: 'btn btn-ghost btn-sm', onclick: async () => {
        const picked = await pickMedia({ multiple: false, kind });
        if (picked.length) { cur = [picked[0].id]; render(); changed(); }
      } }, 'Replace') : null,
    );
  };
  render();
  return { id, el: box, get: () => (multiple ? cur : cur[0] || '') };
}

function entryField(def, value, id, changed, ctx) {
  const multiple = def.type === 'entries';
  let cur = multiple ? (Array.isArray(value) ? value.map((v) => ({ id: v, label: ctx.labels?.[v] || v.slice(0, 8) })) : [])
    : value ? [{ id: value, label: ctx.labels?.[value] || value.slice(0, 8) }] : [];
  const box = h('div', { class: 'ref-list', id });
  const render = () => {
    fill(box, 
      ...cur.map((e, i) => h('div', { class: 'ref-item' }, icon('file'), h('span', {}, e.label),
        iconButton('x', 'Remove', () => { cur.splice(i, 1); render(); changed(); }))),
      (multiple || !cur.length) ? h('button', { type: 'button', class: 'btn btn-sm', onclick: async () => {
        const e = await pickEntry(def.types);
        if (!e || cur.some((c) => c.id === e.id)) return;
        cur = multiple ? [...cur, e] : [e];
        render(); changed();
      } }, icon('plus'), multiple ? 'Add entry' : 'Choose entry') : null);
  };
  render();
  return { id, el: box, get: () => (multiple ? cur.map((c) => c.id) : cur[0]?.id || '') };
}

function termField(def, value, id, changed, ctx) {
  const tax = (ctx.taxonomies || []).find((t) => t.name === def.taxonomy);
  const terms = tax ? tax.terms || [] : [];
  if (def.type === 'term') {
    const el = h('select', { id, onchange: changed }, h('option', { value: '' }, '—'), terms.map((t) => h('option', { value: t.id, selected: t.id === value }, t.label)));
    return { id, el, get: () => el.value };
  }
  const cur = new Set(Array.isArray(value) ? value : []);
  const el = h('div', { class: 'choice-list', id }, terms.map((t) => h('label', { class: 'check' }, h('input', { type: 'checkbox', value: t.id, checked: cur.has(t.id), onchange: changed }), t.label)));
  return { id, el, get: () => [...el.querySelectorAll('input:checked')].map((x) => x.value) };
}

function repeaterField(def, value, id, ctx, changed) {
  const rows = [];
  const list = h('div', { class: 'repeater', id });
  const addBtn = h('button', { type: 'button', class: 'btn btn-sm', onclick: () => { add({}); changed(); } }, icon('plus'), 'Add ' + human(singular(def.label || def.name)).toLowerCase());
  const renumber = () => rows.forEach((r, i) => { r.title.textContent = `${human(singular(def.label || def.name))} ${i + 1}`; });
  const add = (v) => {
    const obj = renderObject(def.fields, v, ctx);
    const title = h('span', {});
    const row = { obj, title };
    const el = h('div', { class: 'repeater-item' },
      h('div', { class: 'repeater-head' }, title,
        iconButton('chevron-up', 'Move up', () => move(row, -1)),
        iconButton('chevron-down', 'Move down', () => move(row, 1)),
        iconButton('trash', 'Remove', () => { const i = rows.indexOf(row); rows.splice(i, 1); el.remove(); renumber(); changed(); }, 'danger')),
      h('div', { class: 'repeater-body' }, obj.el));
    row.el = el;
    rows.push(row);
    list.insertBefore(el, addBtn);
    renumber();
  };
  const move = (row, d) => {
    const i = rows.indexOf(row), j = i + d;
    if (j < 0 || j >= rows.length) return;
    rows.splice(i, 1); rows.splice(j, 0, row);
    rows.forEach((r) => list.insertBefore(r.el, addBtn));
    renumber(); changed();
  };
  list.append(addBtn);
  (Array.isArray(value) ? value : []).forEach(add);
  return {
    id, el: list,
    get: () => rows.map((r) => r.obj.get()),
    setErrors: (e, p) => rows.forEach((r, i) => r.obj.setErrors(e, `${p}${i}.`)),
    text: () => rows[0]?.obj.firstText() || '',
  };
}

function keyValueField(value, id, changed) {
  const box = h('div', { class: 'stack', id });
  const rows = [];
  const add = (k = '', v = '') => {
    const key = h('input', { type: 'text', value: k, placeholder: 'Key', 'aria-label': 'Key', oninput: changed });
    const val = h('input', { type: 'text', value: v, placeholder: 'Value', 'aria-label': 'Value', oninput: changed });
    const row = h('div', { class: 'kv-row' }, key, val, iconButton('x', 'Remove', () => { row.remove(); rows.splice(rows.indexOf(r), 1); changed(); }));
    const r = { key, val };
    rows.push(r);
    box.insertBefore(row, btn);
  };
  const btn = h('button', { type: 'button', class: 'btn btn-sm', onclick: () => add() }, icon('plus'), 'Add row');
  box.append(btn);
  (Array.isArray(value) ? value : []).forEach((p) => add(p.key, p.value));
  return { id, el: box, get: () => rows.filter((r) => r.key.value.trim()).map((r) => ({ key: r.key.value.trim(), value: r.val.value })) };
}

function singular(s) { return s.endsWith('ies') ? s.slice(0, -3) + 'y' : s.endsWith('s') ? s.slice(0, -1) : s; }

export { uid };
