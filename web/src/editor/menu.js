// Menu editor: a nested list of items (link, entry, or label), up to 4 levels.
import { h, fill, icon, iconButton, api, toast, uid } from './ui.js';

export function mountMenuEditor() {
  const D = JSON.parse(document.getElementById('menu-data').textContent);
  window.__csrf = document.querySelector('input[name=csrf]')?.value || window.__csrf;
  const root = document.getElementById('menu-editor');
  let items = clone(D.Items || []);

  const draw = () => {
    fill(root, items.length ? tree(items, 0) : h('p', { class: 'muted' }, 'This menu is empty.'),
      h('div', { class: 'row', style: null }, h('button', { type: 'button', class: 'btn', onclick: () => { items.push(newItem()); draw(); } }, icon('plus'), 'Add item')));
  };
  const newItem = () => ({ id: uid(), type: 'url', label: '', url: '/', children: [] });

  const tree = (list, depth) => h('ul', { class: 'menu-tree' }, list.map((it, i) => {
    const type = h('select', { 'aria-label': 'Item type', onchange: (e) => { it.type = e.target.value; draw(); } },
      [['url', 'Link'], ['entry', 'Page or post'], ['label', 'Label only']].map(([v, l]) => h('option', { value: v, selected: it.type === v }, l)));
    const label = h('input', { class: 'mi-label', type: 'text', value: it.label || '', placeholder: 'Label', 'aria-label': 'Label', oninput: (e) => { it.label = e.target.value; } });
    let target = null;
    if (it.type === 'url') target = h('input', { class: 'mi-target', type: 'text', value: it.url || '', placeholder: '/page or https://…', 'aria-label': 'Link', oninput: (e) => { it.url = e.target.value; } });
    if (it.type === 'entry') target = h('select', { class: 'mi-target', 'aria-label': 'Entry', onchange: (e) => {
      it.ref = e.target.value;
      if (!it.label) { it.label = e.target.selectedOptions[0].textContent.replace(/^[^:]+: /, ''); draw(); }
    } }, h('option', { value: '' }, 'Choose…'), (D.Entries || []).map((e) => h('option', { value: e.id, selected: e.id === it.ref }, e.label)));
    const blank = h('label', { class: 'check', title: 'Open in a new tab' }, h('input', { type: 'checkbox', checked: it.target === '_blank', onchange: (e) => { it.target = e.target.checked ? '_blank' : ''; } }), 'New tab');
    return h('li', {},
      h('div', { class: 'mi' }, type, label, target, it.type === 'url' ? blank : null,
        iconButton('chevron-up', 'Move up', () => { if (i > 0) { [list[i - 1], list[i]] = [list[i], list[i - 1]]; draw(); } }),
        iconButton('chevron-down', 'Move down', () => { if (i < list.length - 1) { [list[i + 1], list[i]] = [list[i], list[i + 1]]; draw(); } }),
        depth < 3 && i > 0 ? iconButton('redirect', 'Indent (make a sub-item)', () => { const prev = list[i - 1]; (prev.children ||= []).push(it); list.splice(i, 1); draw(); }) : null,
        iconButton('plus', 'Add a sub-item', () => { (it.children ||= []).push(newItem()); draw(); }),
        iconButton('trash', 'Remove', () => { list.splice(i, 1); draw(); }, 'danger')),
      it.children?.length ? tree(it.children, depth + 1) : null);
  }));

  document.querySelector('[data-menu-save]')?.addEventListener('click', async () => {
    try {
      await api('POST', `/admin/api/menus/${encodeURIComponent(D.Name)}`, { label: D.Label, items: strip(items) });
      toast('Menu saved. The site updates in a moment.');
    } catch (e) { toast(e.message, 'error'); }
  });
  draw();
}

function clone(x) { return JSON.parse(JSON.stringify(x)); }

function strip(list) {
  return list.map((it) => ({ id: it.id, type: it.type, ref: it.type === 'entry' ? it.ref : undefined, url: it.type === 'url' ? it.url : undefined,
    label: it.label, target: it.target || undefined, children: strip(it.children || []) }));
}
