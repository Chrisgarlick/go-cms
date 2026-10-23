// Options page editor (site-wide fields defined by field groups).
import { h, fill, api, toast } from './ui.js';
import { renderObject } from './fields.js';
import { cacheMedia } from './pickers.js';

export function mountOptionsEditor() {
  const D = JSON.parse(document.getElementById('options-data').textContent);
  window.__csrf = D.csrf;
  cacheMedia(Object.values(D.media || {}));
  const root = document.getElementById('options-editor');
  const objs = (D.groups || []).map((g) => ({ g, obj: renderObject(g.fields, D.values || {}, { onChange: () => {} }) }));
  fill(root, ...objs.map((x) => h('section', { class: 'panel pad stack' }, h('h2', {}, x.g.label), x.obj.el)));
  document.querySelector('[data-options-save]')?.addEventListener('click', async () => {
    const values = {};
    objs.forEach((x) => Object.assign(values, x.obj.get()));
    try {
      await api('POST', `/admin/api/options/${encodeURIComponent(D.name)}`, { values });
      objs.forEach((x) => x.obj.setErrors({}, ''));
      toast('Saved');
    } catch (e) {
      if (e.status === 422) objs.forEach((x) => x.obj.setErrors(e.data.fields || {}, ''));
      toast(e.message, 'error');
    }
  });
}
