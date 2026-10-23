// Block editor: an ordered (optionally nested) list of schema-driven blocks.
import Sortable from 'sortablejs';
import { h, fill, icon, iconButton, uid } from './ui.js';
import { renderObject } from './fields.js';

const ICONS = { text: 'text', 'rich-text': 'text', heading: 'heading', image: 'image', gallery: 'gallery', embed: 'video', quote: 'quote',
  button: 'button', code: 'code', callout: 'info', columns: 'columns', accordion: 'list', hero: 'layout-top', cta: 'megaphone',
  'card-grid': 'grid', query: 'list-ordered', spacer: 'space', separator: 'minus', html: 'code-xml', table: 'table' };

const CATEGORIES = { text: 'Text', media: 'Media', layout: 'Layout', dynamic: 'Dynamic', advanced: 'Advanced', content: 'Content' };

export function blockEditor(tree, opts) {
  // opts: { defs, allowed (top-level names), ctx, onChange, maxDepth }
  const root = blockList(Array.isArray(tree) ? tree : [], opts.allowed, 1, opts);
  return root;
}

function blockList(items, allowed, depth, opts) {
  const blocks = [];
  const list = h('div', { class: 'blocks' });
  const insertWrap = h('div', { class: 'inserter' });
  const container = h('div', { class: 'blocks-wrap' }, list, insertWrap);
  const changed = () => opts.onChange && opts.onChange();

  const sortable = Sortable.create(list, {
    handle: '.block-grip', animation: 150, ghostClass: 'sortable-ghost',
    onEnd: () => {
      const order = [...list.children];
      blocks.sort((a, b) => order.indexOf(a.el) - order.indexOf(b.el));
      changed();
    },
  });

  const add = (b, after, open = true) => {
    const def = opts.defs[b.type];
    const blk = renderBlock(b, def, depth, opts, { remove, duplicate, move, insertAfter: (x) => showInserter(x) });
    if (open) blk.el.classList.add('is-open');
    if (after) {
      const i = blocks.indexOf(after);
      blocks.splice(i + 1, 0, blk);
      after.el.after(blk.el);
    } else {
      blocks.push(blk);
      list.append(blk.el);
    }
    return blk;
  };
  const remove = (blk) => {
    const i = blocks.indexOf(blk);
    if (i < 0) return;
    blocks.splice(i, 1);
    blk.el.remove();
    changed();
  };
  const duplicate = (blk) => {
    const copy = JSON.parse(JSON.stringify(blk.get()));
    const reid = (x) => { x.id = uid(); (x.children || []).forEach(reid); };
    reid(copy);
    add(copy, blk);
    changed();
  };
  const move = (blk, d) => {
    const i = blocks.indexOf(blk), j = i + d;
    if (j < 0 || j >= blocks.length) return;
    blocks.splice(i, 1); blocks.splice(j, 0, blk);
    blocks.forEach((x) => list.append(x.el));
    blk.el.querySelector('.block-label')?.focus();
    changed();
  };

  // Inserter: searchable, grouped by category.
  let pop = null;
  const closePop = () => { pop?.remove(); pop = null; document.removeEventListener('mousedown', outside); };
  const outside = (e) => { if (pop && !pop.contains(e.target) && !insertBtn.contains(e.target)) closePop(); };
  const showInserter = (after) => {
    closePop();
    const names = (allowed && allowed.length ? allowed : Object.keys(opts.defs))
      .filter((n) => opts.defs[n] && (opts.ctx.can_raw_html || !(opts.defs[n].fields || []).some((f) => f.type === 'html')));
    const q = h('input', { type: 'search', placeholder: 'Search blocks…', 'aria-label': 'Search blocks' });
    const body = h('div', {});
    const draw = () => {
      const term = q.value.toLowerCase();
      const groups = {};
      for (const n of names) {
        const d = opts.defs[n];
        if (term && !(d.label || n).toLowerCase().includes(term)) continue;
        (groups[d.category || 'content'] ||= []).push(n);
      }
      fill(body, ...Object.entries(groups).map(([cat, ns]) => [
        h('p', { class: 'inserter-cat' }, CATEGORIES[cat] || cat),
        h('div', { class: 'inserter-grid' }, ns.map((n) => h('button', { type: 'button', class: 'inserter-item', onclick: () => {
          const blk = add({ id: uid(), type: n, data: {} }, after);
          closePop(); changed();
          requestAnimationFrame(() => blk.focus());
        } }, icon(opts.defs[n].icon && ICONS[n] ? ICONS[n] : ICONS[n] || 'square'), opts.defs[n].label || n))),
      ]));
    };
    q.addEventListener('input', draw);
    q.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') closePop();
      if (e.key === 'Enter') { e.preventDefault(); body.querySelector('.inserter-item')?.click(); }
    });
    draw();
    pop = h('div', { class: 'inserter-pop' }, q, body);
    (after ? after.el : insertWrap).append(pop);
    if (after) pop.style.position = 'relative';
    setTimeout(() => { q.focus(); document.addEventListener('mousedown', outside); }, 0);
  };
  const insertBtn = h('button', { type: 'button', class: 'inserter-btn', onclick: () => (pop ? closePop() : showInserter(null)) },
    icon('plus'), depth > 1 ? 'Add inside' : 'Add block');
  insertWrap.append(insertBtn);

  items.forEach((b) => add(b, null, items.length <= 8 || b.type === 'rich-text'));
  return {
    el: container,
    get: () => blocks.map((b) => b.get()),
    setErrors(errs, prefix) {
      blocks.forEach((b, i) => b.setErrors(errs, `${prefix}${i}.`));
    },
    destroy: () => sortable.destroy(),
    get count() { return blocks.length; },
  };
}

function renderBlock(b, def, depth, opts, ops) {
  const changed = () => opts.onChange && opts.onChange();
  const isRT = b.type === 'rich-text';
  if (!def) {
    // Unknown block (definition removed): keep it untouched.
    const el = h('div', { class: 'block is-open has-error' }, h('div', { class: 'block-body' },
      h('p', { class: 'error-text' }, `This “${b.type}” block no longer exists in the site bundle. It will be kept but not shown.`)));
    return { el, get: () => b, setErrors() {}, focus() {} };
  }
  const settings = { ...(b.settings || {}) };
  const fields = renderObject(def.fields, b.data, { ...opts.ctx, onChange: () => { changed(); updateSummary(); } });
  const summary = h('span', { class: 'summary' });
  const updateSummary = () => { summary.textContent = el.classList.contains('is-open') ? '' : fields.firstText().slice(0, 80); };
  const label = h('button', { type: 'button', class: 'block-label', 'aria-expanded': 'true',
    onclick: () => { if (isRT) return; el.classList.toggle('is-open'); label.setAttribute('aria-expanded', el.classList.contains('is-open')); updateSummary(); } },
    icon(ICONS[b.type] || 'square'), def.label || b.type, summary);
  let children = null;
  if (def.children && depth < (opts.maxDepth || 4)) {
    children = blockList(b.children || [], def.children.allowed, depth + 1, opts);
  }
  const anchor = h('input', { type: 'text', value: settings.anchor || '', placeholder: 'e.g. pricing', oninput: changed });
  const cls = h('input', { type: 'text', value: settings.class || '', placeholder: 'CSS classes', oninput: changed });
  const hidden = h('input', { type: 'checkbox', checked: !!settings.hidden, onchange: changed });
  const settingsBox = h('div', { class: 'block-settings', hidden: true },
    h('label', { class: 'field' }, h('span', { class: 'label' }, 'Anchor'), anchor),
    h('label', { class: 'field' }, h('span', { class: 'label' }, 'CSS class'), cls),
    h('label', { class: 'check' }, hidden, 'Hide this block'));
  const errBox = h('div', { class: 'error-text' });
  const el = h('section', { class: `block${isRT ? ' is-rt is-open' : ''}`, 'aria-label': def.label || b.type },
    h('div', { class: 'block-head' },
      h('span', { class: 'block-grip', title: 'Drag to reorder', 'aria-hidden': 'true' }, icon('grip')),
      label,
      iconButton('chevron-up', 'Move up', () => ops.move(blk, -1)),
      iconButton('chevron-down', 'Move down', () => ops.move(blk, 1)),
      iconButton('plus', 'Add a block below', () => ops.insertAfter(blk)),
      iconButton('copy', 'Duplicate', () => ops.duplicate(blk)),
      iconButton('settings-2', 'Block settings', () => { settingsBox.hidden = !settingsBox.hidden; el.classList.add('is-open'); }),
      iconButton('trash', 'Remove block', () => { if (isEmpty(fields.get()) || confirm(`Remove this ${def.label || b.type} block?`)) ops.remove(blk); }, 'danger')),
    h('div', { class: 'block-body' }, fields.el, children ? h('div', { class: 'block-children' }, children.el) : null, settingsBox, errBox));
  const blk = {
    el,
    get: () => {
      const s = {};
      if (anchor.value.trim()) s.anchor = anchor.value.trim();
      if (cls.value.trim()) s.class = cls.value.trim();
      if (hidden.checked) s.hidden = true;
      if (settings.from) s.from = settings.from;
      if (settings.until) s.until = settings.until;
      if (settings.logged_in) s.logged_in = true;
      const out = { id: b.id || uid(), type: b.type, data: fields.get() };
      if (Object.keys(s).length) out.settings = s;
      if (children) out.children = children.get();
      return out;
    },
    setErrors(errs, prefix) {
      fields.setErrors(errs, prefix + 'data.');
      const own = errs[prefix.slice(0, -1)];
      errBox.textContent = own || '';
      const has = Object.keys(errs).some((k) => k.startsWith(prefix));
      el.classList.toggle('has-error', has);
      if (has) el.classList.add('is-open');
      if (children) children.setErrors(errs, prefix + 'children.');
    },
    focus() {
      const f = el.querySelector('.ProseMirror, input:not([type=checkbox]), textarea, select');
      if (f) f.focus(); else label.focus();
    },
  };
  return blk;
}

function isEmpty(o) {
  return !o || Object.values(o).every((v) => v == null || v === '' || (Array.isArray(v) && !v.length));
}
