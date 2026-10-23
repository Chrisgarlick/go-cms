// Rich-text field: Tiptap configured to produce exactly the node and mark set
// the server accepts (internal/richtext). Anything else is dropped there too.
import { Editor } from '@tiptap/core';
import StarterKit from '@tiptap/starter-kit';
import Link from '@tiptap/extension-link';
import Underline from '@tiptap/extension-underline';
import Placeholder from '@tiptap/extension-placeholder';
import Subscript from '@tiptap/extension-subscript';
import Superscript from '@tiptap/extension-superscript';
import { h, icon } from './ui.js';

const empty = { type: 'doc', content: [{ type: 'paragraph' }] };

export function richText(value, { onChange, placeholder = 'Write something…', minimal = false } = {}) {
  const body = h('div', { class: 'rt-body' });
  const bar = h('div', { class: 'rt-bar', role: 'toolbar', 'aria-label': 'Formatting' });
  const wrap = h('div', { class: 'rt' }, bar, body);
  const editor = new Editor({
    element: body,
    injectCSS: false, // strict CSP: base styles live in admin.css
    content: value && value.type === 'doc' ? value : empty,
    extensions: [
      StarterKit.configure({ heading: { levels: [2, 3, 4] } }),
      Link.configure({ openOnClick: false, autolink: true, protocols: ['mailto', 'tel'], HTMLAttributes: { rel: null, target: null } }),
      Underline, Subscript, Superscript,
      Placeholder.configure({ placeholder }),
    ],
    onUpdate: () => onChange && onChange(),
    onSelectionUpdate: () => refresh(),
    onTransaction: () => refresh(),
  });
  const btns = [];
  const add = (name, label, run, isOn) => {
    const b = h('button', { type: 'button', title: label, 'aria-label': label, onmousedown: (e) => e.preventDefault(), onclick: () => { run(); editor.commands.focus(); } }, icon(name));
    b._isOn = isOn;
    btns.push(b);
    bar.append(b);
  };
  const sep = () => bar.append(h('span', { class: 'sep', 'aria-hidden': 'true' }));
  const c = () => editor.chain().focus();
  add('bold', 'Bold (Ctrl+B)', () => c().toggleBold().run(), () => editor.isActive('bold'));
  add('italic', 'Italic (Ctrl+I)', () => c().toggleItalic().run(), () => editor.isActive('italic'));
  add('underline', 'Underline', () => c().toggleUnderline().run(), () => editor.isActive('underline'));
  add('strike', 'Strikethrough', () => c().toggleStrike().run(), () => editor.isActive('strike'));
  add('code', 'Inline code', () => c().toggleCode().run(), () => editor.isActive('code'));
  add('link', 'Link (Ctrl+K)', () => setLink(editor), () => editor.isActive('link'));
  if (!minimal) {
    sep();
    add('h2', 'Heading', () => c().toggleHeading({ level: 2 }).run(), () => editor.isActive('heading', { level: 2 }));
    add('h3', 'Subheading', () => c().toggleHeading({ level: 3 }).run(), () => editor.isActive('heading', { level: 3 }));
    add('list', 'Bulleted list', () => c().toggleBulletList().run(), () => editor.isActive('bulletList'));
    add('list-ordered', 'Numbered list', () => c().toggleOrderedList().run(), () => editor.isActive('orderedList'));
    add('quote', 'Quote', () => c().toggleBlockquote().run(), () => editor.isActive('blockquote'));
    add('code-xml', 'Code block', () => c().toggleCodeBlock().run(), () => editor.isActive('codeBlock'));
    add('minus', 'Divider', () => c().setHorizontalRule().run());
  }
  sep();
  add('undo', 'Undo', () => c().undo().run());
  add('redo', 'Redo', () => c().redo().run());
  function refresh() { for (const b of btns) if (b._isOn) b.classList.toggle('is-on', b._isOn()); }
  editor.view.dom.addEventListener('keydown', (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') { e.preventDefault(); setLink(editor); }
  });
  return {
    el: wrap,
    get: () => {
      const j = editor.getJSON();
      return editor.isEmpty ? null : j;
    },
    text: () => editor.getText(),
    focus: () => editor.commands.focus('end'),
    destroy: () => editor.destroy(),
  };
}

function setLink(editor) {
  const prev = editor.getAttributes('link').href || '';
  const url = window.prompt('Link address (leave empty to remove)', prev || 'https://');
  if (url === null) return;
  const v = url.trim();
  if (!v || v === 'https://') { editor.chain().focus().extendMarkRange('link').unsetLink().run(); return; }
  if (!/^(https?:|mailto:|tel:|\/|#)/i.test(v)) { alert('Links must start with https://, mailto:, tel:, / or #'); return; }
  editor.chain().focus().extendMarkRange('link').setLink({ href: v }).run();
}
