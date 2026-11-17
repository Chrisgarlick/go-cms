# Lightweight Web Component Library: Tech Spec

Status: draft v0.1

A shadcn-style, copy-into-your-project component library built on native Web Components, with a small home-grown reactive core (a "mini Lit") that works in Laravel, Go, WordPress, or plain HTML.

## 1. Goals and non-goals

**Goals**

- Works in any backend: components are plain `<kit-button>` tags; no framework on the consuming side.
- shadcn model: components are copied into the consumer's project and owned there, not installed as a black-box dependency.
- Reactive authoring that feels like React/Vue: change a value, the UI updates.
- Secure by default: the easy path can't produce XSS.
- **Accessible by default:** WCAG 2.2 AA is the floor for every component, enforced by shared helpers, token-level contrast checks, tests and a manual screen-reader pass, not left to the consumer (section 10).
- Predictable async: no stale-state bugs, no out-of-order responses overwriting newer ones.
- No build step required to consume; core runtime target is 4 KB gzipped or less.
- Themeable through CSS custom properties (design tokens).
- **Fast by default:** a small core, per-page loading of only the components a page uses, and budgets for LCP, INP and CLS enforced in CI (section 17).
- **Easy to extend without forking:** site code can subclass any component or listen to its events, and each component documents its extension points (section 16).

**Non-goals (v1)**

- Full SSR/hydration (progressive enhancement instead, see section 11).
- A virtual DOM, a router, or global state management.
- Supporting browsers without Custom Elements, Shadow DOM, and constructable stylesheets (all evergreen browsers).

## 2. Architecture overview

Three tiers, each usable alone:

| Tier | What | Needs JS? | Example |
|---|---|---|---|
| 0 | Design tokens (CSS custom properties) | No | `--kit-radius`, `--kit-color-primary` |
| 1 | CSS + HTML snippet per component | No | `.kit-card` markup pasted into a Blade/Go/WP template |
| 2 | Web Component wrapping the same CSS, adding behaviour | Yes | `<kit-dialog>`, `<kit-tabs>`, `<kit-counter>` |

Static components (badge, card, alert) stay at tier 1. Only components that need state or behaviour get tier 2. The core runtime (section 5) exists purely to make tier 2 pleasant to write.

## 3. Repo structure

```
kit/
  core/
    element.js      # base class: props, state, scheduling, lifecycle
    html.js         # html`` tag + renderer (safe by default)
    css.js          # css`` tag -> constructable stylesheet
    define.js       # safe customElements.define wrapper
    a11y.js         # uid, announce, roving tabindex (section 10.3)
    loader.js       # loads only the components a page uses (section 17.3)
  tokens/
    tokens.css      # :root custom properties
    contrast.json   # foreground/background pairs checked in CI
    themes/dark.css
  components/
    button/
      button.js     # tier 2 component: exports the class, then registers it
      button.css    # shared with tier 1
      snippet.html  # tier 1 markup
      button.test.js
      README.md     # includes the a11y contract
  registry/
    registry.json   # name, version, files, deps, hash, a11y metadata
  cli/              # `kit init`, `kit add button` (copies files in)
  tests/
    xss-corpus.js   # hostile payloads run against every component
    perf/           # size budgets, render benchmarks, INP checks (section 17)
  docs/             # playground / examples per backend
```

The CLI can be a single Go binary (fits an existing Go toolchain) or a small Node script. `kit init` copies `core/` and `tokens/` into the consumer's project; `kit add <name>` copies the component folder and records the version in a lockfile so updates can be diffed, shadcn-style.

## 4. Design tokens and theming

Tokens are plain custom properties on `:root`. They inherit through shadow DOM, which is what makes theming work with encapsulated components.

```css
:root {
  --kit-color-primary: #2563eb;
  --kit-color-on-primary: #fff;
  --kit-radius: 0.5rem;
  --kit-space-2: 0.5rem;
  --kit-font: system-ui, sans-serif;
}
[data-theme="dark"] { --kit-color-primary: #60a5fa; }
```

Customisation surfaces, in order of preference:

1. Tokens (global theming).
2. Per-component custom properties (`--kit-button-bg`) with token fallbacks.
3. `::part(name)` for structural overrides from outside the shadow root.
4. Slots for content.
5. Editing the copied source (the shadcn escape hatch).

## 5. Core runtime

### 5.1 Concept mapping (React/Vue to this)

| React / Vue | Here |
|---|---|
| props | `static props`: attribute-backed, typed, optionally reflected |
| `useState` / `ref` | `static state`: internal reactive fields |
| re-render on change | microtask-batched `render()` |
| `useEffect` | `updated(changed)` / `willUpdate(changed)` |
| effect cleanup | `this.signal` (aborts on disconnect) |
| computed | plain getters or locals in `render()` |
| events up | `this.emit('change', detail)` (bubbling, composed `CustomEvent`) |
| context | CSS custom properties (styling) or events (data) |
| keys in lists | `repeat()` helper (phase 2) |

### 5.2 Reactivity model

Properties are defined as accessors on the prototype. The setter compares with `Object.is`, stores the value, records the previous value, and schedules **one** render in a microtask. Writes are **synchronous**; rendering is **batched**. This is the key to avoiding React-style stale state:

```js
this.count += 1;
this.count += 1;
// this.count is 2 immediately; one render happens afterwards
```

Shallow reactivity only. For objects and arrays, replace instead of mutating (`this.items = [...this.items, x]`), the same convention as React. A deep `reactive()` Proxy helper can come later if wanted, but it adds edge cases (identity, nested arrays, prototype pollution) that aren't needed on day one.

**Gotcha, class fields shadow accessors.** Declaring `count = 0` as a class field in a subclass defines an own property that hides the accessor and silently kills reactivity. Defaults therefore live in `static props` / `static state`, never in class fields. Dev mode should detect own-property shadowing and warn.

### 5.3 Reference implementation: `core/element.js`

```js
import { render } from './html.js';

const kebab = (s) => s.replace(/[A-Z]/g, (m) => '-' + m.toLowerCase());
const from = {
  String: (v) => v ?? '',
  Number: (v) => (v == null ? null : Number(v)),
  Boolean: (v) => v !== null,
};
const to = {
  String: (v) => v,
  Number: (v) => String(v),
  Boolean: (v) => (v ? '' : null),
};

export class Element extends HTMLElement {
  static props = {};   // { variant: { type: String, default: 'primary', reflect: true } }
  static state = {};   // { open: false }  (internal, no attribute)
  static styles = [];   // CSSStyleSheet[] from css``
  static shadow = true;

  static get observedAttributes() {
    this.finalize();
    return [...this._attrs.keys()];
  }

  static finalize() {
    if (Object.hasOwn(this, '_decl')) return;
    const parent = Object.getPrototypeOf(this);
    parent.finalize?.();                     // subclasses inherit parent props and state
    const decl = { ...(parent._decl ?? {}) };
    for (const [k, o] of Object.entries(this.props)) decl[k] = { type: String, ...o, attr: true };
    for (const [k, v] of Object.entries(this.state)) decl[k] = { default: v, attr: false };
    this._decl = decl;
    this._attrs = new Map(
      Object.entries(decl).filter(([, o]) => o.attr).map(([k]) => [kebab(k), k])
    );
    for (const name of Object.keys(decl)) {
      Object.defineProperty(this.prototype, name, {
        configurable: true,
        get() { return this.#values.get(name); },
        set(next) {
          const prev = this.#values.get(name);
          if (Object.is(prev, next)) return;
          this.#values.set(name, next);
          this.requestUpdate(name, prev);
        },
      });
    }
  }

  #values = new Map();
  #changed = new Map();
  #pending = false;
  #connected = false;
  #rendered = false;
  #rendering = false;
  #reflecting = false;
  #upgraded = false;
  #ctl = null;
  #tasks = new Map();
  #complete = Promise.resolve();
  #root;

  constructor() {
    super();
    const C = this.constructor;
    C.finalize();
    for (const [k, o] of Object.entries(C._decl)) this.#values.set(k, o.default);
    this.#root = C.shadow ? this.shadowRoot ?? this.attachShadow({ mode: 'open' }) : this;
  }

  get updateComplete() { return this.#complete; }
  get signal() { return this.#ctl?.signal; }

  connectedCallback() {
    this.#connected = true;
    this.#ctl = new AbortController();
    if (!this.#upgraded) {
      this.#upgraded = true;
      // Handle `el.prop = x` assigned before the element was defined.
      for (const k of Object.keys(this.constructor._decl)) {
        if (Object.hasOwn(this, k)) { const v = this[k]; delete this[k]; this[k] = v; }
      }
      if (this.shadowRoot) this.shadowRoot.adoptedStyleSheets = this.constructor.styles;
    }
    this.connected?.();
    this.requestUpdate();
  }

  disconnectedCallback() {
    this.#connected = false;
    this.#ctl.abort();                       // cancels fetches, listeners, tasks
    for (const c of this.#tasks.values()) c.abort();
    this.disconnected?.();
  }

  attributeChangedCallback(attr, _old, val) {
    if (this.#reflecting) return;
    const name = this.constructor._attrs.get(attr);
    const { type } = this.constructor._decl[name];
    this[name] = from[type.name](val);
  }

  requestUpdate(name, prev) {
    if (this.#rendering) console.warn(`[kit] ${this.localName}: state changed during render()`);
    if (name !== undefined && !this.#changed.has(name)) this.#changed.set(name, prev);
    if (this.#pending) return;
    this.#pending = true;
    this.#complete = Promise.resolve().then(() => this.#flush());
  }

  #flush() {
    this.#pending = false;
    if (!this.#connected) return;            // connectedCallback re-requests an update
    const changed = this.#changed;
    this.#changed = new Map();
    try {
      this.willUpdate?.(changed);
      this.#reflect(changed);
      this.#rendering = true;
      render(this.render(), this.#root);
      this.#rendering = false;
      if (!this.#rendered) { this.#rendered = true; this.firstUpdated?.(); }
      this.updated?.(changed);
    } catch (err) {
      this.#rendering = false;
      console.error(`[kit] ${this.localName} failed to render`, err);
    }
  }

  #reflect(changed) {
    this.#reflecting = true;                 // prevents attribute -> prop -> attribute loop
    for (const name of changed.keys()) {
      const o = this.constructor._decl[name];
      if (!o.reflect) continue;
      const v = to[o.type.name](this[name]);
      v === null ? this.removeAttribute(kebab(name)) : this.setAttribute(kebab(name), v);
    }
    this.#reflecting = false;
  }

  /** Latest-wins async. A newer call with the same key aborts the older one. */
  async run(key, fn) {
    this.#tasks.get(key)?.abort();
    const ctl = new AbortController();
    this.#tasks.set(key, ctl);
    const signal = AbortSignal.any([ctl.signal, this.signal]);
    try {
      const out = await fn(signal);
      return signal.aborted ? undefined : out;  // undefined = superseded or disconnected
    } catch (e) {
      if (signal.aborted) return undefined;
      throw e;
    }
  }

  emit(name, detail) {
    return this.dispatchEvent(new CustomEvent(name, { detail, bubbles: true, composed: true }));
  }
}
```

`core/css.js` and `core/define.js`:

```js
// css.js: no interpolation allowed, which closes off CSS injection
export const css = (strings, ...values) => {
  if (values.length) throw new Error('css`` does not accept interpolations; use custom properties');
  const sheet = new CSSStyleSheet();
  sheet.replaceSync(strings[0]);
  return sheet;
};

// define.js: safe in WordPress, where multiple plugins/themes may load the same bundle
export function define(tag, cls) {
  const existing = customElements.get(tag);
  if (existing) { if (existing !== cls) console.warn(`[kit] <${tag}> already defined`); return; }
  customElements.define(tag, cls);
}
```

Using `adoptedStyleSheets` means one parsed stylesheet shared by every instance (faster than a `<style>` per instance) and it is not blocked by a strict `style-src` CSP.

### 5.4 Reference implementation: `core/html.js`

A tagged-template renderer in the style of lit-html. The template string is parsed **once** into a `<template>`; every later render only updates the dynamic holes. Because the DOM nodes persist, inputs keep focus and caret position, which the earlier `innerHTML` approach could not do.

```js
const BRAND = Symbol('kit.result');
const MARKER = 'k' + Math.random().toString(36).slice(2, 8);
const RE = new RegExp(MARKER + '(\\d+)', 'g');
const cache = new WeakMap();
const roots = new WeakMap();
const URL_ATTRS = new Set(['href', 'src', 'action', 'formaction', 'poster', 'xlink:href']);
const BLOCKED = new Set(['style', 'srcdoc']);

export const html = (strings, ...values) => {
  // Template literals have .raw; a concatenated string does not. Blocks html(userString).
  if (!strings?.raw) throw new TypeError('html must be used as a tagged template literal');
  return { [BRAND]: true, strings, values };
};

function safeUrl(v) {
  try {
    const p = new URL(v, location.href).protocol;
    return ['http:', 'https:', 'mailto:', 'tel:'].includes(p) ? v : '#';
  } catch { return '#'; }
}

function prepare(strings) {
  let s = '';
  for (let i = 0; i < strings.length; i++) {
    s += strings[i];
    if (i === strings.length - 1) break;
    const inTag = s.lastIndexOf('<') > s.lastIndexOf('>');
    s += inTag ? MARKER + i : `<!--${MARKER}${i}-->`;
  }
  const t = document.createElement('template');
  t.innerHTML = s;   // only static, developer-authored strings ever reach this sink
  return t;
}

class Instance {
  constructor(result) {
    this.strings = result.strings;
    let tpl = cache.get(this.strings);
    if (!tpl) cache.set(this.strings, (tpl = prepare(this.strings)));
    this.frag = tpl.content.cloneNode(true);
    this.parts = [];
    const nodes = [];
    const w = document.createTreeWalker(this.frag, 1 | 128); // elements + comments
    while (w.nextNode()) nodes.push(w.currentNode);
    for (const n of nodes) {
      if (n.nodeType === 8) {
        if (n.data.startsWith(MARKER)) this.parts.push(new ChildPart(n, +n.data.slice(MARKER.length)));
      } else {
        for (const a of [...n.attributes]) {
          if (!a.value.includes(MARKER)) continue;
          n.removeAttribute(a.name);
          this.parts.push(new AttrPart(n, a.name, a.value));
        }
      }
    }
    this.update(result.values);
  }
  update(values) { for (const p of this.parts) p.commit(values); }
}

class ChildPart {
  constructor(start, idx) {
    this.start = start;
    this.idx = idx;
    this.end = document.createComment('');
    start.after(this.end);
    this.cur = null;
  }
  #clear() {
    for (let n = this.start.nextSibling; n && n !== this.end; n = this.start.nextSibling) n.remove();
  }
  commit(values) { this.set(values[this.idx]); }
  set(v) {
    if (v?.[BRAND]) {
      if (this.cur instanceof Instance && this.cur.strings === v.strings) return this.cur.update(v.values);
      this.#clear();
      this.cur = new Instance(v);
      this.end.before(this.cur.frag);
    } else if (Array.isArray(v)) {
      this.#clear();
      this.cur = null;
      for (const item of v) {
        const m = document.createComment('');
        this.end.before(m);
        new ChildPart(m, 0).set(item);
      }
    } else {
      const text = v == null || v === false ? '' : String(v);   // ALWAYS a text node
      if (this.cur instanceof Text) {
        if (this.cur.data !== text) this.cur.data = text;
      } else {
        this.#clear();
        this.cur = document.createTextNode(text);
        this.end.before(this.cur);
      }
    }
  }
}

class AttrPart {
  constructor(el, name, raw) {
    this.el = el;
    this.idx = [...raw.matchAll(RE)].map((m) => +m[1]);
    this.statics = raw.split(RE).filter((_, i) => i % 2 === 0);
    this.prefix = /^[@.?]/.test(name) ? name[0] : '';
    this.attr = this.prefix ? name.slice(1) : name;
    const a = this.attr.toLowerCase();
    if (this.prefix !== '@' && a.startsWith('on')) throw new Error(`Binding "${name}" is not allowed`);
    if (BLOCKED.has(a)) throw new Error(`Binding "${name}" is not allowed`);
    if (this.prefix === '@') {
      this.handler = (e) => this.fn?.(e);   // always calls the latest function
      el.addEventListener(this.attr, this.handler);
    }
  }
  commit(values) {
    const { el, attr, prefix } = this;
    const v = values[this.idx[0]];
    if (prefix === '@') { this.fn = typeof v === 'function' ? v : undefined; return; }
    if (prefix === '.') {
      const next = URL_ATTRS.has(attr.toLowerCase()) ? safeUrl(String(v)) : v;
      if (el[attr] !== next) el[attr] = next;   // equality guard keeps caret position in inputs
      return;
    }
    if (prefix === '?') { el.toggleAttribute(attr, !!v); return; }
    let out = this.statics[0];
    this.idx.forEach((i, n) => { out += String(values[i] ?? '') + this.statics[n + 1]; });
    if (URL_ATTRS.has(attr.toLowerCase())) out = safeUrl(out);
    if (this.prev !== out) { el.setAttribute(attr, out); this.prev = out; }
  }
}

export function render(result, container) {
  const cur = roots.get(container);
  if (cur && cur.strings === result.strings) return cur.update(result.values);
  const inst = new Instance(result);
  container.replaceChildren(inst.frag);
  roots.set(container, inst);
}
```

This is a **sketch to be proven by tests**, not finished code. Known limits of the sketch:

- The HTML parser lowercases attribute names, so use lowercase property/event names (`@my-event`, `.value`) or add a case-insensitive lookup against declared props.
- No bindings inside `<style>`, `<script>`, `<textarea>`, `<title>` (raw-text elements; use `.value=` for textarea). This is also a security feature.
- The "inside a tag" detection is a heuristic (breaks if a literal `>` appears inside an attribute value).
- Arrays re-render fully each time; a keyed `repeat()` is needed before long or input-heavy lists.
- Light DOM mode (`static shadow = false`) wipes the host's children on first render. Capture slot-like content first (`this.initialChildren`) or render into an inner container.

## 6. Worked example: counter (click increments a number)

```js
import { Element } from '../../core/element.js';
import { html } from '../../core/html.js';
import { css } from '../../core/css.js';
import { define } from '../../core/define.js';

class KitCounter extends Element {
  static props = { step: { type: Number, default: 1 } };
  static state = { count: 0 };
  static styles = [css`
    button { background: var(--kit-button-bg, var(--kit-color-primary));
             color: var(--kit-color-on-primary); border: 0;
             border-radius: var(--kit-radius); padding: .5rem 1rem; }
  `];

  inc = () => {
    this.count += this.step;
    this.emit('change', { count: this.count });
  };

  render() {
    return html`<button part="button" @click=${this.inc}>Count: ${this.count}</button>`;
  }
}
define('kit-counter', KitCounter);
```

Usage in any backend: `<kit-counter step="2"></kit-counter>`. Outside JS can listen with `el.addEventListener('change', ...)`, which works with Alpine, htmx, or vanilla scripts.

What happens on a click: handler sets `count` (sync), the setter queues one microtask, the microtask re-runs `render()`, the renderer updates a single text node. Ten rapid clicks produce ten correct increments and at most one render per task.

**Two-way input**

```js
static state = { text: '' };
render() {
  return html`<input .value=${this.text} @input=${(e) => (this.text = e.target.value)}>
              <p>${this.text}</p>`;   // text node, never HTML
}
```

The `<input>` persists between renders, so focus and caret are preserved.

## 7. XSS and security model

### 7.1 Trust boundaries

| Source | Trusted? | Notes |
|---|---|---|
| Template literal strings in component source | Yes | Authored by the developer |
| `${}` values in templates | **No** | May originate from users, attributes, APIs |
| Attributes in server-rendered HTML | No | Backend must escape (Blade `{{ }}`, Go `html/template`, WP `esc_attr()`) |
| Slotted light-DOM content | Owned by the host page | Components never read it as HTML |

### 7.2 Rules enforced by the core

1. **Interpolations are text, never HTML.** Child bindings create text nodes, so `<img onerror>` renders as visible characters.
2. **No `innerHTML` in the public API.** The one internal use parses static template strings only. Under Trusted Types, that single sink gets a policy.
3. **`html` must be a tagged literal.** The `strings.raw` check rejects `html(userString)` and string-concatenated templates.
4. **Attribute bindings use `setAttribute`**, which never parses HTML.
5. **Event handlers must be functions.** `onclick="${...}"` style bindings throw; string handlers are impossible.
6. **Blocked dynamic bindings:** any `on*` attribute or property, `style`, `srcdoc`.
7. **URL attributes (`href`, `src`, `action`, `formaction`, `poster`) are validated** against an allowlist (`http`, `https`, `mailto`, `tel`, relative). `javascript:` and `data:` become `#`. This applies to the property form (`.href`) too.
8. **The `css` tagged template takes no interpolations**, preventing CSS injection. Dynamic styling goes through custom properties set via properties/attributes you control.
9. **Escape hatch is explicit and loud:** `unsafeHTML(dirty, sanitizer)` in a separate module, requiring a sanitizer argument (DOMPurify or the native Sanitizer API when available). Not part of core; phase 2.
10. **No `eval`, no `new Function`, no inline scripts needed**, so a strict CSP works: `script-src 'self'`; `require-trusted-types-for 'script'` is achievable.
11. If a deep `reactive()` or JSON-merge helper is ever added, guard against prototype pollution (`__proto__`, `constructor`, `prototype` keys).

Note: the earlier quick `innerHTML` base class was **not** safe for user content. This design replaces it.

### 7.3 Backend-side guidance (documented per platform)

- Laravel: always `{{ }}`, never `{!! !!}`, for attribute values passed to components.
- Go: use `html/template` (contextual escaping), not `text/template`.
- WordPress: `esc_attr()` / `esc_url()` / `wp_kses_post()` as appropriate.
- Passing structured data: JSON in a `<script type="application/json">` block (escape `<` as `\u003c`) rather than a large attribute.

### 7.4 Supply chain and verification

Copying code into a project (the shadcn model) means the registry is a trust boundary, so it gets the same care as the runtime.

- **Zero runtime dependencies in core.** Nothing to compromise upstream; optional extras (a sanitizer) are separate modules with pinned versions.
- **Hash-verified installs.** `registry.json` lists a SHA-256 for every file. `kit add` downloads, verifies the hash, and refuses to write on a mismatch. `kit.lock` records the hashes.
- **`kit verify`** re-checks local files against the lock and reports local edits (informational, not an error), so you can see exactly what differs from upstream.
- **Optionally sign the registry** (for example with minisign) and pin the public key in the CLI. HTTPS only; CI pins a registry version instead of fetching "latest".
- **No code execution on install.** `kit add` only copies files; there are no install scripts.
- **Review before accepting updates:** `kit diff` shows upstream changes before they touch your copy.
- **If loading from a CDN at all,** use Subresource Integrity (`integrity` on `<script type="module">` and `<link rel="modulepreload">`). The copy model avoids third-party CDNs by default.
- **Recommended header** for sites that can support it: `Content-Security-Policy: script-src 'self'; object-src 'none'; base-uri 'none'; require-trusted-types-for 'script'`. WordPress plugins often inject inline scripts, so a strict policy may need nonces or per-page tuning; the kit itself needs no inline script or `eval`.
- **Add a `SECURITY.md`** to the kit repo with a private reporting address.

## 8. Race conditions and async correctness

| Hazard | Mitigation |
|---|---|
| Stale state in closures (React's classic bug) | Setters are synchronous and read live from `this`, so `this.count += 1` is never stale. Handlers are re-pointed on every render, so they never close over old values. |
| Multiple writes before render | Coalesced into one microtask render; `changed` map holds the *first* previous value per property. |
| Out-of-order responses (search-as-you-type) | `this.run(key, fn)`: a newer call aborts the older; superseded results return `undefined`. |
| Work after the element is removed | `this.signal` aborts on disconnect; `run()` tasks abort too; renders are skipped when disconnected. |
| Reconnect (element moved, htmx/Turbo swaps) | `connectedCallback` is idempotent: fresh `AbortController`, upgrade step runs once, update re-requested. |
| Property set before element is defined | Upgrade step re-applies own properties through the accessors. |
| Attribute and property feedback loops | `#reflecting` guard around reflected writes. |
| State set during `render()` | Dev warning; renders must be pure. |
| State set in `updated()` causing loops | Allowed (it schedules another cycle), but dev mode should throw after about 50 consecutive cycles. |
| Reading the DOM right after setting state | DOM is stale until the microtask; use `await this.updateComplete`. |
| Double submit | Component sets a `busy` state before awaiting and ignores re-entry while true. |

Example, search with out-of-order protection:

```js
async search(q) {
  this.loading = true;
  const data = await this.run('search', (signal) =>
    fetch(`/api/search?q=${encodeURIComponent(q)}`, { signal }).then((r) => r.json())
  );
  if (data === undefined) return;   // superseded or disconnected: leave state alone
  this.results = data;
  this.loading = false;
}
```

## 9. Dynamic content patterns

- **Derived values:** compute in `render()` (`const total = this.items.length`). Use `willUpdate(changed)` only when derivation is expensive or must set other state.
- **Conditional UI:** `${this.open ? html`<div>...</div>` : null}`. Template identity is preserved, so toggling is cheap.
- **Lists:** `${this.items.map((i) => html`<li>${i.name}</li>`)}`. Fine for display; add keyed `repeat()` for lists with inputs.
- **Outbound data:** `this.emit('change', detail)` (bubbles, composed, crosses shadow boundaries).
- **Inbound data from the host page:** attributes for simple values, properties for objects (`el.items = [...]`), and slots for markup.
- **Content swapped by htmx/Turbo/morphing libraries:** elements connect and disconnect repeatedly, which the lifecycle handles. Shadow content is untouched by morphers.

## 10. Accessibility (built in, not bolted on)

Every component meets WCAG 2.2 AA as a floor and follows the matching WAI-ARIA Authoring Practices (APG) pattern, so consumers get accessibility by default rather than by effort. "Accessible" here means conformant and tested, not claimed: automated tools catch only part of the problems, so the guarantee comes from native-first markup, shared helpers, token-level contrast checks, and a mandatory manual pass (10.6).

### 10.1 Principles

1. **Native first.** `<button>`, `<a>`, `<input>`, `<select>`, `<dialog>`, `<details>` and `popover` bring roles, keyboard handling and focus behaviour for free. ARIA is added only where no native element exists, and then exactly as the APG pattern specifies.
2. **The accessible name is required.** Components whose name can't come from visible text (icon buttons, close buttons) take a required `label` prop; dev mode logs an error when it is missing.
3. **Keyboard parity.** Everything a mouse can do, the keyboard can do, with visible focus, a logical order and no traps.
4. **Never rely on colour, hover or motion alone.**
5. **Defaults are accessible.** Retheming through tokens can't silently break contrast (10.4).
6. **Every component documents its a11y contract:** role, key bindings, labelling requirements, announcements, known limits.

### 10.2 Shadow DOM and ARIA (the main risk in this architecture)

ID-based references (`aria-labelledby`, `aria-describedby`, `aria-controls`, `<label for>`) don't cross shadow boundaries. Rules that follow:

- Keep related elements in one root: a text field renders its label, input, hint and error inside the same shadow root.
- Attributes on the host (`aria-label`, `aria-labelledby`) are not forwarded inward automatically. A component that wraps an inner control exposes a `label` prop and wires it itself.
- Form controls use `formAssociated` + `ElementInternals`. Form-associated custom elements are labelable, so an outside `<label for="id">` works. Default semantics (`role`, `ariaLabel`) go on `internals` rather than as attributes on the host (check current browser support).
- Form-heavy components (text field, select, checkbox group) default to light DOM (`static shadow = false`), where consumer labels and ID references just work. Self-contained widgets (dialog, tabs) can stay in shadow DOM.
- Element-reference ARIA (`ariaLabelledByElements`) can bridge roots where supported: progressive enhancement only, never a dependency.

### 10.3 Core helpers: `core/a11y.js`

```js
let counter = 0;
/** Unique id for aria-labelledby / aria-describedby wiring inside one root. */
export const uid = (prefix = 'kit') => `${prefix}-${++counter}`;

let region;
/** Announce a status message without moving focus (WCAG 4.1.3). */
export function announce(message, politeness = 'polite') {
  if (!region) {
    region = document.createElement('div');
    region.setAttribute('aria-atomic', 'true');
    region.style.cssText =
      'position:fixed;inline-size:1px;block-size:1px;overflow:hidden;clip-path:inset(50%);white-space:nowrap';
    document.body.append(region);
  }
  region.setAttribute('aria-live', politeness);
  region.textContent = '';
  // Short delay so screen readers register the region and re-announce identical text.
  setTimeout(() => { region.textContent = message; }, 100);
}

/** Roving tabindex + arrow keys for tabs, toolbars, menus, radio groups (APG). */
export function roving(container, { selector, orientation = 'horizontal', wrap = true, signal } = {}) {
  const items = () => [...container.querySelectorAll(selector)].filter((el) => !el.disabled && !el.hidden);
  const activate = (el) => { items().forEach((i) => { i.tabIndex = i === el ? 0 : -1; }); el.focus(); };
  container.addEventListener('keydown', (e) => {
    const list = items();
    const i = list.indexOf(e.target.closest(selector));
    if (i < 0) return;
    const rtl = getComputedStyle(container.host ?? container).direction === 'rtl';
    const [fwd, back] = orientation === 'vertical'
      ? ['ArrowDown', 'ArrowUp']
      : rtl ? ['ArrowLeft', 'ArrowRight'] : ['ArrowRight', 'ArrowLeft'];
    let j = null;
    if (e.key === fwd) j = i + 1;
    else if (e.key === back) j = i - 1;
    else if (e.key === 'Home') j = 0;
    else if (e.key === 'End') j = list.length - 1;
    if (j === null) return;
    e.preventDefault();
    if (wrap) j = (j + list.length) % list.length;
    if (list[j]) activate(list[j]);
  }, { signal });
}
```

A tabs component calls `roving(this.shadowRoot, { selector: '[role=tab]', signal: this.signal })` once in `connected()`; the listener is delegated, so it works for tabs rendered later. Native `<dialog>.showModal()` already makes the page behind it inert, closes on Escape and returns focus to the opener, so there is no hand-rolled focus trap to get wrong.

Dev-mode checks in `Element` (stripped from production builds) warn when a component that needs an accessible name has none, when `tabindex` is greater than 0, or when an `aria-*` ID reference doesn't resolve inside its own root.

### 10.4 Visual and perceptual requirements

- **Contrast:** every foreground/background token pair is declared in `tokens/contrast.json`. CI fails if body text is under 4.5:1, large text under 3:1, or UI boundaries and focus indicators under 3:1, in every theme. Consumers who override tokens run `kit check-contrast` against their own values.
- **Focus:** a visible `:focus-visible` outline from `--kit-focus-ring` (at least 2px, 3:1 against adjacent colours); never `outline: none` without a replacement; focused elements are not hidden behind sticky headers (`scroll-margin`, WCAG 2.4.11).
- **Target size:** at least 24x24 CSS px (WCAG 2.2 AA minimum), 44x44 default for touch-primary controls.
- **Zoom and reflow:** rem/em units and logical properties; no fixed heights on text containers; usable at 320px width and 400% zoom without two-axis scrolling; survives user text-spacing overrides.
- **Forced colours:** `@media (forced-colors: active)` styles use system colours (`CanvasText`, `Highlight`) and borders, never background-only or box-shadow-only distinctions.
- **Motion:** a `--kit-duration` token drops to 0 under `prefers-reduced-motion`; nothing auto-plays or moves for more than 5 seconds without a pause control.
- **Not colour alone:** errors and states pair colour with an icon or text.
- **Language and direction:** logical CSS properties so RTL works; no hard-coded user-facing strings ("Close", "Loading") inside components, they come from props or slots with English defaults.

### 10.5 Component behaviour rules

| Concern | Rule |
|---|---|
| Dialog | Native `<dialog>` with `showModal()`; labelled via `aria-labelledby`; initial focus on the first control or the heading; focus returns to the opener on close |
| Popover, menu, tooltip | `popover` API; tooltips supplement, never hold essential information; hover/focus content is dismissible, hoverable and persistent (WCAG 1.4.13) |
| Tabs | APG tabs pattern: `tablist`, `tab`, `tabpanel`; roving tabindex; arrows, Home, End |
| Disclosure, accordion | Native `<details>`/`<summary>`, or a button with `aria-expanded` and `aria-controls` |
| Form fields | Visible `<label>`; hint and error wired with `aria-describedby`; `aria-invalid`; matching `autocomplete`; placeholder is never the label |
| Status updates | Announced through `announce()` or `role="status"` without moving focus |
| Loading, disabled | `aria-busy` while loading; where discoverability matters, `aria-disabled` plus an explanation instead of removing the control |
| Drag interactions | Always ship a non-drag alternative (WCAG 2.5.7) |
| Icons | Decorative icons `aria-hidden="true"`; informative icons get a text alternative |
| Headings, landmarks | Components never hard-code heading levels (they take a `level` prop); landmarks always have a label |

### 10.6 Testing: five layers

1. **axe-core** on every component in every state and theme.
2. **Keyboard-flow tests** per component, generated from its APG key table (Tab, arrows, Home/End, Enter/Space, Escape), asserting where focus lands after each key.
3. **Accessibility-tree snapshots** (Playwright `ariaSnapshot`) to lock roles, names and states against regressions.
4. **Emulated conditions:** `forced-colors`, `prefers-reduced-motion`, 200% and 400% zoom, a 320px viewport, and RTL.
5. **Manual pass** before a component is marked stable: VoiceOver with Safari (macOS and iOS), NVDA with Firefox or Chrome, keyboard-only, and a voice-control spot check. Results and date go in the component README and the registry entry.

### 10.7 Definition of done (every component)

- [ ] Uses a native element where one exists; ARIA matches the APG pattern named in the README
- [ ] Accessible-name requirement enforced (required prop plus dev warning)
- [ ] Keyboard table implemented and tested
- [ ] Focus visible, never obscured, and lands somewhere sensible after close or removal
- [ ] Contrast pairs registered and passing in all themes
- [ ] Works under forced colours, reduced motion, 400% zoom and RTL
- [ ] axe clean, accessibility-tree snapshot committed, manual screen-reader pass recorded
- [ ] README holds the a11y contract (role, keys, labelling, announcements, known limits)
- [ ] Extension points documented in the README (exported class, overridable methods, events, parts; section 16)
- [ ] Registry entry carries `a11y` metadata (pattern, WCAG criteria covered, last tested date)

## 11. Server-rendered and progressive enhancement

- Author fallback content as light-DOM children, e.g. `<kit-tabs><a href="#a">A</a>...</kit-tabs>`; it is usable before JS runs.
- Avoid layout shift with `kit-card:not(:defined) { display: block; min-height: ...; }`.
- Declarative Shadow DOM and true hydration are **phase 3**; v1 is client render only. Tier 1 CSS-only components cover the "must render on the server" cases.
- Loading: one `<script type="module">`, or only the components a page uses (section 17.3). WordPress: register as a script module (check the current Script Modules API for the WP version in use). Laravel: Vite or a static `public/` path. Go: `//go:embed` and serve as static assets.
- Namespace tags with a prefix (`kit-`) to avoid collisions with other plugins/themes; the prefix is configurable at `kit init`.

## 12. Distribution

- `registry.json` entries: `name`, `version`, `files[]`, `dependsOn[]` (other components), `hash`, `a11y`.
- `kit init` copies `core/` and `tokens/` into the project (no npm runtime dependency).
- `kit add button` downloads the component folder, verifies each file's SHA-256 against the registry (refusing on mismatch, section 7.4), copies it, then records name, version, and file hashes in `kit.lock`. `kit verify` re-checks local files against the lock.
- `kit diff button` compares the local copy to upstream, so consumers can pull updates without losing local edits.
- Prefer subclassing a kit component in site code over editing the copied file: untouched files update cleanly with `kit diff`, edited ones conflict (section 16).
- Types: JSDoc plus `tsc --checkJs` generates `.d.ts`; no TypeScript build for consumers.
- Optional later: publish a bundled, versioned package for people who prefer installing to copying.

## 13. Testing strategy

- **Real browsers**, not jsdom (Custom Elements, Shadow DOM, adoptedStyleSheets are incomplete there): Web Test Runner or Playwright component tests.
- **Core unit tests:** batching (10 writes, 1 render), reflection loops, upgrade-before-define, disconnect/reconnect, aborted tasks, shadowing warning.
- **XSS corpus:** every component is mounted with hostile values in every prop, attribute, and slot (`<img src=x onerror=...>`, `"><script>`, `javascript:`, `data:text/html`); assert no script runs, no unexpected elements appear, and URL attrs are neutralised.
- **Race tests** with a controllable fake `fetch` that resolves out of order.
- **a11y:** axe-core on every component in every state and theme, plus keyboard-flow, accessibility-tree, forced-colors, reduced-motion and zoom tests (section 10.6).
- **Visual:** screenshot diffs per theme (light/dark).
- **Security lints:** fail on unsafe sinks (`innerHTML`, `outerHTML`, `insertAdjacentHTML`, `document.write`, `eval`, `new Function`, string `setTimeout`), and run the demo pages under a strict CSP with Trusted Types enforced (sections 7.4 and 18).
- **Performance:** per-file gzipped size budgets, render benchmarks under 4x CPU throttling, Lighthouse CI, and scripted INP checks on demo pages (section 17.7).

## 14. Phases and acceptance criteria

| Phase | Scope | Done when |
|---|---|---|
| 1 | Tokens, `Element`, `html`, `css`, `define`; 5 components (button with extension points, badge, card, counter, dialog) | Core tests and XSS corpus pass; counter demo works in Laravel, Go, and WP |
| 2 | `repeat()`, `unsafeHTML` with sanitizer, `formAssociated` support, `loader.js` (per-page loading), CLI (`init`, `add`, `verify`) | A form field submits natively; list with inputs keeps focus; `kit add` works end to end |
| 3 | DSD/hydration, `reactive()` deep helper (optional), registry diffing, docs site | Server-rendered first paint without layout shift |

**Quality gate (all phases):** a component isn't done until it meets the definition of done in section 18 (accessibility, security, performance and extension points). Phase 1 also ships the token contrast check, the a11y test harness, the XSS corpus runner, the size-budget check and the benchmark harness, so every later component inherits them.

## 15. Open decisions

1. **Shadow DOM default vs light DOM default.** Spec assumes shadow for encapsulation, with `static shadow = false` as an option. Light DOM is easier for global CSS and forms but loses slots and scoping. For accessibility, shadow DOM's main cost is that ID-based ARIA references (aria-labelledby, label for) don't cross roots (10.2), which argues for light DOM on form-heavy components.
2. **CLI language:** Go single binary vs Node script.
3. **Tag prefix** and package/registry naming.
4. **Shallow-only reactivity** (as specced) vs adding a deep Proxy `reactive()` later.
5. **Build own renderer vs depend on `lit-html`.** Own renderer keeps zero dependencies, but the renderer is the hardest piece to get right (keyed lists, edge cases). A reasonable fallback: ship this API and swap the internals of `render()` for lit-html if maintenance gets heavy; component code stays the same.
6. **Loading strategy:** server-emitted per-page script tags (simplest, no client runtime), the client auto-loader (section 17.3), or build-time per-page bundles. They can be combined; decide which is the default for WordPress, Laravel and Go.

## 16. Extending and customising components

Site-specific behaviour (copy a product ID, add to a basket) lives in site code, not in the kit. Kit components expose documented extension points, so a theme can add behaviour without editing copied files, and anything every site should get by default goes into the kit repo.

### 16.1 Customisation ladder (use the first rung that works)

| Rung | Use when | How |
|---|---|---|
| 1. Tokens, custom properties, `::part()` | Look and feel | CSS only (section 4) |
| 2. Delegated event listener | One-off behaviour on existing markup | `document.addEventListener('click', ...)` with `closest('kit-button[data-product-id]')` |
| 3. Subclass element | Reusable behaviour with its own tag | `class ShopAddButton extends KitButton`, registered as `<shop-add-button>` |
| 4. Change the kit | Behaviour every site should get by default | Add a prop, hook or event in the kit repo, then update sites with `kit diff` |
| 5. Edit the copied file in a site | Last resort | Will conflict with later `kit diff` updates |

Rule of thumb: if it would be useful on any site (a loading state, copy-to-clipboard), it belongs in the kit. If it's tied to one site (basket endpoints, product IDs), keep it in the site's own element.

### 16.2 Reference: `KitButton` with extension points

```js
import { Element } from '../../core/element.js';
import { html } from '../../core/html.js';
import { css } from '../../core/css.js';
import { define } from '../../core/define.js';

export class KitButton extends Element {          // exported so sites can extend it
  static formAssociated = true;                   // lets type="submit" work from shadow DOM
  static props = {
    variant: { type: String, default: 'primary', reflect: true },
    type: { type: String, default: 'button' },    // button | submit | reset
    disabled: { type: Boolean, reflect: true },
    loading: { type: Boolean, reflect: true },
    label: { type: String },                      // accessible name for icon-only buttons
  };
  static styles = [css`
    button { background: var(--kit-button-bg, var(--kit-color-primary));
             color: var(--kit-color-on-primary); border: 0;
             border-radius: var(--kit-radius); padding: .5rem 1rem;
             min-block-size: 2.75rem; }
    button:focus-visible { outline: 2px solid var(--kit-focus-ring); outline-offset: 2px; }
    :host([loading]) button { cursor: progress; opacity: .7; }
  `];

  #internals = this.attachInternals();

  #click = (e) => {
    // While loading, swallow the click so site listeners don't fire twice.
    // The button stays focusable (not `disabled`), so keyboard focus isn't lost.
    if (this.loading) { e.stopImmediatePropagation(); e.preventDefault(); return; }
    if (this.type === 'submit') this.#internals.form?.requestSubmit();
    if (this.type === 'reset') this.#internals.form?.reset();
    this.onActivate?.(e);                         // extension point: override in subclasses
  };

  render() {
    // An empty aria-label is ignored, so the slotted text becomes the name.
    return html`<button part="button" type="button"
      ?disabled=${this.disabled}
      aria-busy=${this.loading ? 'true' : 'false'}
      aria-label=${this.label}
      @click=${this.#click}><slot></slot></button>`;
  }
}
define('kit-button', KitButton);
```

Notes:

- `loading` uses `aria-busy` and stays focusable. Pair it with an `announce()` call when the work finishes (section 10.3).
- Props, state and styles are inherited by subclasses automatically (section 5.3), so subclasses don't need to spread the parent's `static props`.
- To add styles in a subclass: `static styles = [...KitButton.styles, css\`...\`]`.

### 16.3 Site example: WordPress shop button

Theme layout (the `kit/` folder is copied in by the CLI and owned by the site):

```
wp-content/themes/mytheme/
  kit/                      # core/, tokens/, components/
  js/shop/
    index.js                # imports and registers site elements
    shop-add-button.js
```

Enqueue as a script module (check the WordPress docs for your version):

```php
add_action('wp_enqueue_scripts', function () {
    wp_enqueue_script_module('mytheme-shop', get_theme_file_uri('js/shop/index.js'), [], '1.0.0');
});
```

The site element:

```js
// js/shop/shop-add-button.js
import { KitButton } from '../../kit/components/button/button.js';
import { define } from '../../kit/core/define.js';
import { announce } from '../../kit/core/a11y.js';

class ShopAddButton extends KitButton {
  static props = { productId: { type: String } };   // becomes the product-id attribute

  async onActivate() {
    this.loading = true;
    try {
      await navigator.clipboard.writeText(this.productId);   // needs https and a user gesture
      await addToBasket(this.productId);                     // your basket endpoint
      announce('Added to basket');
    } catch {
      announce('Could not add to basket', 'assertive');
    } finally {
      this.loading = false;
    }
  }
}
define('shop-add-button', ShopAddButton);
```

Usage: `<shop-add-button product-id="123">Add to basket</shop-add-button>`.

Always import the kit from one canonical path in the theme; the same module URL is one module instance, and `define()` warns instead of throwing if a tag is registered twice.

If the basket call goes through WooCommerce's Store API, check its docs for the nonce requirement.

The delegated-listener alternative (rung 2), when you only need a quick hook:

```js
document.addEventListener('click', async (e) => {
  const btn = e.target.closest('kit-button[data-product-id]');
  if (!btn) return;
  btn.loading = true;
  try { await addToBasket(btn.dataset.productId); announce('Added to basket'); }
  finally { btn.loading = false; }
});
```

Clicks from inside a shadow root are retargeted to the host, so `closest()` works, and one listener covers buttons added later.

### 16.4 Rules for kit authors (so everything stays extendable)

1. **Export the class, then register it** with `define()` as the last line of the file, so sites can import and extend it.
2. **Put behaviour in small named methods** that subclasses can override (`onActivate`, `renderIcon`), and call optional ones with `?.()`.
3. **Emit events for outcomes** (`change`, `close`) and make "before" events cancelable (`kit-before-close`, `preventDefault()` to stop it).
4. **Treat parts and custom properties as public API.** `part` names and `--kit-*` properties are listed in the README and only change with a version bump.
5. **Don't hide extension points behind `#private` fields.** Subclasses can't reach them. Use public or `_underscore` members and document them; keep `#private` for true internals (like `#click` above).
6. **Don't weaken a11y in subclasses.** Keep the inner native element and accessible-name rules; the kit exports each component's keyboard and a11y test suite as a function so a site can run it against its own tag.
7. **Document extension points.** Anything not listed in the README may change in a `kit diff` update.

### 16.5 README template: extension points

```
## Extension points
- Class: `KitButton` (exported from button.js)
- Methods: `onActivate(event)`
- Props: variant, type, disabled, loading, label
- Events: native `click` (bubbles, composed)
- Parts: `button`
- CSS custom properties: `--kit-button-bg`, `--kit-focus-ring`
- Subclass notes: props/styles inherit; spread `KitButton.styles` to add styles
```

## 17. Performance and Core Web Vitals

Performance is a per-component requirement like accessibility: it has budgets, tests in CI, and a place in the definition of done (section 18). The numbers below are starting points to be calibrated once the benchmark harness exists.

### 17.1 Targets

Core Web Vitals "good" thresholds, measured at the 75th percentile of real visits (check web.dev for current values; INP replaced FID in 2024):

| Metric | Good | What the kit does about it |
|---|---|---|
| LCP (Largest Contentful Paint) | 2.5 s or less | The LCP element never waits on a component's JS: it is server-rendered (tier 1 markup or light-DOM fallback). Kit JS is a deferred module, with `modulepreload` for above-the-fold components. |
| INP (Interaction to Next Paint) | 200 ms or less | Handlers do little synchronous work; renders are batched and small; long work yields to the main thread (17.4). |
| CLS (Cumulative Layout Shift) | 0.1 or less | Space is reserved for components that haven't loaded yet; nothing is injected above existing content; fonts don't reflow text (17.5). |

TBT (Total Blocking Time) is the lab proxy for INP, since INP needs real interactions.

### 17.2 Budgets (provisional)

| Item | Budget |
|---|---|
| Core runtime (`element`, `html`, `css`, `define`) | 4 KB gzipped |
| Typical component, JS plus CSS | 3 KB gzipped (hard cap 8 KB) |
| Tokens CSS | 2 KB gzipped |
| Page with 50 kit components | 20 KB gzipped of kit JS in total |
| First render of one component | 5 ms at 4x CPU throttle |
| Update that changes one text node | 2 ms at 4x CPU throttle |
| Mounting 100 buttons | 50 ms at 4x CPU throttle |
| Synchronous work in one event handler | 50 ms maximum, 16 ms target |

CI fails when a file exceeds its size budget or a benchmark regresses by more than 20 percent.

### 17.3 Loading only the components a page uses

Yes, and there are three ways to do it. They can be combined.

**A. The server emits per-page script tags (simplest, no client runtime).** The backend already knows which components it renders: a Blade component can push its module onto a stack, a Go template can add the tag to its page data, and in WordPress you enqueue the script module only when the block or shortcode renders. Above-the-fold components also get `<link rel="modulepreload">`.

**B. A small client auto-loader, `core/loader.js` (about 0.5 KB).** It finds custom elements that aren't defined yet, looks each tag up in a manifest, and imports only that module. It also watches for elements added later (htmx swaps, AJAX), and can defer below-the-fold components until they approach the viewport.

```js
// core/loader.js
// The manifest maps tag -> module URL and is generated by the CLI (kit.manifest.json),
// inlined as <script type="application/json" data-kit-manifest>.
const manifest = JSON.parse(
  document.querySelector('script[data-kit-manifest]')?.textContent ?? '{}'
);
const loading = new Set();

function load(tag) {
  if (!Object.hasOwn(manifest, tag) || customElements.get(tag) || loading.has(tag)) return;
  loading.add(tag);
  import(manifest[tag]).catch((err) => {
    loading.delete(tag);
    console.error(`[kit] failed to load <${tag}>`, err);
  });
}

// Below-the-fold elements marked data-kit-lazy load when they get near the viewport.
const io = new IntersectionObserver((entries) => {
  for (const e of entries) {
    if (!e.isIntersecting) continue;
    io.unobserve(e.target);
    load(e.target.localName);
  }
}, { rootMargin: '200px' });

function scan(node) {
  if (node.nodeType !== 1) return;
  const undefinedEls = [node, ...node.querySelectorAll(':not(:defined)')]
    .filter((el) => el.matches(':not(:defined)'));
  for (const el of undefinedEls) {
    el.hasAttribute('data-kit-lazy') ? io.observe(el) : load(el.localName);
  }
}

scan(document.documentElement);
new MutationObserver((records) => {
  for (const r of records) r.addedNodes.forEach(scan);
}).observe(document.documentElement, { childList: true, subtree: true });
```

The manifest doubles as a security control: the module URL comes from the manifest, never from the DOM, so page content can't make the loader import an arbitrary URL.

**C. Build-time per-page bundles (optional).** For sites where request count matters more than caching, a build step (esbuild or similar) can produce one bundle per page or template, with the core as a shared chunk.

Things to watch:

- **Import waterfalls.** A component imports the core, which imports its parts, so each level can cost a round trip. Fix with `modulepreload` for the core and above-the-fold components, long-lived caching with hashed filenames, and HTTP/2 or HTTP/3.
- **Never lazy-load the LCP component.** If it matters for first paint, render it on the server and load its JS eagerly.
- **Reserve space until upgrade** so lazy loading doesn't cause layout shift (17.5).

### 17.4 Interaction to Next Paint (INP)

INP measures the time from a user interaction (click, tap, key press) until the browser paints the next frame, and the reported value is close to the slowest interaction of a visit. Three parts matter: input delay (the main thread was busy), handler time, and rendering time.

- **Respond first, work second.** Set `loading = true` before any slow work. The batched render runs as a microtask and paints on the next frame, so the user sees feedback immediately.
- **Keep synchronous handler work under 50 ms.** Split long loops and yield between chunks:

```js
export const yieldToMain = () =>
  globalThis.scheduler?.yield ? scheduler.yield() : new Promise((resolve) => setTimeout(resolve));
```

- **Avoid layout thrashing.** Don't read layout (`offsetHeight`, `getBoundingClientRect`) after writing styles in the same task. Do reads in `updated()` and batch them before writes.
- **Keep input handlers cheap.** Never rebuild a whole list per keystroke; debounce expensive derived work.
- **Use passive listeners** for scroll, wheel and touch (`{ passive: true }`).
- **Keep constructors and `connectedCallback` light.** Defer non-critical setup with `requestIdleCallback`, falling back to `setTimeout` where it's missing (Safari).
- **Long lists:** use the keyed `repeat()` helper (phase 2), plus `content-visibility: auto` with `contain-intrinsic-size`, or windowing for very long lists.
- **Cheap animations only.** Animate `transform` and `opacity`; avoid animating layout properties; use `backdrop-filter` and large shadows sparingly on elements that update often.
- **Test with third-party scripts on.** Tag managers, chat widgets and consent banners are often the biggest INP cost on WordPress sites, and they sit outside the kit.

### 17.5 LCP and CLS

**LCP**

- The LCP element (hero image, main heading) is server-rendered and styled by tier 1 CSS, so it doesn't wait for any component's JS.
- Image-bearing components pass through `width`, `height`, `loading` and `fetchpriority`, so the LCP image can be marked `fetchpriority="high"`.
- Module scripts are deferred by default, so they don't block parsing. Tokens and tier 1 CSS are small enough to load as a normal stylesheet in `<head>`.
- Fonts: preload the primary font, use `font-display: swap`, and set `size-adjust` on the fallback font to reduce text reflow.

**CLS**

- Ship each component's tier 1 CSS in the page head, and give undefined elements a reserved size:

```css
kit-button:not(:defined) { display: inline-block; min-block-size: 2.75rem; }
kit-card:not(:defined), kit-tabs:not(:defined) { display: block; min-block-size: 8rem; }
```

- Set width and height (or `aspect-ratio`) on all media; skeleton and loading states match the final size.
- Don't insert content above existing content after load unless the user caused it. Toasts and banners overlay instead of pushing content.

### 17.6 Rendering and memory

- One shared stylesheet per class through `adoptedStyleSheets`, and a template parse cache (both already in the core).
- No leaks on long-lived pages: listeners and tasks abort on disconnect (section 8). CI mounts and unmounts a component 10,000 times and checks that heap size stays flat.
- Each shadow root has a memory cost, so for thousands of tiny repeated items (badges in a big table), use tier 1 CSS-only markup instead of a component per item.
- Components don't create their own `MutationObserver`s; the loader owns the only one.

### 17.7 Measuring

**Lab (CI)**

- Lighthouse CI on the demo pages, with budgets for LCP, CLS and TBT.
- Playwright scripts that perform the real interactions (click, type, open dialog) while recording `event` timing entries, or the `web-vitals` library's INP reading, and assert that results stay within the budget under 4x CPU throttling.
- Benchmarks for mount and update counts against the table in 17.2.
- Size checks per file (gzip size) with a tool like `size-limit`.

**Field (real users)**

- Add the `web-vitals` library with attribution and send results to your analytics. INP attribution names the element and the slow phase, which finds slow kit components quickly.
- Use CrUX or the Search Console Core Web Vitals report for the real-world picture, since INP depends heavily on devices and third-party scripts.
- The Long Animation Frames API (Chromium) shows which scripts block slow interactions.

### 17.8 Anti-patterns to avoid

- Re-rendering whole lists on every state change without keys.
- Doing network or heavy computation in a constructor or `connectedCallback`.
- Rendering the LCP element from JS.
- Loading every component on every page "just in case".
- Reading layout inside `render()`.
- Measuring only on a fast laptop; always throttle.

## 18. Definition of done (all pillars)

A component is done only when all four lists pass. Each list is also run in CI wherever it can be automated.

**Accessibility:** see section 10.7.

**Security**

- [ ] The XSS corpus passes, with hostile values in every prop, attribute and slot
- [ ] No unsafe sinks: lint fails on `innerHTML`, `outerHTML`, `insertAdjacentHTML`, `document.write`, `eval`, `new Function` and string `setTimeout`
- [ ] Works under a strict CSP with Trusted Types enforced (the demo page sends `script-src 'self'; require-trusted-types-for 'script'`)
- [ ] URL-bearing attributes go through the validator; no `on*`, `style` or `srcdoc` bindings
- [ ] No network requests, storage or cookies unless documented in the README
- [ ] No third-party runtime dependencies (or each is listed with version and reason)
- [ ] Registry hash updated and verified by `kit add`

**Performance**

- [ ] Within its gzipped size budget (17.2), checked in CI
- [ ] Render and update benchmarks within budget under 4x CPU throttling
- [ ] No layout shift: a `:not(:defined)` rule reserves space, and the demo page scores CLS 0.1 or less
- [ ] Event handlers do under 50 ms of synchronous work; scripted INP on the demo page is 200 ms or less under throttling
- [ ] 10,000 mount and unmount cycles leave the heap flat
- [ ] Safe to lazy-load: importing the module has no side effects beyond `define()`

**Extensibility:** see section 16.4 and the extension-points checklist item in 10.7.
