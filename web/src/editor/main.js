// Entry editor. Reads #editor-data, renders the working copy, autosaves it,
// and publishes through the same services as the CLI and API.
import { h, fill, icon, iconButton, api, toast, debounce, autosize, slugify } from './ui.js';
import { renderObject } from './fields.js';
import { blockEditor } from './blocks.js';
import { cacheMedia, pickMedia, mediaCache, mediaThumb } from './pickers.js';
import { mountMenuEditor } from './menu.js';
import { mountOptionsEditor } from './options.js';

const dataEl = document.getElementById('editor-data');
if (dataEl) mountEditor(JSON.parse(dataEl.textContent));
if (document.getElementById('menu-data')) mountMenuEditor();
if (document.getElementById('options-data')) mountOptionsEditor();

function mountEditor(D) {
  window.__csrf = D.csrf;
  cacheMedia(Object.values(D.media || {}));
  const T = D.type;
  const has = (f) => (T.supports || []).includes(f);
  let entry = D.entry;
  let liveURL = D.live_url;
  let dirty = false;
  let saving = null;
  // The slug follows the title until it is edited by hand or the entry is published.
  let slugTouched = entry.published_version != null || (!!entry.slug && entry.slug !== slugify(entry.title) && !/^untitled(-\d+)?$/.test(entry.slug));
  const root = document.getElementById('editor');
  const ctx = {
    authors: D.authors, taxonomies: D.taxonomies, can_raw_html: D.can_raw_html,
    can: (c) => c !== 'content.raw_html' || D.can_raw_html,
    onChange: () => markDirty(),
  };

  // --- Top bar
  const statusText = h('span', { class: 'ed-status', role: 'status', 'aria-live': 'polite' });
  const saveBtn = h('button', { type: 'button', class: 'btn', onclick: () => save(false) }, 'Save draft');
  const publishBtn = h('button', { type: 'button', class: 'btn btn-primary', onclick: () => publish() }, 'Publish');
  const previewBtn = h('button', { type: 'button', class: 'btn btn-ghost', onclick: () => openPreview() }, icon('eye'), 'Preview');
  const conflictBar = h('div', { class: 'conflict', hidden: true });
  const top = h('div', { class: 'ed-top' },
    h('a', { class: 'ed-back', href: `/admin/content/${T.name}/` }, icon('chevron-up'), T.label.plural),
    statusText,
    h('div', { class: 'ed-actions' }, previewBtn, saveBtn, D.can_publish ? publishBtn : null));
  top.querySelector('.ed-back .icon').style.transform = 'rotate(-90deg)';

  // --- Canvas: title, slug, fields, blocks
  const title = h('textarea', { class: 'ed-title', rows: 1, placeholder: `${T.label.singular} title`, 'aria-label': 'Title', value: entry.title,
    oninput: () => { if (!slugTouched) slug.value = slugify(title.value); markDirty(); } });
  title.value = entry.title;
  autosize(title);
  title.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); root.querySelector('.ProseMirror')?.focus(); } });
  const slug = h('input', { type: 'text', value: entry.slug, 'aria-label': 'URL slug', spellcheck: 'false',
    oninput: () => { slugTouched = true; markDirty(); }, onblur: () => { slug.value = slugify(slug.value) || slug.value; } });
  const urlPrefix = (T.url || '').split('{')[0];
  const slugRow = T.url ? h('div', { class: 'ed-slug' }, icon('link'), h('span', {}, D.site_url.replace(/^https?:\/\//, '') + urlPrefix), slug, h('span', {}, '/')) : null;
  const titleErr = h('div', { class: 'error-text' });

  const fieldsMain = renderObject(T.fields || [], entry.fields, ctx);
  const groups = (D.groups || []).map((g) => ({ g, obj: renderObject(g.fields, entry.fields, ctx) }));
  const blocks = has('blocks') ? blockEditor(entry.blocks, { defs: D.blocks, allowed: T.blocks?.allowed, ctx, onChange: () => markDirty(), maxDepth: T.blocks?.max_depth || 4 }) : null;

  const doc = h('div', { class: 'ed-doc' }, title, titleErr, slugRow,
    (T.fields || []).length ? h('div', { class: 'fgroup' }, fieldsMain.el) : null,
    groups.filter((x) => x.g.position !== 'sidebar').map((x) => h('section', { class: 'fgroup' }, h('h2', { class: 'fgroup-title' }, x.g.label), x.obj.el)),
    blocks ? blocks.el : null);

  // --- Sidebar
  const side = h('aside', { class: 'ed-side', 'aria-label': 'Entry settings' });
  const publishCard = h('section', { class: 'ed-card' });
  side.append(publishCard);

  let excerpt = null, featured = null;
  if (has('excerpt') || has('featured_image')) {
    const body = h('div', { class: 'ed-card-body' });
    if (has('excerpt')) {
      excerpt = h('textarea', { rows: 3, maxlength: 1000, placeholder: 'A short summary for lists, search and social sharing.', oninput: () => { markDirty(); updateSERP(); } }, entry.meta.excerpt || '');
      body.append(h('label', { class: 'field' }, h('span', { class: 'label' }, 'Excerpt'), excerpt));
    }
    if (has('featured_image')) {
      featured = mediaSlot(entry.meta.featured_image, 'Featured image');
      body.append(featured.el);
    }
    side.append(h('details', { class: 'ed-card', open: true }, h('summary', {}, 'Summary'), body));
  }

  const termBoxes = {};
  for (const tx of D.taxonomies || []) {
    const cur = new Set((entry.meta.terms || {})[tx.name] || []);
    const list = h('div', { class: 'term-list' });
    const draw = () => {
      const byParent = {};
      for (const t of tx.terms || []) (byParent[t.parent || ''] ||= []).push(t);
      const rows = [];
      const walk = (p, d) => (byParent[p] || []).forEach((t) => {
        rows.push(h('label', { class: `check${d ? ' child' : ''}` }, h('input', { type: 'checkbox', value: t.id, checked: cur.has(t.id),
          onchange: (e) => { e.target.checked ? cur.add(t.id) : cur.delete(t.id); markDirty(); } }), t.label));
        walk(t.id, d + 1);
      });
      walk('', 0);
      fill(list, ...rows);
      if (!rows.length) list.append(h('p', { class: 'hint' }, `No ${tx.label.plural.toLowerCase()} yet.`));
    };
    draw();
    const newIn = h('input', { type: 'text', placeholder: `New ${tx.label.singular.toLowerCase()}`, 'aria-label': `New ${tx.label.singular}` });
    const addTerm = async () => {
      const name = newIn.value.trim();
      if (!name) return;
      try {
        const t = await api('POST', '/admin/api/terms', { taxonomy: tx.name, name });
        tx.terms = [...(tx.terms || []), { id: t.id, label: t.label }];
        cur.add(t.id); newIn.value = ''; draw(); markDirty();
      } catch (e) { toast(e.message, 'error'); }
    };
    newIn.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); addTerm(); } });
    termBoxes[tx.name] = cur;
    side.append(h('details', { class: 'ed-card', open: true }, h('summary', {}, tx.label.plural),
      h('div', { class: 'ed-card-body' }, list, h('div', { class: 'term-add' }, newIn, h('button', { type: 'button', class: 'btn btn-sm', onclick: addTerm }, 'Add')))));
  }

  let parentSel = null;
  if (T.hierarchical) {
    parentSel = h('select', { onchange: markDirty }, h('option', { value: '' }, 'None (top level)'),
      (D.parents || []).map((p) => h('option', { value: p.id, selected: p.id === entry.meta.parent_id }, p.label)));
    side.append(h('details', { class: 'ed-card', open: true }, h('summary', {}, 'Page attributes'),
      h('div', { class: 'ed-card-body' }, h('label', { class: 'field' }, h('span', { class: 'label' }, 'Parent'), parentSel))));
  }

  let authorSel = null;
  if (has('author') || !T.hierarchical) {
    authorSel = h('select', { onchange: markDirty }, h('option', { value: '' }, '—'),
      (D.authors || []).map((a) => h('option', { value: a.id, selected: a.id === entry.author_id }, a.label)));
    side.append(h('details', { class: 'ed-card' }, h('summary', {}, 'Author'),
      h('div', { class: 'ed-card-body' }, h('label', { class: 'field' }, h('span', { class: 'label' }, 'Shown as the author'), authorSel))));
  }

  for (const x of groups.filter((x) => x.g.position === 'sidebar')) {
    side.append(h('details', { class: 'ed-card', open: true }, h('summary', {}, x.g.label), h('div', { class: 'ed-card-body' }, x.obj.el)));
  }

  // SEO
  const seo = entry.meta.seo || {};
  const seoTitle = h('input', { type: 'text', value: seo.title || '', placeholder: 'Defaults to the title', oninput: () => { markDirty(); updateSERP(); } });
  const seoDesc = h('textarea', { rows: 3, placeholder: 'Defaults to the excerpt', oninput: () => { markDirty(); updateSERP(); } }, seo.description || '');
  const seoCanon = h('input', { type: 'text', value: seo.canonical || '', placeholder: 'Only if this content lives elsewhere', oninput: markDirty });
  const seoNoindex = h('input', { type: 'checkbox', checked: !!seo.noindex, onchange: markDirty });
  const ogImg = mediaSlot(seo.og_image, 'Social image');
  const counterT = h('span', { class: 'counter' }), counterD = h('span', { class: 'counter' });
  const serp = h('div', { class: 'serp', 'aria-hidden': 'true' });
  const updateSERP = () => {
    const t = seoTitle.value || title.value || 'Untitled';
    const d = seoDesc.value || (excerpt ? excerpt.value : '') || '';
    counterT.textContent = `${t.length} / 60`; counterT.classList.toggle('over', t.length > 60);
    counterD.textContent = `${d.length} / 160`; counterD.classList.toggle('over', d.length > 160);
    fill(serp, h('div', { class: 'serp-url' }, D.site_url.replace(/^https?:\/\//, '') + (T.url ? urlPrefix + slug.value + '/' : '')),
      h('div', { class: 'serp-title' }, t.length > 62 ? t.slice(0, 60) + '…' : t),
      h('div', { class: 'serp-desc' }, d ? (d.length > 162 ? d.slice(0, 160) + '…' : d) : 'Add an excerpt or description to control this text.'));
  };
  title.addEventListener('input', updateSERP);
  slug.addEventListener('input', updateSERP);
  side.append(h('details', { class: 'ed-card' }, h('summary', {}, 'Search & social'), h('div', { class: 'ed-card-body' }, serp,
    h('label', { class: 'field' }, h('span', { class: 'label' }, 'SEO title'), seoTitle, counterT),
    h('label', { class: 'field' }, h('span', { class: 'label' }, 'Meta description'), seoDesc, counterD),
    ogImg.el,
    h('label', { class: 'field' }, h('span', { class: 'label' }, 'Canonical URL'), seoCanon),
    h('label', { class: 'check' }, seoNoindex, 'Hide from search engines'))));
  updateSERP();

  // Revisions
  const revList = h('div', { class: 'stack' }, h('p', { class: 'hint' }, 'Open to load the history.'));
  const revCard = h('details', { class: 'ed-card' }, h('summary', {}, 'History'), h('div', { class: 'ed-card-body' }, revList));
  revCard.addEventListener('toggle', () => { if (revCard.open) loadRevisions(); });
  side.append(revCard);

  fill(root, h('div', { class: 'ed' }, top, conflictBar, h('div', { class: 'ed-canvas' }, doc), side));
  if (!entry.title) title.focus();

  function mediaSlot(id, labelText) {
    let cur = id || '';
    const box = h('div', { class: 'field' }, h('span', { class: 'label' }, labelText));
    const inner = h('div', { class: 'media-field' });
    box.append(inner);
    const draw = () => {
      fill(inner, cur ? h('div', { class: 'media-chip media-wide' }, mediaThumb(mediaCache.get(cur), true),
        iconButton('x', 'Remove', () => { cur = ''; draw(); markDirty(); })) : null,
      h('button', { type: 'button', class: 'btn btn-sm', onclick: async () => {
        const p = await pickMedia({ multiple: false, kind: 'image/' });
        if (p.length) { cur = p[0].id; draw(); markDirty(); }
      } }, icon('image'), cur ? 'Replace' : 'Choose image'));
    };
    draw();
    return { el: box, get: () => cur };
  }

  // --- State and saving
  function collect(autosave) {
    const fields = { ...fieldsMain.get() };
    for (const x of groups) Object.assign(fields, x.obj.get());
    const terms = {};
    for (const [k, v] of Object.entries(termBoxes)) if (v.size) terms[k] = [...v];
    return {
      version: entry.version, title: title.value.trim(), slug: slug.value.trim(), autosave,
      meta: {
        excerpt: excerpt ? excerpt.value.trim() : entry.meta.excerpt,
        featured_image: featured ? featured.get() : entry.meta.featured_image,
        parent_id: parentSel ? parentSel.value : '',
        terms,
        template: entry.meta.template || '',
        seo: { title: seoTitle.value.trim(), description: seoDesc.value.trim(), canonical: seoCanon.value.trim(), noindex: seoNoindex.checked, og_image: ogImg.get() },
      },
      fields,
      blocks: blocks ? blocks.get() : [],
      author_id: authorSel && authorSel.value ? authorSel.value : null,
    };
  }

  const autosave = debounce(() => save(true), 1500);
  function markDirty() {
    dirty = true;
    setStatus('Unsaved changes', '');
    autosave();
  }

  function setStatus(text, cls) {
    statusText.className = `ed-status ${cls}`;
    statusText.textContent = text;
  }

  async function save(isAuto) {
    autosave.cancel();
    if (saving) { await saving; if (!dirty && isAuto) return; }
    const body = collect(isAuto);
    setStatus('Saving…', 'ed-saving');
    saving = (async () => {
      try {
        const res = await api('POST', `/admin/api/entries/${entry.id}/save`, body);
        entry = res.entry;
        dirty = false;
        clearErrors();
        setStatus(isAuto ? 'Draft saved' : 'Saved', 'ed-saved');
        renderPublishCard();
        if (!isAuto) toast('Saved');
        return true;
      } catch (e) {
        if (e.status === 409) { showConflict(e.data.current_version); setStatus('Not saved', 'ed-error-dot'); return false; }
        if (e.status === 422) {
          if (isAuto) { setStatus('Draft saved with issues', 'ed-saving'); return false; }
          showErrors(e.data.fields || {});
          setStatus('Fix the highlighted fields', 'ed-error-dot');
          return false;
        }
        setStatus(e.offline ? 'Offline: will retry' : 'Not saved', 'ed-error-dot');
        if (e.offline) setTimeout(() => dirty && save(true), 5000);
        else toast(e.message, 'error');
        return false;
      } finally {
        saving = null;
      }
    })();
    return saving;
  }

  async function publish() {
    if (dirty || saving) { const ok = await save(false); if (!ok) return; }
    publishBtn.disabled = true;
    try {
      const res = await api('POST', `/admin/api/entries/${entry.id}/action/publish`, { version: entry.version });
      entry = res.entry;
      liveURL = res.live_url;
      clearErrors();
      renderPublishCard();
      setStatus('Published', 'ed-saved');
      toast(entry.published_version ? 'Published. The site updates in a moment.' : 'Published');
    } catch (e) {
      if (e.status === 422) { showErrors(e.data.fields || {}); toast('Some fields need attention before publishing.', 'error'); }
      else if (e.status === 409) showConflict(e.data.current_version);
      else toast(e.message, 'error');
    } finally {
      publishBtn.disabled = false;
    }
  }

  async function action(name, body = {}, confirmText) {
    if (confirmText && !confirm(confirmText)) return;
    if (dirty) { const ok = await save(false); if (!ok) return; }
    try {
      const res = await api('POST', `/admin/api/entries/${entry.id}/action/${name}`, { version: entry.version, ...body });
      if (res.redirect) { location.href = res.redirect; return; }
      if (name === 'trash') { location.href = `/admin/content/${T.name}/?ok=deleted`; return; }
      if (res.entry) {
        const reload = name === 'discard';
        entry = res.entry;
        liveURL = res.live_url ?? (entry.published_version ? liveURL : '');
        if (reload) { location.reload(); return; }
      }
      renderPublishCard();
      toast('Done');
    } catch (e) {
      if (e.status === 422) showErrors(e.data.fields || {});
      toast(e.message, 'error');
    }
  }

  function renderPublishCard() {
    const published = entry.published_version != null;
    const pending = published && entry.published_version !== entry.version;
    const scheduled = !!entry.scheduled_at;
    const badge = entry.status === 'trashed' ? 'trashed' : scheduled ? 'scheduled' : pending ? 'pending' : entry.status;
    publishBtn.textContent = published ? (pending ? 'Update' : 'Published') : 'Publish';
    publishBtn.disabled = published && !pending && !dirty;
    const when = h('input', { type: 'datetime-local', 'aria-label': 'Publish at', min: new Date(Date.now() + 60000).toISOString().slice(0, 16) });
    fill(publishCard, 
      h('div', { class: 'ed-card-head' }, 'Status', h('span', { class: `badge badge-${badge}` }, badge === 'pending' ? 'Unpublished changes' : badge.replace(/^./, (c) => c.toUpperCase()))),
      h('div', { class: 'ed-card-body' },
        h('dl', { class: 'ed-meta' },
          published ? [h('dt', {}, 'Published'), h('dd', {}, fmt(entry.published_at))] : null,
          scheduled ? [h('dt', {}, 'Scheduled'), h('dd', {}, fmt(entry.scheduled_at))] : null,
          entry.unpublish_at ? [h('dt', {}, 'Unpublishes'), h('dd', {}, fmt(entry.unpublish_at))] : null,
          h('dt', {}, 'Last saved'), h('dd', {}, fmt(entry.updated_at))),
        liveURL ? h('a', { class: 'btn btn-ghost btn-sm', href: D.site_url + liveURL, target: '_blank', rel: 'noopener' }, icon('external'), 'View live') : null,
        D.can_publish && !scheduled ? h('details', {}, h('summary', { class: 'hint' }, published ? 'Schedule this update' : 'Schedule for later'),
          h('div', { class: 'row' }, h('div', { class: 'grow' }, when), h('button', { type: 'button', class: 'btn btn-sm', onclick: () => {
            if (!when.value) { toast('Choose a date and time', 'error'); return; }
            action('schedule', { at: new Date(when.value).toISOString() });
          } }, icon('clock'), 'Schedule'))) : null,
        D.can_publish && scheduled ? h('button', { type: 'button', class: 'btn btn-sm', onclick: () => action('unschedule') }, 'Cancel schedule') : null,
        h('div', { class: 'row' },
          pending ? h('button', { type: 'button', class: 'btn btn-ghost btn-sm', onclick: () => action('discard', {}, 'Throw away your changes and go back to the published version?') }, 'Discard changes') : null,
          D.can_publish && published ? h('button', { type: 'button', class: 'btn btn-ghost btn-sm', onclick: () => action('unpublish', {}, 'Take this off the site? It stays here as a draft.') }, 'Unpublish') : null,
          h('button', { type: 'button', class: 'btn btn-ghost btn-sm', onclick: () => action('clone') }, 'Duplicate'),
          h('button', { type: 'button', class: 'btn btn-ghost btn-sm btn-danger-text', onclick: () => action('trash', {}, 'Move this to the trash?') }, 'Trash'))));
  }
  renderPublishCard();

  function fmt(s) {
    if (!s) return '—';
    const d = new Date(s);
    return d.toLocaleString(undefined, { day: 'numeric', month: 'short', year: 'numeric', hour: '2-digit', minute: '2-digit' });
  }

  function clearErrors() { showErrors({}); conflictBar.hidden = true; }

  function showErrors(errs) {
    titleErr.textContent = errs.title ? 'Title ' + errs.title : '';
    if (errs.slug) titleErr.textContent += (titleErr.textContent ? ' · ' : '') + 'Address ' + errs.slug;
    fieldsMain.setErrors(errs, 'fields.');
    groups.forEach((x) => x.obj.setErrors(errs, 'fields.'));
    if (blocks) blocks.setErrors(errs, 'blocks.');
    const other = Object.entries(errs).filter(([k]) => k.startsWith('meta.') || k === 'blocks');
    if (other.length) toast(other.map(([k, v]) => `${k.replace('meta.', '').replace('seo.', 'SEO ')} ${v}`).join('; '), 'error', 6000);
    const first = root.querySelector('.field-error, .block.has-error');
    if (first && Object.keys(errs).length) first.scrollIntoView({ behavior: 'smooth', block: 'center' });
  }

  function showConflict(current) {
    conflictBar.hidden = false;
    fill(conflictBar, icon('alert'), h('span', {}, 'Someone else saved this entry while you were editing.'),
      h('button', { type: 'button', class: 'btn btn-sm', onclick: () => { dirty = false; location.reload(); } }, 'Load their version'),
      h('button', { type: 'button', class: 'btn btn-sm btn-primary', onclick: () => { entry.version = current; conflictBar.hidden = true; save(false); } }, 'Keep mine'));
  }

  async function loadRevisions() {
    try {
      const res = await api('GET', `/admin/api/entries/${entry.id}/revisions`);
      fill(revList, ...(res.revisions || []).map((r) => h('div', { class: 'row' },
        h('div', { class: 'grow' }, h('div', {}, `#${r.Number} · ${r.Kind}`), h('div', { class: 'hint' }, `${fmt(r.CreatedAt)}${r.AuthorName ? ' · ' + r.AuthorName : ''}`)),
        h('button', { type: 'button', class: 'btn btn-ghost btn-sm', onclick: async () => {
          if (!confirm(`Restore revision #${r.Number} into the editor? Your current draft is kept in the history.`)) return;
          try { await api('POST', `/admin/api/entries/${entry.id}/revisions/${r.ID}/restore`); location.reload(); } catch (e) { toast(e.message, 'error'); }
        } }, 'Restore'))));
      if (!res.revisions?.length) fill(revList, h('p', { class: 'hint' }, 'No saved revisions yet. Autosaves are not kept as revisions.'));
    } catch (e) { fill(revList, h('p', { class: 'error-text' }, e.message)); }
  }

  async function openPreview() {
    if (dirty) await save(true);
    const win = window.open('about:blank', '_blank');
    try {
      const res = await api('GET', `/admin/api/entries/${entry.id}/preview`);
      if (win) win.location.href = res.url; else location.href = res.url;
    } catch (e) { win?.close(); toast(e.message, 'error'); }
  }

  document.addEventListener('keydown', (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 's') { e.preventDefault(); save(false); }
  });
  window.addEventListener('beforeunload', (e) => { if (dirty || saving) { e.preventDefault(); e.returnValue = ''; } });
  setStatus(entry.published_version != null && entry.published_version !== entry.version ? 'Draft ahead of the live version' : '', '');
}
