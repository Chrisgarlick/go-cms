/*
 * Public forms (spec 4.8). Include with
 *   <script src="{{.Asset "forms.js"}}" defer></script>
 * and mark a form up as
 *   <div data-cms-form-container>
 *     <form method="post" action="/forms/contact" data-cms-form="contact" novalidate>
 *       <div class="hp" aria-hidden="true"><input name="_hp" tabindex="-1" autocomplete="off"></div>
 *       … fields, each followed by <span data-error="field-name" hidden></span> …
 *       <p data-form-error hidden>Something went wrong.</p>
 *       <button type="submit">Send</button>
 *     </form>
 *     <div data-form-success hidden tabindex="-1"><p></p></div>
 *   </div>
 *
 * The page stays cacheable: the signed token is fetched when someone starts
 * filling the form in, not baked into the HTML.
 */
(() => {
  // Shows or hides an element whichever way the theme hides it ([hidden], .hidden or display:none).
  const show = (el, on) => {
    if (!el) return;
    el.hidden = !on;
    el.classList.toggle('hidden', !on);
    el.style.display = '';
    if (on && getComputedStyle(el).display === 'none') el.style.display = 'block';
  };

  async function token(form) {
    let input = form.querySelector('input[name="_token"]');
    if (input && input.value) return;
    if (!input) {
      input = document.createElement('input');
      input.type = 'hidden';
      input.name = '_token';
      form.append(input);
    }
    if (!form._cmsToken) {
      form._cmsToken = fetch(`/forms/${encodeURIComponent(form.dataset.cmsForm)}/token`, { headers: { Accept: 'application/json' }, credentials: 'omit' })
        .then((r) => (r.ok ? r.json() : {}))
        .then((d) => { input.value = d.token || ''; })
        .catch(() => {})
        .finally(() => { form._cmsToken = null; });
    }
    await form._cmsToken;
  }

  function errorFor(form, name) { return form.querySelector(`[data-error="${CSS.escape(name)}"]`); }

  function clearErrors(form) {
    form.querySelectorAll('[data-error]').forEach((el) => { show(el, false); });
    form.querySelectorAll('[aria-invalid]').forEach((el) => el.removeAttribute('aria-invalid'));
    show(form.querySelector('[data-form-error]'), false);
  }

  function fieldError(form, name, message) {
    const el = errorFor(form, name);
    const input = form.elements[name];
    if (input && input.setAttribute) {
      input.setAttribute('aria-invalid', 'true');
      if (el?.id) input.setAttribute('aria-describedby', el.id);
    }
    if (el) {
      if (message) el.textContent = message;
      show(el, true);
    }
    return input;
  }

  function validate(form) {
    let first = null;
    for (const input of form.querySelectorAll('input, textarea, select')) {
      if (!input.name || input.name.startsWith('_') || input.type === 'hidden') continue;
      if (input.checkValidity()) continue;
      const el = errorFor(form, input.name);
      // Keep the theme's own "is required" text; explain other problems.
      if (el && !el.dataset.required) el.dataset.required = el.textContent.trim();
      const msg = input.validity.valueMissing ? (el?.dataset.required || 'This field is required.')
        : input.validity.typeMismatch && input.type === 'email' ? 'Enter a valid email address.'
          : input.validity.tooShort ? `Please write at least ${input.minLength} characters.` : input.validationMessage;
      const f = fieldError(form, input.name, msg);
      first = first || f;
    }
    if (first) first.focus();
    return !first;
  }

  function init(form) {
    const container = form.closest('[data-cms-form-container]') || form.parentElement;
    const success = container.querySelector('[data-form-success]');
    const failure = form.querySelector('[data-form-error]');
    const button = form.querySelector('button[type="submit"]');
    const label = button?.textContent;
    form.noValidate = true;
    form.addEventListener('focusin', () => token(form), { once: true });

    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      clearErrors(form);
      if (!validate(form)) return;
      if (button) { button.disabled = true; button.textContent = button.dataset.busy || 'Sending…'; }
      form.setAttribute('aria-busy', 'true');
      try {
        await token(form);
        const res = await fetch(form.action, {
          method: 'POST',
          headers: { Accept: 'application/json', 'Content-Type': 'application/x-www-form-urlencoded' },
          body: new URLSearchParams(new FormData(form)),
          credentials: 'omit',
        });
        const body = await res.json().catch(() => ({}));
        if (res.ok) {
          const p = success?.querySelector('p') || success;
          if (p && body.message && !p.dataset.keep) p.textContent = body.message;
          form.reset();
          show(form, false);
          show(success, true);
          success?.focus();
          return;
        }
        if (res.status === 422 && body.errors) {
          let first = null;
          for (const [name, msg] of Object.entries(body.errors)) first = first || fieldError(form, name, String(msg));
          first?.focus?.();
        } else if (failure) {
          if (body.error && !failure.dataset.keep) failure.textContent = body.error;
        }
        if (res.status !== 422) show(failure, true);
        // A rejected token is single-use from the client's point of view: get a fresh one.
        const t = form.querySelector('input[name="_token"]');
        if (t && res.status === 400) t.value = '';
      } catch {
        show(failure, true);
      } finally {
        form.removeAttribute('aria-busy');
        if (button) { button.disabled = false; button.textContent = label; }
      }
    });
  }

  const start = () => document.querySelectorAll('form[data-cms-form]').forEach(init);
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', start);
  else start();
})();
