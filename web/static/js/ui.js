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

  }

  function initConfirmationDialogs() {
    const boundDialogs = new WeakSet();
    const dialogReturnTargets = new WeakMap();
    const openDialog = (dialog, trigger) => {
      if (!(dialog instanceof HTMLDialogElement) || dialog.open) return;
      const menu = trigger?.closest('[data-menu]');
      dialogReturnTargets.set(dialog, menu ? menu.querySelector('summary') : trigger);
      if (menu) menu.open = false;
      dialog.showModal();
    };
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
      } else target.querySelector('.repository-table-scroll')?.scrollTo({top:0,behavior:'instant'});
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
  initNavigation();
  initConfirmationDialogs();
  initAccountMenus();
  initQueueForms();
  initRepositorySearch();
})();
