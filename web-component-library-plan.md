# Kit: review, implementation plan and build choices

Companion to `web-component-library-spec.md` (draft v0.1). Written 2 November 2026.

The aim, in your words: one UI library that everything you build uses (Kritano's app and sites, chrisgarlick.com, future apps), shown off on **ui.chrisgarlick.com** as a fully interactive site. Premium, sleek and beautiful; very light and very fast; and good enough for **data analysis** (dense tables, charts, dashboards), not just forms and buttons.

This document has four parts:

1. A deeper review of the spec (what to change before writing code)
2. Go vs Rust (and where each would actually run)
3. How ui.chrisgarlick.com is built and hosted on a small server
4. The implementation plan, phase by phase, with "done when" for each

---

## 1. Review of the spec

The spec is strong: the three tiers, the security rules, the async table, the accessibility chapter and the definition of done are the right foundations, and the copy-in model suits a library you'll use across several stacks. What follows is what I'd change before phase 1, ordered by how much it matters.

### 1.1 The data-analysis goal isn't in the spec yet

The spec is written for app chrome (buttons, dialogs, tabs). Data analysis needs things the core can't do today:

| Need | Gap in the spec | Change |
|---|---|---|
| Charts are SVG | Templates parse in an HTML `<template>`, so an SVG fragment returned on its own (`<path>`, `<g>`) is created in the HTML namespace and doesn't render | Add an `svg` tag alongside `html` (parse inside `<svg>`, unwrap) |
| Bar widths, positions, colours driven by data | `style` bindings are blocked, and the spec says "use custom properties", but there's no safe way to set one per element from a template | Add a `styleMap`-style directive that only calls `el.style.setProperty('--x', value)` through the CSSOM (CSP-safe, no CSS parsing of user input). Allow `--*` names only |
| Tables with 10,000+ rows | Arrays re-render fully; keyed `repeat()` is phase 2 | Keyed `repeat()` moves to the core in phase 1. The data grid doesn't use the template renderer per cell at all: it recycles a fixed pool of row elements (virtualisation) |
| Export (CSV, PNG) | `safeUrl` turns `blob:` URLs into `#` | Allow same-origin `blob:` for `href` and `download` |
| Heavy work (grouping 1M rows) | Nothing on workers | A small worker helper: transferable typed arrays, latest-wins via `run()` |
| Scroll performance | `@` bindings can't take listener options | Support `@scroll.passive`, `@touchstart.passive` (and `.capture`, `.once`) |
| Numbers and dates | Not covered | `Intl.NumberFormat` / `DateTimeFormat` wrappers with tabular figures; locale from `<html lang>` |
| Density | Not covered | A `--kit-density` token (comfortable, compact) that every component respects; data UIs default to compact |

### 1.2 Bugs and holes in the reference code

1. **`.innerHTML=${value}` is allowed.** Property bindings block `on*`, `style` and `srcdoc`, but not `innerHTML`, `outerHTML` or `innerText` on script-like elements. That breaks the promise that the easy path can't produce XSS. Block `innerHTML` and `outerHTML` in both forms, and add them to the XSS corpus.
2. **The "inside a tag" detection is a heuristic, and it breaks on ordinary markup** (`title="a > b"`). The spec lists it as a known limit, but it's too common to leave. Replace it with a small scanner over the static strings (text, tag, attribute-name, quoted value), as lit-html does.
3. **The base class is called `Element`**, which shadows the browser's `Element` in every module that imports it (`instanceof Element` breaks, and it reads badly). Call it `KitElement`.
4. **Default values are never reflected.** Reflection runs only for changed properties, so `variant: 'primary', reflect: true` never appears as an attribute, and `:host([variant=primary])` CSS never matches until someone sets it. Reflect reflected defaults on the first update.
5. **`announce()` breaks inside modal dialogs.** `showModal()` makes everything outside the dialog inert, including the live region appended to `<body>`, so announcements made while a dialog is open aren't read. Keep a live region inside each open dialog (the dialog component owns one), and fall back to the body region otherwise.
6. **One live region switches between polite and assertive.** Screen readers don't reliably notice a politeness change on an existing region. Use two fixed regions.
7. **The loader can't see inside shadow roots.** A dialog that renders a `kit-button` in its shadow root never gets it loaded. Components import their own dependencies statically (the registry's `dependsOn` already says what they are); the loader is for top-level tags only.
8. **Implicit form submission doesn't work.** Pressing Enter in a field won't submit through a `kit-button type="submit"`, because the browser doesn't treat a custom element as a submit button. Either render a hidden native submit button in light DOM, or document it as a known limit.
9. **The ~50-cycle guard for state set in `updated()`** is described but not implemented.
10. **Two kit versions on one page** (two WordPress plugins with their own copies) silently keep whichever registered first. The configurable prefix fixes it; `define()` should say which version won.
11. **`kit diff` can't merge.** `kit.lock` stores hashes, not the files the copy started from, so a three-way merge of upstream changes into local edits isn't possible. Cache the base version's files (`.kit/base/<version>/`).
12. **The 4 KB core budget won't survive** `svg`, keyed `repeat()`, `styleMap`, listener options and the dev checks. Budget 6 KB gzipped for the core (still far below Lit's ~6–7 KB plus directives), with dev-only checks stripped from the production build.

### 1.3 Things the spec doesn't decide

- **Browser support.** State it: the last two versions of Chrome, Edge, Firefox and Safari (desktop and iOS), tested in Playwright's Chromium, Firefox and WebKit. That makes the newer APIs below usable, each with a fallback where noted.
- **Theming per app.** One kit, several brands: a neutral default plus a `kritano` theme (indigo, amber, Outfit and Instrument Serif) and a `chrisgarlick` theme. Light and dark for each, chosen by `prefers-color-scheme` or an explicit `data-theme`, exactly as Kritano's app now does it.
- **Colour.** Define the palette in OKLCH and use `light-dark()` so each token is declared once for both schemes. The Go CLI converts to sRGB to check contrast.
- **Positioning** (menus, popovers, tooltips, date pickers). CSS anchor positioning where supported; a ~1.5 KB fallback for the rest. Popover API for the top layer.
- **Icons.** One SVG sprite of the icons the kit and your apps use (a subset of an ISC/MIT set such as Lucide), with `aria-hidden` by default.
- **Versioning.** Semver for the kit; parts, custom properties and events are public API (the spec already says so); a deprecation note lasts one minor version.
- **Licence and name.** Needed before the registry is public. Placeholder `kit` throughout this document.
- **Chart accessibility.** Charts need their own section: every chart ships a visually hidden data table (or a "Show as table" toggle), a text summary, and keyboard navigation between points. Colour palettes for series must pass 3:1 against the background and stay distinguishable for colour-blind users (patterns or labels, not colour alone).
- **Print.** Data reports get printed. Tier 1 CSS includes print styles (no shadows, dark-on-white, full tables).

---

## 2. Go or Rust?

### 2.1 Where code actually runs

| Place | Language | Why |
|---|---|---|
| The browser: every component | **Plain JavaScript** (ES modules, JSDoc types) | Smallest download, no build step for consumers, best INP and LCP. Rust or Go UI frameworks compiled to WebAssembly (Leptos, Yew, Go's wasm) cost 100 KB to 2 MB before the first component, which is the opposite of the goal |
| The browser: heavy number-crunching (optional, later) | **Rust → WebAssembly**, loaded only when needed | If profiling ever shows JavaScript plus workers isn't enough (very large group-bys, statistics on millions of rows), Rust produces small, fast WASM (tens of KB) with SIMD. Go's WASM is ~2 MB; TinyGo is smaller but slower and patchy |
| Tooling: CLI, build, registry, site generator, dev server | **Go** | See below |
| Browser tests | **Playwright** (Node) | Real Chromium, Firefox and WebKit. You already run it in cms and Kritano |

### 2.2 Why Go for the tooling

- **It's your stack.** cms and Kritano are Go, with the same lint, test and release habits. One toolchain to keep up to date, on a Mac with little disk to spare.
- **esbuild is written in Go and works as a library.** The CLI can minify and bundle JavaScript and CSS in-process (`github.com/evanw/esbuild/pkg/api`), with no Node in the build. It's the fastest bundler there is.
- **The other tools are in Go's standard library or close to it:** gzip sizes (stdlib), Brotli sizes (a pure-Go package), SHA-256 hashing, signing (minisign has a Go library), templates for the site (`html/template`, contextual escaping), a dev server with live reload (`net/http`), and embedding (`//go:embed`).
- **A single static binary** for `kit init`, `kit add`, `kit verify` and `kit diff`, cross-compiled for Mac and Linux.
- **Dogfooding Kritano.** Kritano's audit engine is Go with Chromium. CI can audit every page of ui.chrisgarlick.com with it (accessibility, security headers, performance) alongside axe.

### 2.3 What Rust would buy you, and why it isn't worth it for the tooling

Rust has the best CSS tooling (Lightning CSS) and the oxc parser/minifier, and its binaries are a little smaller. None of that outweighs a second language for a one-person project: esbuild's CSS minifier is good enough for hand-written token CSS, and if Lightning CSS's syntax lowering is ever needed, the Go CLI can call its binary.

**Recommendation:** Go for everything outside the browser; plain JavaScript inside it; Rust only as an optional, lazily loaded WASM module for heavy data work, decided by benchmarks in phase 6, not up front.

---

## 3. ui.chrisgarlick.com on a small server

The chrisgarlick.com server has 1 CPU and about 1 GB of memory (460 MB free), already running the site, the queue worker and Typeset. So the UI site adds **no process at all**:

- **Fully static.** `kit site build` (Go) generates every page, demo and the registry into `dist/` on your Mac or in CI. The server only stores files.
- **Served by the nginx that's already there**, as one more server block: `gzip_static` with pre-compressed `.gz` files (that nginx has no Brotli module), hashed filenames cached for a year, HTML revalidated. Memory cost: effectively zero.
- **Interactivity is all client-side**: live demos, props controls, theme switching, the playground and the dashboards run in the browser. Heavy pages (the code editor in the playground, big datasets) load their modules only on those pages.
- **Deploy** with `kit site deploy`: rsync to a new release folder, then switch a symlink (atomic, instant rollback). No database, no PHP, no Docker.
- **The site is also the registry.** `ui.chrisgarlick.com/r/registry.json` (signed) plus each file at a hashed URL, so `kit add` installs from the same place people browse.
- **Optional later:** put Cloudflare in front for a global edge cache. Not needed for the traffic you'll have.

### What makes it premium

Light and fast is the foundation, but "premium" is mostly craft in the design system:

- **Typography:** one distinctive variable sans for UI, a mono for code and figures, tabular numbers everywhere data appears, a modular type scale, and optical sizing.
- **Colour:** OKLCH scales with perceptually even steps, a dark mode designed in its own right rather than inverted, and restrained accent use.
- **Depth:** layered, soft shadows and 1 px alpha borders, as on the Kritano redesign; elevation as tokens.
- **Motion:** short, purposeful transitions with designed easing curves (CSS `linear()` springs), View Transitions on the site, and everything off under `prefers-reduced-motion`.
- **Detail:** focus rings that look designed, not default; hover and press states on everything; skeletons that match final sizes; empty states and error states as first-class components.
- **Density:** comfortable for marketing and forms, compact for data. Dashboards look calm even when packed.

### Site map

- **Home:** a showcase of live components and a dashboard teaser.
- **Foundations:** colour, type, spacing, elevation, motion, density, icons, themes (with a live theme switcher across the whole site).
- **One page per component:**
  - a live demo, with props controls generated from `static props`;
  - code tabs for HTML, Blade, Go templates and WordPress;
  - the accessibility contract and keyboard table;
  - the extension points;
  - the size (gzipped, from CI) and the test status.
- **Data:** charts, the data grid, and a gallery of full dashboards built on real public datasets.
- **Playground:** edit a snippet, see it live (sandboxed iframe, strict CSP).
- **Theme builder:** pick brand colours and fonts, see every component re-theme, check contrast, export `tokens.css`.
- **Get started:** the CLI, the copy-in model, per-backend setup. Plus a changelog.

---

## 4. Implementation plan

Each phase ends in something usable, and each has the spec's definition of done as its quality gate. Rough sizes are relative (S, M, L, XL), not dates.

### Phase 0: decisions (S)

Make the calls the spec leaves open, so nothing is rebuilt later:

- name, tag prefix and licence
- repo: its own repo (recommended: `~/dev/kit`, `Chrisgarlick/kit`), not inside cms
- browser support matrix (1.3)
- shadow DOM default: shadow for self-contained widgets, light DOM for form controls (as the spec leans)
- loading default per backend: server-emitted tags for Go and Laravel; the loader for WordPress
- typefaces and the first two brand themes
- fold the review (part 1) into the spec as v0.2

**Done when** the spec is v0.2 with every open decision answered.

### Phase 1: core and harnesses (L)

The engine and the safety nets every later component inherits.

- **Core:**
  - `KitElement`, `html`, `svg`, `css` and `define`, with the fixes from 1.2: a proper template scanner, reflected defaults, blocked `innerHTML`, a loop guard and listener options;
  - keyed `repeat()` and `styleMap` (custom properties only);
  - `a11y.js`, with per-dialog live regions and two politeness regions;
  - dev checks that are stripped from production.
- **Go CLI skeleton:** `kit build` (esbuild minify, per-file gzip and Brotli sizes, hashed filenames, manifest), `kit check` (size budgets, unsafe-sink lint).
- **Test harness:** Playwright component tests in three engines, the XSS corpus runner, race tests with a controllable fake `fetch`, render benchmarks at 4x CPU throttle, and a 10,000-cycle mount/unmount heap check.
- **Tokens v1:** colour (OKLCH, `light-dark()`), type, space, radius, elevation, motion, density; `kit check-contrast` in Go.

**Done when** the core tests, XSS corpus and benchmarks pass in all three engines, the core is under 6 KB gzipped, and a counter works in a Go template, a Blade view and a WordPress page.

### Phase 2: design system and static components (M)

The look, before the behaviour.

- **Final tokens and themes:** neutral, `kritano` and `chrisgarlick`, each light and dark.
- **Tier 1 CSS:**
  - type, layout primitives (stack, cluster, grid, sidebar), surfaces and cards, badge, alert, avatar, kbd, skeleton, empty state;
  - table styles (tier 1, for server-rendered tables), form field styles, print styles.
- **The icon sprite.**
- **A visual-regression baseline** for every theme.

**Done when** a static page built only from tier 1 CSS looks finished in all six theme/scheme combinations, and contrast passes everywhere.

### Phase 3: interactive components (L)

APG patterns, form-associated where they hold values.

- **Actions:** button (with `onActivate`, loading and icon-only label), icon button, segmented control.
- **Overlays:** dialog, drawer, popover, menu and dropdown, tooltip, toast (via `announce`).
- **Navigation:** tabs, disclosure and accordion, breadcrumbs, pagination.
- **Form controls:** text field, textarea, checkbox, radio group, switch, select, combobox with filtering, slider, date picker, file drop.
- **Command palette:** Kritano's ⌘K, generalised.
- **The positioning helper** (anchor positioning plus fallback).

**Done when** every component meets the definition of done, including the manual screen-reader pass, and a sample form submits natively with validation messages.

### Phase 4: ui.chrisgarlick.com v1 and the registry (M)

- **`kit site build`:**
  - Go templates that read each component's README, demo and metadata;
  - generated props controls, code tabs per backend, sizes and test status;
  - the foundations pages and the theme switcher.
- **`kit init`, `kit add`, `kit verify`, `kit diff`:** signed registry, SHA-256 per file, cached base versions for three-way diffs.
- **Hosting:** an nginx block on the chrisgarlick.com server, plus `kit site deploy` (rsync and symlink switch).
- **CI:** builds the site, runs Lighthouse CI and axe on every page, and audits it with Kritano.

**Done when** ui.chrisgarlick.com is live, every page scores LCP ≤ 2.5 s, CLS ≤ 0.1 and INP ≤ 200 ms (lab, throttled), and `kit add button` installs a verified copy into a fresh project.

### Phase 5: data analysis (XL)

The part that makes the kit different.

- **Data layer:**
  - a columnar dataset type (typed arrays for numbers);
  - a small scale module (linear, log, time, band) with "nice" ticks;
  - `Intl` formatters;
  - aggregation (group-by, bin, rollups) in a worker with transferable arrays.
- **Charts:**
  - SVG first: line, area, bar (grouped, stacked), sparkline, scatter, histogram, donut, heatmap;
  - Canvas renderer for large point counts, with downsampling (largest-triangle-three-buckets for lines);
  - shared axes, legends, crosshair tooltips and brushing to zoom;
  - accessible by default (data table, summary, keyboard navigation).
- **Data grid:**
  - virtualised rows and columns;
  - sort, filter, column resize, reorder and pin;
  - selection, the APG grid keyboard model, sticky header, totals row;
  - CSV export.
- **Dashboard pieces:** stat tiles with deltas, filter bar, date-range picker with presets, layout grid.
- **Budgets:**
  - charts core ≤ 12 KB gzipped, each chart type ≤ 3 KB on top;
  - grid ≤ 15 KB gzipped;
  - 100,000-row grid scrolls at 60 fps;
  - a 50,000-point line chart renders in under 50 ms at 4x throttle.

  For scale: Chart.js is about 65 KB and ECharts several hundred.
- **Dashboard gallery on the site,** using real public datasets.

**Done when** the budgets hold in CI, charts and the grid pass the definition of done (including screen-reader passes), and the gallery dashboards meet the Core Web Vitals targets.

### Phase 6: adoption, then the advanced parts (L, ongoing)

- **Kritano's app first:**
  - replace its hand-built pieces: score chart and sparklines, ⌘K palette, tables, menus, dialogs and tabs;
  - then the Kritano and docs sites through cms themes;
  - then chrisgarlick.com.

  Each move is measured: page weight and Core Web Vitals before and after.
- **Go helpers for cms and Kritano templates,** so tier 1 markup and per-page script tags come from one partial.
- **Declarative shadow DOM** for server-rendered first paint of tier 2 components.
- **Rust WASM kernels, only if phase 5's benchmarks show the need.**
- **Arrow and Parquet import,** lazily loaded.

**Done when** Kritano's app ships no UI code of its own outside site-specific subclasses.

### Order of work, in one line

0 → 1 → 2 → 3 → 4 (site goes live with the core components) → 5 (data) → 6 (adoption). Phases 2 and 3 can overlap once the core is stable; the site can go live as early as phase 2 if you want something to show.

---

## 5. Decisions needed from you

1. Name, tag prefix and licence.
2. Own repo at `~/dev/kit` (recommended)?
3. Typefaces: a distinctive UI sans plus a mono. Self-hosted, licensed for the web.
4. Go for tooling, plain JS in the browser, Rust only if benchmarks demand it (recommended)?
5. Which comes first after the core: the design system and site (phases 2–4), or data components (phase 5) because Kritano needs them?
