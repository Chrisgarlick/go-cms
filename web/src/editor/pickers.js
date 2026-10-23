// Media and entry pickers used by fields.
import { h, fill, icon, api, modal, debounce, toast } from './ui.js';

export const mediaCache = new Map();

export function cacheMedia(items) {
  for (const m of items || []) mediaCache.set(m.id, m);
}

export function mediaThumb(m, wide = false) {
  if (!m) return h('span', { class: 'media-placeholder' }, icon('image'), 'Missing');
  if (m.thumb) return h('img', { src: wide && m.url ? m.url : m.thumb, alt: m.alt || '', loading: 'lazy' });
  if (m.is_image) return h('span', { class: 'media-placeholder' }, 'Processing…');
  return h('span', { class: 'media-placeholder' }, icon('file'), m.mime);
}

// pickMedia opens the library; resolves to an array of media items.
export function pickMedia({ multiple = false, kind = 'image/' } = {}) {
  return new Promise((resolve) => {
    const selected = new Map();
    const grid = h('div', { class: 'picker-grid' });
    const search = h('input', { type: 'search', placeholder: 'Search…', 'aria-label': 'Search media' });
    const fileIn = h('input', { type: 'file', multiple: true, hidden: true, accept: kind === 'image/' ? 'image/*' : null });
    const status = h('span', { class: 'muted' });
    const chooseBtn = h('button', { type: 'button', class: 'btn btn-primary', disabled: true, onclick: () => done([...selected.values()]) }, 'Choose');
    let m;
    const done = (items) => { m.close(); resolve(items); };
    const load = async () => {
      const q = new URLSearchParams({ q: search.value, kind });
      try {
        const res = await api('GET', '/admin/api/media?' + q);
        cacheMedia(res.items);
        fill(grid, ...res.items.map(tile));
        if (!res.items.length) grid.append(h('p', { class: 'muted' }, 'Nothing found. Upload a file.'));
      } catch (e) { fill(grid, h('p', { class: 'error-text' }, e.message)); }
    };
    const tile = (it) => {
      const b = h('button', { type: 'button', class: 'picker-item', title: it.filename,
        onclick: () => {
          if (!multiple) { selected.clear(); grid.querySelectorAll('.is-selected').forEach((x) => x.classList.remove('is-selected')); }
          if (selected.has(it.id)) { selected.delete(it.id); b.classList.remove('is-selected'); } else { selected.set(it.id, it); b.classList.add('is-selected'); }
          chooseBtn.disabled = selected.size === 0;
          chooseBtn.textContent = multiple && selected.size ? `Choose ${selected.size}` : 'Choose';
        },
        ondblclick: () => { if (!multiple) done([it]); } }, mediaThumb(it), h('span', {}, it.filename));
      return b;
    };
    const upload = async (files) => {
      for (const f of files) {
        status.textContent = `Uploading ${f.name}…`;
        const fd = new FormData(); fd.append('file', f, f.name);
        try {
          const res = await api('POST', '/admin/api/media', fd);
          (res.errors || []).forEach((er) => toast(er, 'error'));
          cacheMedia(res.items);
          for (const it of res.items || []) { selected.set(it.id, it); }
        } catch (e) { toast(e.message, 'error'); }
      }
      status.textContent = '';
      await load();
      grid.querySelectorAll('.picker-item').forEach((b) => { if ([...selected.values()].some((s) => s.filename === b.title)) b.classList.add('is-selected'); });
      chooseBtn.disabled = selected.size === 0;
    };
    fileIn.addEventListener('change', () => upload([...fileIn.files]));
    search.addEventListener('input', debounce(load, 250));
    const body = h('div', {}, h('div', { class: 'row', style: null }, h('div', { class: 'grow' }, search),
      h('button', { type: 'button', class: 'btn', onclick: () => fileIn.click() }, icon('upload'), 'Upload'), fileIn), h('div', { class: 'pad' }), grid);
    body.addEventListener('dragover', (e) => e.preventDefault());
    body.addEventListener('drop', (e) => { e.preventDefault(); if (e.dataTransfer.files.length) upload([...e.dataTransfer.files]); });
    m = modal(multiple ? 'Choose files' : 'Choose a file', body, [status, h('button', { type: 'button', class: 'btn', onclick: () => done([]) }, 'Cancel'), chooseBtn]);
    load();
  });
}

// pickEntry searches entries of the given types; resolves to {id, label} or null.
export function pickEntry(types) {
  return new Promise((resolve) => {
    const list = h('div', { class: 'ref-list' });
    const search = h('input', { type: 'search', placeholder: 'Search by title…', 'aria-label': 'Search entries' });
    let m;
    const load = async () => {
      const q = new URLSearchParams({ q: search.value });
      (types || []).forEach((t) => q.append('type', t));
      try {
        const res = await api('GET', '/admin/api/entries?' + q);
        fill(list, ...res.entries.map((e) => h('button', { type: 'button', class: 'menu-pop-item',
          onclick: () => { m.close(); resolve({ id: e.ID, label: e.DraftTitle || 'Untitled' }); } },
          e.DraftTitle || 'Untitled', h('span', { class: 'muted' }, ` · ${e.Type}${e.Status === 'published' ? '' : ' (' + e.Status + ')'}`))));
        if (!res.entries.length) list.append(h('p', { class: 'muted' }, 'No matches.'));
      } catch (e) { fill(list, h('p', { class: 'error-text' }, e.message)); }
    };
    search.addEventListener('input', debounce(load, 200));
    m = modal('Choose an entry', h('div', { class: 'stack' }, search, list), [h('button', { type: 'button', class: 'btn', onclick: () => { m.close(); resolve(null); } }, 'Cancel')]);
    load();
  });
}
