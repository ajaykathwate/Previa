// Progressive enhancement: native menus, dialogs, and forms are the baseline.
(() => {
  document.documentElement.classList.add('js-enabled');
  function initMotion() {
    const motion = window.matchMedia('(prefers-reduced-motion: reduce)');
    const reveals = document.querySelectorAll('[data-scroll-reveal]');
    let revealObserver;
    if ('IntersectionObserver' in window && !motion.matches) {
      revealObserver = new IntersectionObserver((entries) => {
        entries.forEach(({target, isIntersecting}) => {
          if (!isIntersecting) return;
          target.classList.add('is-visible');
          revealObserver.unobserve(target);
        });
      }, {threshold: 0.02});
      reveals.forEach((element) => {
        if (element.getBoundingClientRect().top < window.innerHeight) return;
        element.classList.add('scroll-pending');
        revealObserver.observe(element);
      });
    }
    const illustration = document.querySelector('.preview-window');
    let illustrationVisible = true;
    const updateAnimation = () => {
      if (illustration) illustration.style.animationPlayState =
        illustrationVisible && !document.hidden && !motion.matches ? 'running' : 'paused';
    };
    if (illustration && 'IntersectionObserver' in window) {
      new IntersectionObserver(([entry]) => {
        illustrationVisible = entry.isIntersecting;
        updateAnimation();
      }).observe(illustration);
    }
    document.addEventListener('visibilitychange', updateAnimation);
    motion.addEventListener('change', () => {
      if (motion.matches) {
        revealObserver?.disconnect();
        reveals.forEach(element => element.classList.remove('scroll-pending', 'is-visible'));
      }
      updateAnimation();
    });
    updateAnimation();
  }

  function initNavigation() {
    document.querySelectorAll('[data-mobile-nav] a').forEach(link => {
      link.addEventListener('click', () => { link.closest('details').open = false; });
    });
    document.addEventListener('click', (event) => {
      const backdrop = event.target.closest('[data-mobile-nav-close]');
      if (backdrop) backdrop.closest('details').open = false;
    });
    document.addEventListener('keydown', (event) => {
      if (event.key !== 'Escape' || document.querySelector('dialog[open]')) return;
      document.querySelectorAll('[data-mobile-nav][open]').forEach((nav) => {
        nav.open = false;
        nav.querySelector('summary').focus();
      });
    });
    const mac = /Mac|iPhone|iPad/.test(navigator.userAgentData?.platform || navigator.platform || navigator.userAgent);
    if (mac) document.querySelectorAll('[data-shortcut-label]').forEach(label => { label.textContent = '⌘K'; });
  }

  // Theme choice is a per-browser preference; without a choice the OS theme applies.
  const themeQuery = window.matchMedia('(prefers-color-scheme: dark)');
  const effectiveTheme = () => document.documentElement.dataset.theme || (themeQuery.matches ? 'dark' : 'light');
  function labelThemeToggles() {
    const next = effectiveTheme() === 'dark' ? 'light' : 'dark';
    document.querySelectorAll('[data-theme-toggle]').forEach((toggle) => {
      toggle.setAttribute('aria-label', `Switch to ${next} theme`);
      toggle.title = `Switch to ${next} theme`;
    });
  }
  function toggleTheme() {
    const next = effectiveTheme() === 'dark' ? 'light' : 'dark';
    document.documentElement.dataset.theme = next;
    try { localStorage.setItem('previa-theme', next); } catch { /* Storage may be unavailable. */ }
    labelThemeToggles();
  }
  function initTheme() {
    labelThemeToggles();
    themeQuery.addEventListener('change', labelThemeToggles);
    document.addEventListener('click', (event) => {
      if (event.target.closest('[data-theme-toggle]')) toggleTheme();
    });
  }

  // Prefetch workspace pages on hover so ordinary navigation feels immediate.
  // HTMX-driven links, mutations and authentication routes are never prefetched.
  function initPrefetch() {
    if (!document.querySelector('.app-shell') || !HTMLScriptElement.supports?.('speculationrules')) return;
    const rules = document.createElement('script');
    rules.type = 'speculationrules';
    rules.textContent = JSON.stringify({prefetch: [{
      source: 'document',
      where: {and: [
        {href_matches: '/*'},
        {not: {href_matches: ['/', '/auth/*', '/installations/*', '/queue/*', '/webhooks/*', '/static/*']}},
        {not: {selector_matches: '[target], [download], [hx-get], [hx-post]'}},
      ]},
      eagerness: 'moderate',
    }]});
    document.head.append(rules);
  }

  const dialogReturnTargets = new WeakMap();
  const openDialog = (dialog, trigger) => {
    if (!(dialog instanceof HTMLDialogElement) || dialog.open) return;
    const menu = trigger?.closest('[data-menu]');
    dialogReturnTargets.set(dialog, menu ? menu.querySelector('summary') : trigger);
    if (menu) menu.open = false;
    dialog.showModal();
  };

  function initConfirmationDialogs() {
    const boundDialogs = new WeakSet();
    document.addEventListener('click', (event) => {
      const trigger = event.target.closest('[data-open-dialog]');
      if (trigger) {
        event.preventDefault(); // Prevent duplicate native command handling.
        openDialog(document.getElementById(trigger.dataset.openDialog), trigger);
      }
      const dismiss = event.target.closest('[data-close-dialog]');
      if (dismiss) {
        event.preventDefault();
        dismiss.closest('dialog').close();
      }
    });
    const bindDialogs = () => document.querySelectorAll('[data-confirmation-dialog]').forEach(dialog => {
      if (boundDialogs.has(dialog)) return;
      boundDialogs.add(dialog);
      // Keep Tab inside the dialog even where the browser includes its own chrome
      // in the native dialog's focus cycle. Native inert behavior remains the base.
      dialog.addEventListener('keydown', (event) => {
        if (event.key !== 'Tab') return;
        const controls = [...dialog.querySelectorAll(
          'button:not(:disabled), a[href], input:not([type="hidden"]):not(:disabled), select:not(:disabled), textarea:not(:disabled), [tabindex]:not([tabindex="-1"])'
        )].filter(element => element.getClientRects().length > 0);
        const first = controls[0];
        const last = controls[controls.length - 1];
        if (event.shiftKey && document.activeElement === first) {
          event.preventDefault(); last.focus();
        } else if (!event.shiftKey && document.activeElement === last) {
          event.preventDefault(); first.focus();
        }
      });
      dialog.addEventListener('close', () => {
        const target = dialogReturnTargets.get(dialog);
        if (target?.isConnected) target.focus({preventScroll: true});
        dialogReturnTargets.delete(dialog);
      });
      dialog.addEventListener('click', (event) => {
        if (event.target !== dialog) return;
        const rect = dialog.getBoundingClientRect();
        if (event.clientX < rect.left || event.clientX > rect.right ||
            event.clientY < rect.top || event.clientY > rect.bottom) dialog.close();
      });
    });
    bindDialogs();
    document.addEventListener('htmx:load', bindDialogs);
    document.addEventListener('htmx:historyRestore', bindDialogs);
    if (new URLSearchParams(window.location.search).get('confirm') === 'logout') {
      openDialog(document.getElementById('logout-confirmation'), document.querySelector('.account-menu summary'));
    }
  }

  function initAccountMenus() {
    const menus = document.querySelectorAll('[data-menu]');
    document.addEventListener('click', (event) => {
      menus.forEach((menu) => {
        if (!menu.contains(event.target)) menu.open = false;
      });
    });
    document.addEventListener('keydown', (event) => {
      if (event.key !== 'Escape' || document.querySelector('[data-confirmation-dialog][open]')) return;
      menus.forEach((menu) => {
        if (menu.open) {
          menu.open = false;
          menu.querySelector('summary').focus();
        }
      });
    });
  }

  function initQueueForms() {
    document.addEventListener('submit', async (event) => {
      const form = event.target;
      if (!(form instanceof HTMLFormElement) || !form.matches('[data-queue-form]')) return;
      event.preventDefault();
      if (form.dataset.pending === 'true') return;
      const output = form.querySelector('[data-queue-result]');
      const submit = form.querySelector('button[type="submit"]');
      form.dataset.pending = 'true';
      submit.disabled = true;
      form.setAttribute('aria-busy', 'true');
      output.dataset.state = 'pending';
      output.textContent = 'Sending your ping…';
      const controller = new AbortController();
      const timeout = setTimeout(() => controller.abort(), 15000);
      try {
        const response = await fetch(form.action, {
          method: 'POST', credentials: 'same-origin', redirect: 'manual', signal: controller.signal,
          body: new URLSearchParams(new FormData(form)),
          headers: {Accept: 'application/json'},
        });
        if (response.type === 'opaqueredirect') throw new Error('Your session has ended. Sign in again, then retry.');
        if (!response.ok) {
          if (response.status === 403) throw new Error('Your session check failed. Refresh the page, then retry.');
          if (response.status === 400) throw new Error('Check the key: use letters, numbers, or _ . / : # - without spaces.');
          throw new Error('The ping could not be queued. Please try again.');
        }
        const result = await response.json();
        if (!Number.isSafeInteger(result.job_id) || typeof result.duplicate !== 'boolean') throw new Error('The queue returned an unexpected response. Refresh before retrying.');
        output.dataset.state = 'success';
        output.textContent = result.duplicate
          ? `An active ping already exists for this key. Job #${result.job_id}.`
          : `Ping queued. Job #${result.job_id}. Your worker will process it.`;
      } catch (error) {
        output.dataset.state = 'error';
        output.textContent = error instanceof TypeError
          ? 'Could not reach the server. Check your connection and try again.'
          : error.name === 'AbortError'
            ? 'The request timed out. Retry with the same key to avoid a duplicate active job.'
            : error.message;
      } finally {
        clearTimeout(timeout);
        delete form.dataset.pending;
        submit.disabled = false;
        form.removeAttribute('aria-busy');
      }
    });
  }

  function initCommandPalette() {
    const palette = document.querySelector('[data-command-palette]');
    if (!palette) return;
    const input = palette.querySelector('#command-input');
    const items = [...palette.querySelectorAll('[data-command-item]')];
    const groups = [...palette.querySelectorAll('.command-group')];
    const empty = palette.querySelector('[data-command-empty]');
    let active = null;
    let returnTarget = null;
    const visible = () => items.filter(item => !item.hidden);
    const select = (item) => {
      active?.setAttribute('aria-selected', 'false');
      active = item;
      if (!item) { input.removeAttribute('aria-activedescendant'); return; }
      item.setAttribute('aria-selected', 'true');
      input.setAttribute('aria-activedescendant', item.id);
      item.scrollIntoView({block: 'nearest'});
    };
    const filter = () => {
      const terms = input.value.trim().toLowerCase().split(/\s+/).filter(Boolean);
      items.forEach((item) => {
        const text = item.dataset.keywords.toLowerCase();
        item.hidden = !terms.every(term => text.includes(term));
      });
      groups.forEach((group) => { group.hidden = !group.querySelector('[data-command-item]:not([hidden])'); });
      empty.hidden = visible().length > 0;
      select(visible()[0] || null);
    };
    const open = (trigger) => {
      if (palette.open || document.querySelector('[data-confirmation-dialog][open]')) return;
      document.querySelectorAll('[data-menu][open], [data-mobile-nav][open]').forEach((details) => { details.open = false; });
      returnTarget = trigger;
      input.value = '';
      filter();
      palette.showModal();
      input.focus();
    };
    const run = (item, newTab) => {
      if (!item) return;
      const {href, action} = item.dataset;
      if (action === 'theme') { toggleTheme(); palette.close(); return; }
      if (action === 'logout') {
        returnTarget = null;
        palette.close();
        openDialog(document.getElementById('logout-confirmation'), document.querySelector('.account-menu summary'));
        return;
      }
      if (!href) return;
      if (newTab) { window.open(href, '_blank', 'noopener'); return; }
      returnTarget = null;
      palette.close();
      window.location.assign(href);
    };
    palette.addEventListener('close', () => {
      if (returnTarget?.isConnected) returnTarget.focus({preventScroll: true});
      returnTarget = null;
    });
    input.addEventListener('input', filter);
    input.addEventListener('keydown', (event) => {
      const list = visible();
      const index = list.indexOf(active);
      if (event.key === 'ArrowDown') { event.preventDefault(); select(list[(index + 1) % list.length] || null); }
      else if (event.key === 'ArrowUp') { event.preventDefault(); select(list[(index - 1 + list.length) % list.length] || null); }
      else if (event.key === 'Home' && list.length) { event.preventDefault(); select(list[0]); }
      else if (event.key === 'End' && list.length) { event.preventDefault(); select(list[list.length - 1]); }
      else if (event.key === 'Enter') { event.preventDefault(); run(active, event.metaKey || event.ctrlKey); }
      else if (event.key === 'Tab') event.preventDefault();
    });
    palette.addEventListener('pointermove', (event) => {
      const item = event.target.closest('[data-command-item]');
      if (item && item !== active) select(item);
    });
    palette.addEventListener('click', (event) => {
      const item = event.target.closest('[data-command-item]');
      if (item) { run(item, event.metaKey || event.ctrlKey); return; }
      if (event.target !== palette) return;
      const rect = palette.getBoundingClientRect();
      if (event.clientX < rect.left || event.clientX > rect.right ||
          event.clientY < rect.top || event.clientY > rect.bottom) palette.close();
    });
    document.addEventListener('click', (event) => {
      const trigger = event.target.closest('[data-command-open]');
      if (trigger) { event.preventDefault(); open(trigger); }
    });
    document.addEventListener('keydown', (event) => {
      if (!(event.metaKey || event.ctrlKey) || event.altKey || event.key.toLowerCase() !== 'k') return;
      event.preventDefault();
      if (palette.open) palette.close(); else open(document.activeElement);
    });
  }

  function initRepositorySearch() {
    if (!document.querySelector('#repository-filters, #repository-detail')) return;
    const requests = new Map();
    const targets = new Set(['repository-results', 'repository-detail', 'pull-request-results']);
    const queryKeys = ['q', 'access', 'page', 'pr_q', 'pr_state', 'pr_page'];
    const feedbackFor = target => target.id === 'repository-detail'
      ? document.querySelector('dialog[open] [data-policy-feedback]') || document.querySelector('[data-detail-feedback]')
      : target.parentElement.querySelector('[data-repository-feedback]');
    document.addEventListener('htmx:configRequest', event => {
      const {target, elt, verb, parameters} = event.detail;
      if (!targets.has(target?.id)) return;
      const requestURL = new URL(event.detail.path, window.location.href);
      if (elt.hasAttribute('data-clear-directory')) {
        const form = document.getElementById('repository-filters');
        form.elements.q.value = '';
        form.querySelector('input[value="all"]').checked = true;
        parameters.q = ''; parameters.access = 'all'; parameters.page = '1';
      }
      if (verb === 'get') {
        // A poll follows current filters, rather than the heading's older snapshot.
        if (elt.getAttribute('hx-trigger')?.includes('every ')) {
          const current = new URLSearchParams(window.location.search);
          queryKeys.forEach(key => {
            requestURL.searchParams.delete(key);
            if (current.has(key)) parameters[key] = current.get(key); else delete parameters[key];
          });
        }
        // Form parameters replace URL values; duplicate keys would keep stale filters.
        queryKeys.forEach(key => {
          if (Object.hasOwn(parameters, key)) requestURL.searchParams.delete(key);
        });
        event.detail.path = requestURL.pathname + requestURL.search;
      } else if (target.id === 'repository-detail') {
        const query = new URLSearchParams();
        const current = new URLSearchParams(window.location.search);
        queryKeys.forEach(key => {if (current.has(key)) query.set(key, current.get(key));});
        const pr = window.location.pathname.match(/\/pull-requests\/(\d+)$/);
        if (pr) query.set('pr', pr[1]);
        parameters.workspace_query = query.toString();
      }
    });
    document.addEventListener('htmx:beforeRequest', event => {
      const {target, xhr, elt} = event.detail;
      if (!targets.has(target?.id)) return;
      const polling = elt.getAttribute('hx-trigger')?.includes('every ');
      if (polling && (document.querySelector('#build-policy[open]') || document.activeElement?.closest('#pull-request-filters'))) {
        event.preventDefault(); return;
      }
      requests.set(target.id, {xhr, userAction: !polling});
      target.setAttribute('aria-busy', 'true');
      const feedback = feedbackFor(target);
      if (feedback) {delete feedback.dataset.error; feedback.textContent = 'Updating…';}
    });
    document.addEventListener('htmx:afterRequest', event => {
      const {target, xhr, successful} = event.detail;
      if (!targets.has(target?.id) || requests.get(target.id)?.xhr !== xhr) return;
      if (!successful) target.removeAttribute('aria-busy');
      const feedback = feedbackFor(target);
      if (!feedback) return;
      if (successful) feedback.textContent = '';
      else {feedback.dataset.error = 'true'; feedback.textContent = 'Could not update this view. Check your connection or sign in again, then retry.';}
    });
    document.addEventListener('htmx:afterSettle', event => {
      const {target} = event.detail;
      if (!targets.has(target?.id)) return;
      target.removeAttribute('aria-busy');
      if (target.id === 'repository-detail' && requests.get(target.id)?.userAction) {
        target.querySelector('[data-detail-heading]')?.focus({preventScroll:true});
      } else if (requests.get(target.id)?.userAction) {
        // Bring the start of new results back under the sticky filters.
        const scroller = document.querySelector('.app-content');
        const toolbar = target.parentElement.querySelector('.repository-toolbar');
        if (!scroller) return;
        const offset = (toolbar?.offsetHeight || 0) + 8;
        const top = target.getBoundingClientRect().top - scroller.getBoundingClientRect().top;
        if (top < offset) scroller.scrollBy({top: top - offset, behavior: 'smooth'});
      }
    });
    document.addEventListener('htmx:historyRestore', () => {
      targets.forEach(id => {
        const target = document.getElementById(id);
        if (!target) return;
        requests.get(id)?.xhr.abort(); target.removeAttribute('aria-busy');
        const feedback = feedbackFor(target);
        if (feedback) {feedback.textContent = ''; delete feedback.dataset.error;}
      });
    });
  }

  initMotion();
  initTheme();
  initNavigation();
  initPrefetch();
  initConfirmationDialogs();
  initAccountMenus();
  initQueueForms();
  initCommandPalette();
  initRepositorySearch();
})();
