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
    // The page's Content-Security-Policy admits inline rules by its nonce.
    const nonce = document.querySelector('script[data-theme-boot]')?.nonce;
    if (nonce) rules.nonce = nonce;
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

  // Live updates: the server streams change signals (no data) for the page's
  // repository; each signal refetches through the ordinary GET handler. A
  // refresh that arrives while the user is filtering or confirming is deferred.
  let livePending = false;
  const liveRefresh = () => {
    livePending = false;
    if (document.querySelector('[data-live-refresh]')) window.htmx?.trigger(document.body, 'live-update');
  };
  // Live refreshes wait while a confirmation dialog is open (policy, teardown)
  // or the PR search has focus, so nothing moves under the owner's hand.
  const liveBlocked = () => Boolean(document.querySelector('dialog[data-confirmation-dialog][open]') ||
    document.activeElement?.closest('#pull-request-filters'));

  // A state that changes with time alone (a sync turning stale) carries
  // data-refresh-at: refresh the panel once that moment passes, since no
  // database change will announce it.
  function initRefreshDeadlines() {
    const timers = new Set();
    const schedule = () => {
      timers.forEach(clearTimeout);
      timers.clear();
      document.querySelectorAll('[data-refresh-at]').forEach((element) => {
        const delay = Date.parse(element.dataset.refreshAt) - Date.now() + 1000;
        if (!(delay > 0 && delay < 30 * 60 * 1000)) return;
        timers.add(setTimeout(() => { if (liveBlocked()) livePending = true; else liveRefresh(); }, delay));
      });
    };
    schedule();
    document.addEventListener('htmx:afterSettle', schedule);
  }

  function initLiveUpdates() {
    const config = document.querySelector('[data-live-events]');
    if (!config || !('EventSource' in window)) return;
    const base = new URL(config.dataset.liveEvents, window.location.href);
    let source = null;
    let lastID = '';
    let failures = 0;
    let retry = 0;
    const setState = (state) => { document.documentElement.dataset.liveState = state; };
    const request = () => { if (liveBlocked()) livePending = true; else liveRefresh(); };
    const connect = () => {
      clearTimeout(retry);
      const url = new URL(base);
      if (lastID) url.searchParams.set('since', lastID);
      source = new EventSource(url);
      const remember = (event) => { if (event.lastEventId) lastID = event.lastEventId; };
      source.addEventListener('ready', (event) => { remember(event); failures = 0; setState('live'); });
      for (const name of ['change', 'resync']) {
        source.addEventListener(name, (event) => { remember(event); failures = 0; setState('live'); request(); });
      }
      source.addEventListener('error', () => {
        if (source?.readyState !== EventSource.CLOSED) { setState('reconnecting'); return; } // Browser retries with Last-Event-ID.
        source = null;
        failures++;
        if (failures > 5) { setState('offline'); return; }
        setState('reconnecting');
        retry = setTimeout(connect, Math.min(30000, 1000 * 2 ** failures));
      });
    };
    const disconnect = () => { clearTimeout(retry); source?.close(); source = null; };
    // Hidden tabs release their connection; returning resumes from the last id.
    document.addEventListener('visibilitychange', () => {
      if (document.hidden) { disconnect(); setState('paused'); } else if (!source) { failures = 0; connect(); }
    });
    window.addEventListener('pagehide', disconnect);
    window.addEventListener('pageshow', (event) => { if (event.persisted && !source && !document.hidden) connect(); });
    // Deferred refreshes run once the user finishes filtering or confirming.
    document.addEventListener('close', (event) => {
      if (event.target.id === 'build-policy' && livePending) setTimeout(() => { if (livePending && !liveBlocked()) liveRefresh(); });
    }, true);
    document.addEventListener('focusout', (event) => {
      const form = event.target.closest?.('#pull-request-filters');
      if (!form || form.contains(event.relatedTarget) || !livePending) return;
      setTimeout(() => { if (livePending && !liveBlocked()) liveRefresh(); });
    });
    if (!document.hidden) connect(); else setState('paused');
  }

  // Build logs: the server streams the current attempt's lines with ids
  // "<epoch>.<next>". A "reset" means a new attempt replaced the logs. Lines are
  // inserted as text, never markup. Hidden tabs release the connection.
  function initBuildLogs() {
    const panel = document.querySelector('[data-log-events]');
    const list = panel?.querySelector('[data-log-lines]');
    if (!panel || !list || !('EventSource' in window)) return;
    const base = new URL(panel.dataset.logEvents, window.location.href);
    const empty = panel.querySelector('[data-log-empty]');
    const follow = panel.querySelector('[data-log-follow]');
    const connection = document.querySelector('[data-log-connection]');
    const status = document.querySelector('.log-status');
    let cursor = panel.dataset.logCursor || '';
    let source = null;
    let failures = 0;
    let retry = 0;
    panel.querySelector('[data-log-follow-control]')?.removeAttribute('hidden');
    const nearBottom = () => list.scrollHeight - list.scrollTop - list.clientHeight < 24;
    const stick = () => { if (follow?.checked) list.scrollTop = list.scrollHeight; };
    list.addEventListener('scroll', () => { if (follow) follow.checked = nearBottom(); }, { passive: true });
    follow?.addEventListener('change', stick);
    const setState = (state, text) => {
      if (!connection) return;
      connection.dataset.state = state;
      connection.textContent = text;
    };
    const append = (lines) => {
      const fragment = document.createDocumentFragment();
      for (const line of lines) {
        const number = String(line.n + 1);
        const item = document.createElement('li');
        item.id = 'L' + number;
        item.className = line.system ? 'log-line log-system' : 'log-line';
        if (!line.system && /\b(error|failed|fatal|panic)\b/i.test(line.text)) item.classList.add('log-error');
        const anchor = document.createElement('a');
        anchor.className = 'log-number';
        anchor.href = '#L' + number;
        anchor.tabIndex = -1;
        anchor.textContent = number;
        const text = document.createElement('span');
        text.className = 'log-text';
        text.textContent = line.text;
        item.append(anchor, text);
        fragment.append(item);
      }
      const wasAtBottom = nearBottom();
      list.append(fragment);
      if (empty) empty.hidden = list.childElementCount > 0;
      if (follow?.checked || wasAtBottom) stick();
    };
    const showState = (state) => {
      const badge = status?.querySelector('[data-log-badge]');
      if (badge) {
        badge.className = 'badge ' + state.tone;
        badge.querySelector('[data-log-label]').textContent = state.label;
      }
      const summary = status?.querySelector('[data-log-summary]');
      if (summary && summary.textContent !== state.summary) summary.textContent = state.summary;
      let sha = status?.querySelector('[data-log-sha]');
      if (state.sha && !sha && summary) {
        sha = document.createElement('code');
        sha.className = 'sha';
        sha.dataset.logSha = '';
        summary.after(sha);
      }
      if (sha) { sha.textContent = state.sha.slice(0, 12); sha.title = state.sha; sha.hidden = !state.sha; }
      const notices = status?.querySelector('[data-log-notices]');
      const template = status?.querySelector('[data-log-notice-template]');
      if (notices && template) {
        notices.replaceChildren(...state.notices.map((notice) => {
          const item = template.content.firstElementChild.cloneNode(true);
          item.classList.add(notice.tone);
          item.querySelector('span').textContent = notice.text;
          return item;
        }));
      }
      if (empty && state.empty) empty.textContent = state.empty;
    };
    const remember = (event) => { if (event.lastEventId) cursor = event.lastEventId; };
    const connect = () => {
      clearTimeout(retry);
      const url = new URL(base);
      if (cursor) url.searchParams.set('since', cursor);
      source = new EventSource(url);
      source.addEventListener('reset', (event) => {
        remember(event);
        list.replaceChildren();
        if (empty) empty.hidden = false;
      });
      source.addEventListener('lines', (event) => {
        remember(event);
        failures = 0;
        setState('live', 'Live');
        append(JSON.parse(event.data).lines);
      });
      source.addEventListener('state', (event) => {
        remember(event);
        failures = 0;
        setState('live', 'Live');
        showState(JSON.parse(event.data));
      });
      source.addEventListener('error', () => {
        if (source?.readyState !== EventSource.CLOSED) { setState('reconnecting', 'Reconnecting…'); return; }
        source = null;
        failures++;
        if (failures > 5) { setState('offline', 'Live updates unavailable — reload to retry'); return; }
        setState('reconnecting', 'Reconnecting…');
        retry = setTimeout(connect, Math.min(30000, 1000 * 2 ** failures));
      });
    };
    const disconnect = () => { clearTimeout(retry); source?.close(); source = null; };
    document.addEventListener('visibilitychange', () => {
      if (document.hidden) { disconnect(); setState('paused', 'Paused while hidden'); } else if (!source) { failures = 0; connect(); }
    });
    window.addEventListener('pagehide', disconnect);
    window.addEventListener('pageshow', (event) => { if (event.persisted && !source && !document.hidden) connect(); });
    if (!window.location.hash) stick();
    if (!document.hidden) connect(); else setState('paused', 'Paused while hidden');
  }

  // Times are rendered as exact UTC; show them relative ("3 minutes ago") and
  // keep the exact time as the title. Refreshed every 30 s and after swaps.
  function initRelativeTimes() {
    if (!('RelativeTimeFormat' in Intl)) return;
    const format = new Intl.RelativeTimeFormat('en', {numeric: 'auto'});
    const units = [['year', 31536e6], ['month', 2592e6], ['week', 6048e5], ['day', 864e5], ['hour', 36e5], ['minute', 6e4]];
    const render = () => document.querySelectorAll('time[data-relative]').forEach((element) => {
      const when = Date.parse(element.dateTime);
      if (Number.isNaN(when)) return;
      const diff = when - Date.now();
      const [unit, size] = units.find(([, length]) => Math.abs(diff) >= length) || ['second', 1000];
      const text = unit === 'second' ? (diff < 0 ? 'just now' : 'in a moment') : format.format(Math.round(diff / size), unit);
      if (element.textContent !== text) element.textContent = text;
    });
    render();
    setInterval(render, 30000);
    document.addEventListener('htmx:afterSettle', render);
  }

  // Copy buttons: data-copy holds the text, or data-copy-lines names a log
  // list whose line texts are copied. The label confirms for two seconds.
  function initCopyButtons() {
    const timers = new WeakMap();
    document.addEventListener('click', async (event) => {
      const button = event.target.closest('[data-copy], [data-copy-lines]');
      if (!button) return;
      const status = button.querySelector('[data-copy-status]');
      const label = status?.textContent;
      let text = button.dataset.copy ?? '';
      if (button.dataset.copyLines) {
        const list = document.querySelector(button.dataset.copyLines);
        text = [...(list?.querySelectorAll('.log-text') || [])].map(line => line.textContent).join('\n');
      }
      try {
        await navigator.clipboard.writeText(text);
        button.dataset.copied = '';
        if (status) status.textContent = 'Copied';
      } catch {
        if (status) status.textContent = 'Copy failed';
      }
      clearTimeout(timers.get(button));
      timers.set(button, setTimeout(() => {
        delete button.dataset.copied;
        if (status && label) status.textContent = label;
      }, 2000));
    });
  }

  // .env text: the same rules as the server's secrets.ParseDotenv.
  function parseDotenv(text) {
    const lines = text.replace(/\r\n/g, '\n').split('\n');
    const entries = [];
    const closing = (body) => {
      for (let i = 0; i < body.length; i++) {
        if (body[i] === '\\') i++;
        else if (body[i] === '"') return i;
      }
      return -1;
    };
    const unescape = body => body.replace(/\\(.)/g, (_, c) => (c === 'n' ? '\n' : c === 't' ? '\t' : c));
    for (let i = 0; i < lines.length; i++) {
      const number = i + 1;
      let line = lines[i].trim();
      if (!line || line.startsWith('#')) continue;
      if (line.startsWith('export ')) line = line.slice(7);
      const equals = line.indexOf('=');
      const name = equals < 0 ? '' : line.slice(0, equals).trim();
      if (!name) throw new Error(`Line ${number}: expected NAME=value.`);
      let raw = line.slice(equals + 1).trim();
      let value;
      if (raw.startsWith('"')) {
        let body = raw.slice(1);
        for (;;) {
          const end = closing(body);
          if (end >= 0) { value = unescape(body.slice(0, end)); break; }
          i++;
          if (i >= lines.length) throw new Error(`Line ${number}: the quoted value of ${name} is not closed.`);
          body += '\n' + lines[i].replace(/[ \t]+$/, '');
        }
      } else if (raw.startsWith("'")) {
        const end = raw.lastIndexOf("'");
        if (end === 0) throw new Error(`Line ${number}: the quoted value of ${name} is not closed.`);
        value = raw.slice(1, end);
      } else {
        const comment = raw.indexOf(' #');
        if (comment >= 0) raw = raw.slice(0, comment).trim();
        value = raw;
      }
      entries.push([name, value]);
    }
    return entries;
  }

  // Secrets editor: lines of name and value like a key-value table. Pasting
  // several NAME=value lines into a name splits them into lines; a .env file
  // or pasted text can be moved into lines to review before saving. Checks
  // run before submit; the server checks everything again.
  function initSecretEditor() {
    const form = document.querySelector('[data-secret-editor]');
    if (!form) return;
    const list = form.querySelector('[data-secret-rows]');
    const template = form.querySelector('[data-secret-row-template]');
    const existing = new Set((form.dataset.existingNames || '').split(' ').filter(Boolean));
    const remaining = Number(form.dataset.remaining || 0);
    const dotenv = form.querySelector('[data-dotenv-text]');
    const dotenvFeedback = form.querySelector('[data-dotenv-feedback]');
    const feedback = form.querySelector('[data-secret-editor-feedback]');
    const submitLabel = form.querySelector('[data-secret-submit-label]');
    const namePattern = /^[A-Za-z_][A-Za-z0-9_]{0,127}$/;
    const rows = () => [...list.querySelectorAll('[data-secret-row]')];
    const fields = row => [row.querySelector('[name="name"]'), row.querySelector('[name="value"]')];
    // setNote shows a hint, or an error that marks invalidField.
    const setNote = (row, text, invalidField = null) => {
      const note = row.querySelector('[data-row-note]');
      row.classList.toggle('has-error', Boolean(invalidField));
      fields(row).forEach((field) => {
        if (field === invalidField) field.setAttribute('aria-invalid', 'true'); else field.removeAttribute('aria-invalid');
      });
      note.replaceChildren();
      if (!text) return;
      if (invalidField) {
        const span = document.createElement('span');
        span.className = 'field-error';
        span.textContent = text;
        note.append(span);
      } else note.textContent = text;
    };
    const hint = (row) => {
      const name = fields(row)[0].value.trim();
      if (!row.classList.contains('has-error')) setNote(row, existing.has(name) ? 'Replaces the stored value.' : '');
    };
    const grow = (area) => { area.rows = Math.min(8, Math.max(1, area.value.split('\n').length)); };
    const relabel = () => rows().forEach((row, index) => {
      const [name, value] = fields(row);
      name.setAttribute('aria-label', `Name, line ${index + 1}`);
      value.setAttribute('aria-label', `Value, line ${index + 1}`);
      row.querySelector('[data-remove-secret-row]').setAttribute('aria-label', `Remove line ${index + 1}`);
    });
    const count = () => {
      const filled = rows().filter(row => fields(row).some(field => field.value.trim() !== '')).length;
      let pasted = 0;
      try { pasted = dotenv?.value.trim() ? parseDotenv(dotenv.value).length : 0; } catch { /* Reported on submit. */ }
      const total = filled + pasted;
      if (submitLabel) submitLabel.textContent = total > 1 ? `Save ${total} secrets` : total === 1 ? 'Save 1 secret' : 'Save secrets';
    };
    const addRow = (name = '', value = '') => {
      const empty = rows().find(row => fields(row).every(field => field.value === ''));
      const row = empty || template.content.firstElementChild.cloneNode(true);
      if (!empty) list.append(row);
      const [nameField, valueField] = fields(row);
      nameField.value = name;
      valueField.value = value;
      grow(valueField);
      hint(row);
      relabel();
      return row;
    };
    const fill = (entries) => {
      entries.forEach(([name, value]) => addRow(name, value));
      count();
    };
    list.addEventListener('input', (event) => {
      const row = event.target.closest('[data-secret-row]');
      if (!row) return;
      if (event.target.name === 'value') grow(event.target);
      if (row.classList.contains('has-error')) setNote(row, '');
      hint(row);
      count();
    });
    list.addEventListener('paste', (event) => {
      if (event.target.name !== 'name') return;
      const text = event.clipboardData?.getData('text') || '';
      if (!text.includes('=')) return;
      try {
        const entries = parseDotenv(text);
        if (!entries.length) return;
        event.preventDefault();
        const row = event.target.closest('[data-secret-row]');
        fields(row).forEach(field => { field.value = ''; });
        fill(entries);
        if (feedback) feedback.textContent = `Added ${entries.length} line${entries.length === 1 ? '' : 's'} from the pasted text.`;
      } catch { /* Not .env text: paste it as typed. */ }
    });
    list.addEventListener('click', (event) => {
      const remove = event.target.closest('[data-remove-secret-row]');
      if (!remove) return;
      const row = remove.closest('[data-secret-row]');
      if (rows().length > 1) {
        const next = row.nextElementSibling || row.previousElementSibling;
        row.remove();
        fields(next)[0]?.focus();
      } else {
        fields(row).forEach(field => { field.value = ''; });
        setNote(row, '');
        fields(row)[0].focus();
      }
      relabel();
      count();
    });
    form.querySelector('[data-add-secret-row]')?.addEventListener('click', () => {
      const row = template.content.firstElementChild.cloneNode(true);
      list.append(row);
      relabel();
      fields(row)[0].focus();
    });
    dotenv?.addEventListener('input', count);
    form.querySelector('[data-dotenv-file]')?.addEventListener('change', async (event) => {
      const file = event.target.files?.[0];
      if (!file || !dotenv) return;
      if (file.size > 256 * 1024) { dotenvFeedback.textContent = 'That file is too large for secrets (256 KiB at most).'; return; }
      dotenv.value = await file.text();
      event.target.value = '';
      dotenvFeedback.textContent = `Loaded ${file.name}. Review it, then save or move it into lines.`;
      count();
    });
    form.querySelector('[data-dotenv-split]')?.addEventListener('click', () => {
      if (!dotenv?.value.trim()) { dotenvFeedback.textContent = 'Paste .env text or choose a file first.'; return; }
      try {
        const entries = parseDotenv(dotenv.value);
        fill(entries);
        dotenv.value = '';
        dotenv.removeAttribute('aria-invalid');
        dotenvFeedback.textContent = `Moved ${entries.length} line${entries.length === 1 ? '' : 's'} above.`;
        count();
      } catch (error) {
        dotenvFeedback.textContent = error.message;
      }
    });
    form.addEventListener('submit', (event) => {
      const seen = new Map();
      let firstInvalid = null;
      let fresh = 0;
      const fail = (row, message, field) => { setNote(row, message, field); firstInvalid ||= field; };
      rows().forEach((row) => {
        const [nameField, valueField] = fields(row);
        const name = nameField.value.trim();
        const value = valueField.value;
        if (!name && !value) { setNote(row, ''); return; }
        if (!name) return fail(row, 'Enter a name for this value.', nameField);
        if (!namePattern.test(name)) return fail(row, 'Use letters, digits and underscores, starting with a letter or underscore.', nameField);
        if (name.toUpperCase().startsWith('PREVIA_')) return fail(row, 'Names starting with PREVIA_ are reserved for the platform.', nameField);
        if (seen.has(name)) return fail(row, `${name} appears more than once. Keep one value per name.`, nameField);
        if (!value) return fail(row, `Enter a value for ${name}.`, valueField);
        seen.set(name, row);
        if (!existing.has(name)) fresh++;
        hint(row);
      });
      let pasted = [];
      try {
        pasted = dotenv?.value.trim() ? parseDotenv(dotenv.value) : [];
      } catch (error) {
        dotenvFeedback.textContent = error.message;
        firstInvalid ||= dotenv;
      }
      for (const [name] of pasted) {
        if (seen.has(name)) {
          dotenvFeedback.textContent = `${name} is both in the lines above and in the pasted text. Keep one.`;
          firstInvalid ||= dotenv;
        }
        seen.set(name, null);
        if (!existing.has(name)) fresh++;
      }
      if (!firstInvalid && seen.size === 0) {
        if (feedback) feedback.textContent = 'Add at least one name and value.';
        firstInvalid = fields(rows()[0])[0];
      } else if (!firstInvalid && fresh > remaining) {
        if (feedback) feedback.textContent = `Only ${remaining} more name${remaining === 1 ? '' : 's'} fit. Delete some secrets or save fewer new names.`;
        firstInvalid = fields(rows()[0])[0];
      }
      if (firstInvalid) {
        event.preventDefault();
        firstInvalid.closest('details')?.setAttribute('open', '');
        firstInvalid.focus();
      }
    });
    rows().forEach((row) => { hint(row); grow(fields(row)[1]); });
    count();
  }

  // Bulk delete: checkboxes belong to the confirmation's form; the trigger
  // shows how many are selected and stays disabled until one is.
  function initBulkDelete() {
    const trigger = document.querySelector('[data-bulk-delete]');
    if (!trigger) return;
    const boxes = [...document.querySelectorAll('[data-select-secret]')];
    const all = document.querySelector('[data-select-all]');
    const counter = trigger.querySelector('[data-bulk-delete-count]');
    const summary = document.querySelector('[data-bulk-delete-summary]');
    const update = () => {
      const chosen = boxes.filter(box => box.checked);
      trigger.disabled = chosen.length === 0;
      counter.textContent = chosen.length ? `(${chosen.length})` : '';
      if (all) {
        all.checked = chosen.length === boxes.length && boxes.length > 0;
        all.indeterminate = chosen.length > 0 && chosen.length < boxes.length;
      }
      if (summary) summary.textContent = chosen.length ? `${chosen.length === 1 ? '1 secret' : chosen.length + ' secrets'}: ${chosen.map(box => box.value).join(', ')}` : '';
    };
    boxes.forEach(box => box.addEventListener('change', update));
    all?.addEventListener('change', () => { boxes.forEach((box) => { box.checked = all.checked; }); update(); });
    update();
  }

  function initRepositorySearch() {
    if (!document.querySelector('#repository-filters, #repository-detail')) return;
    const requests = new Map();
    const targets = new Set(['repository-results', 'repository-detail', 'pull-request-results']);
    const queryKeys = ['q', 'access', 'page', 'pr_q', 'pr_state', 'pr_page'];
    const feedbackFor = target => target.id === 'repository-detail'
      ? document.querySelector('dialog[open] [data-policy-feedback], dialog[open] [data-dialog-feedback]') || document.querySelector('[data-detail-feedback]')
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
        // A live refresh follows current filters and path, not its render-time URL.
        if (elt.hasAttribute('data-live-refresh')) {
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
      const live = elt.hasAttribute('data-live-refresh');
      if (live && liveBlocked()) {
        livePending = true;
        event.preventDefault(); return;
      }
      requests.set(target.id, {xhr, userAction: !live});
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
      if (successful) {feedback.textContent = ''; return;}
      feedback.dataset.error = 'true';
      // Refused actions explain themselves in a short plain-text sentence
      // (for example "GitHub is still checking mergeability"); show it as text.
      const reason = (xhr.getResponseHeader('Content-Type') || '').startsWith('text/plain') ? xhr.responseText.trim() : '';
      const explained = reason !== '' && reason.length <= 300 && ((xhr.status >= 400 && xhr.status < 500) || xhr.status === 503);
      feedback.textContent = explained ? reason : 'Could not update this view. Check your connection or sign in again, then retry.';
    });
    document.addEventListener('htmx:afterSettle', event => {
      const {target} = event.detail;
      if (!targets.has(target?.id)) return;
      target.removeAttribute('aria-busy');
      if (target.id === 'repository-detail') livePending = false; // The panel is now current.
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

  // "How it works" (home page): Kai asks one question at a time and Previa
  // answers after a short typing pause (none with reduced motion). The stage
  // rail and "Show the whole conversation" jump ahead. Turns enter the log as
  // new nodes, so screen readers hear them; without JavaScript every turn is
  // already in the page and the stages are ordinary links.
  function initStory() {
    const motion = window.matchMedia('(prefers-reduced-motion: reduce)');
    document.querySelectorAll('[data-story]').forEach((story) => {
      const thread = story.querySelector('[data-story-thread]');
      const typing = story.querySelector('[data-story-typing]');
      const turns = [...thread.querySelectorAll('[data-story-turn]')];
      const outro = thread.querySelector('[data-story-outro]');
      const stages = [...story.querySelectorAll('[data-story-stage]')];
      const composer = story.querySelector('[data-story-composer]');
      const next = story.querySelector('[data-story-next]');
      const question = story.querySelector('[data-story-question]');
      const again = story.querySelector('[data-story-again]');
      const all = story.querySelector('[data-story-all]');
      const restart = story.querySelector('[data-story-restart]');
      const count = story.querySelector('[data-story-count]');
      const bar = story.querySelector('[data-story-bar]');
      const answerOf = turn => turn.querySelector('[data-story-answer]');
      let shown = 0;
      let timers = [];
      turns.forEach(turn => turn.remove());
      outro.remove();
      composer.hidden = false;
      thread.tabIndex = 0;

      const render = () => {
        const done = shown === turns.length;
        stages.forEach((stage, index) => {
          stage.classList.toggle('is-done', index < shown);
          if (index === shown - 1) stage.setAttribute('aria-current', 'step');
          else stage.removeAttribute('aria-current');
        });
        count.textContent = `${shown} / ${turns.length}`;
        bar.style.width = `${(shown / turns.length) * 100}%`;
        restart.hidden = shown === 0;
        all.hidden = done;
        next.hidden = done;
        again.hidden = !done;
        if (!done) question.textContent = turns[shown].querySelector('[data-story-ask] .chat-bubble').textContent.trim();
      };
      const scrollToTurn = (turn) => {
        thread.scrollTo({top: Math.max(0, turn.offsetTop - 16), behavior: motion.matches ? 'auto' : 'smooth'});
      };
      // Ends a pending answer: every placed turn shows its answer.
      const settle = () => {
        timers.forEach(clearTimeout);
        timers = [];
        typing.hidden = true;
        turns.slice(0, shown).forEach((turn) => { answerOf(turn).hidden = false; });
        if (shown === turns.length && !outro.isConnected) thread.insertBefore(outro, typing);
        next.removeAttribute('aria-disabled');
        thread.removeAttribute('aria-busy');
      };
      const place = (turn) => {
        turn.querySelectorAll('.chat-enter').forEach(message => message.classList.remove('chat-enter'));
        thread.insertBefore(turn, typing);
      };
      // Shows every turn through upTo at once, then focuses one of them.
      const reveal = (upTo, focusIndex) => {
        settle();
        thread.setAttribute('aria-live', 'off');
        while (shown <= upTo && shown < turns.length) place(turns[shown++]);
        settle();
        render();
        const target = turns[Math.min(focusIndex, shown - 1)];
        if (target) {
          scrollToTurn(target);
          target.focus({preventScroll: true});
        }
        requestAnimationFrame(() => thread.setAttribute('aria-live', 'polite'));
      };
      const ask = () => {
        if (next.getAttribute('aria-disabled') === 'true' || shown >= turns.length) return;
        const reduce = motion.matches;
        const turn = turns[shown++];
        const answer = answerOf(turn);
        place(turn);
        answer.hidden = true;
        if (!reduce) turn.querySelector('[data-story-ask]').classList.add('chat-enter');
        scrollToTurn(turn);
        const finish = () => {
          settle();
          if (!reduce) answer.classList.add('chat-enter');
          render();
          scrollToTurn(turn);
          if (shown === turns.length && (document.activeElement === next || document.activeElement === document.body)) again.focus({preventScroll: true});
        };
        if (reduce) { finish(); return; }
        next.setAttribute('aria-disabled', 'true');
        thread.setAttribute('aria-busy', 'true');
        // A longer answer takes a little longer to "type", within reason.
        const pause = Math.min(1500, 450 + answer.textContent.trim().length * 2.5);
        timers.push(setTimeout(() => {
          typing.hidden = false;
          typing.classList.add('chat-enter');
          thread.scrollTo({top: thread.scrollHeight, behavior: 'smooth'});
        }, 300));
        timers.push(setTimeout(finish, 300 + pause));
      };
      const startOver = () => {
        settle();
        turns.forEach(turn => turn.remove());
        outro.remove();
        shown = 0;
        render();
        thread.scrollTo({top: 0});
        next.focus({preventScroll: true});
      };

      next.addEventListener('click', ask);
      again.addEventListener('click', startOver);
      restart.addEventListener('click', startOver);
      all.addEventListener('click', () => {
        const from = shown;
        reveal(turns.length - 1, from);
      });
      stages.forEach((stage, index) => stage.addEventListener('click', (event) => {
        event.preventDefault();
        reveal(index, index);
        thread.closest('.story-window').scrollIntoView({block: 'nearest', behavior: motion.matches ? 'auto' : 'smooth'});
      }));
      render();
      // A shared #story-… link opens the conversation at that stage.
      const linked = turns.findIndex(turn => `#${turn.id}` === window.location.hash);
      if (linked >= 0) {
        reveal(linked, linked);
        story.scrollIntoView({block: 'start'});
      }
    });
  }

  // FAQ (home page and workspace): each question is a native <details>, so it
  // works without JavaScript. With it, topic buttons and search narrow the
  // list, Previa "types" briefly before an answer opens (not with reduced
  // motion), and a #faq-… link opens its answer.
  function initFAQ() {
    const motion = window.matchMedia('(prefers-reduced-motion: reduce)');
    document.querySelectorAll('[data-faq]').forEach((faq) => {
      const items = [...faq.querySelectorAll('[data-faq-item]')];
      const groups = [...faq.querySelectorAll('[data-faq-group]')];
      const filters = [...faq.querySelectorAll('[data-faq-filter]')];
      const search = faq.querySelector('[data-faq-search]');
      const empty = faq.querySelector('[data-faq-empty]');
      const status = faq.querySelector('[data-faq-status]');
      const words = new Map(items.map(item => [item, item.textContent.toLowerCase()]));
      let topic = 'all';
      faq.querySelector('[data-faq-tools]').hidden = false;

      const apply = () => {
        const terms = search.value.trim().toLowerCase().split(/\s+/).filter(Boolean);
        let visible = 0;
        items.forEach((item) => {
          item.hidden = !((topic === 'all' || item.dataset.topic === topic) && terms.every(term => words.get(item).includes(term)));
          if (!item.hidden) visible += 1;
        });
        groups.forEach((group) => { group.hidden = !group.querySelector('[data-faq-item]:not([hidden])'); });
        empty.hidden = visible > 0;
        status.textContent = terms.length || topic !== 'all' ? `${visible} of ${items.length} questions shown.` : '';
      };
      const choose = (value) => {
        topic = value;
        filters.forEach((button) => button.setAttribute('aria-pressed', String(button.dataset.faqFilter === value)));
        apply();
      };
      filters.forEach((button) => button.addEventListener('click', () => choose(button.dataset.faqFilter)));
      search.addEventListener('input', apply);
      search.addEventListener('keydown', (event) => {
        if (event.key === 'Escape' && search.value) { search.value = ''; apply(); }
      });
      faq.querySelector('[data-faq-reset]').addEventListener('click', () => {
        search.value = '';
        choose('all');
        search.focus();
      });
      items.forEach((item) => {
        let timer = 0;
        // The click comes before the details opens, so the answer never flashes.
        item.querySelector('summary').addEventListener('click', () => {
          if (!item.open && !motion.matches) item.classList.add('is-typing');
        });
        item.addEventListener('toggle', () => {
          clearTimeout(timer);
          if (!item.open) { item.classList.remove('is-typing'); return; }
          if (item.classList.contains('is-typing')) timer = setTimeout(() => item.classList.remove('is-typing'), 650);
        });
      });
      const openLinked = () => {
        const item = items.find(candidate => `#${candidate.id}` === window.location.hash);
        if (!item) return;
        search.value = '';
        choose('all');
        item.open = true;
        item.scrollIntoView({block: 'center'});
        item.querySelector('summary').focus({preventScroll: true});
      };
      window.addEventListener('hashchange', openLinked);
      openLinked();
    });
  }

  // Overview: the latest-activity lists become two tabs (the choice is kept for
  // this browser session), each live preview shows how much of its lifetime is
  // left, and the header says how fresh the numbers are. Without JavaScript
  // both lists show and their rows still expand.
  function initOverview() {
    const overview = document.querySelector('[data-overview]');
    if (!overview) return;
    const activity = overview.querySelector('[data-overview-activity]');
    const tablist = activity?.querySelector('[data-overview-tabs]');
    if (tablist) {
      const tabs = [...tablist.querySelectorAll('[role="tab"]')];
      const panels = tabs.map(tab => document.getElementById(tab.getAttribute('aria-controls')));
      const select = (index, focus) => {
        tabs.forEach((tab, i) => {
          tab.setAttribute('aria-selected', String(i === index));
          tab.tabIndex = i === index ? 0 : -1;
          panels[i].hidden = i !== index;
        });
        if (focus) tabs[index].focus();
        try { sessionStorage.setItem('previa-overview-tab', tabs[index].id); } catch { /* Storage may be unavailable. */ }
      };
      panels.forEach((panel, i) => {
        panel.setAttribute('role', 'tabpanel');
        panel.setAttribute('aria-labelledby', tabs[i].id);
        panel.tabIndex = 0;
      });
      tabs.forEach((tab, index) => {
        tab.addEventListener('click', () => select(index, false));
        tab.addEventListener('keydown', (event) => {
          const moves = {ArrowRight: index + 1, ArrowLeft: index - 1, Home: 0, End: tabs.length - 1};
          if (!(event.key in moves)) return;
          event.preventDefault();
          select((moves[event.key] + tabs.length) % tabs.length, true);
        });
      });
      let saved = null;
      try { saved = sessionStorage.getItem('previa-overview-tab'); } catch { /* Storage may be unavailable. */ }
      tablist.hidden = false;
      activity.classList.add('has-tabs');
      select(Math.max(0, tabs.findIndex(tab => tab.id === saved)), false);
    }
    const lifetimes = [...overview.querySelectorAll('[data-lifetime]')];
    const updated = overview.querySelector('[data-overview-updated]');
    const loaded = Date.now();
    const refresh = () => {
      const now = Date.now();
      lifetimes.forEach((life) => {
        const from = Date.parse(life.dataset.from);
        const to = Date.parse(life.dataset.to);
        if (!(to > from)) return;
        const left = Math.min(1, Math.max(0, (to - now) / (to - from)));
        life.querySelector('[data-lifetime-fill]').style.width = `${left * 100}%`;
        life.querySelector('[data-lifetime-text]').textContent = left > 0 ? `${Math.max(1, Math.round(left * 100))}% of its lifetime left` : 'Expired';
        life.classList.toggle('is-low', left < 0.15);
        life.hidden = false;
      });
      const minutes = Math.floor((now - loaded) / 60000);
      updated.textContent = minutes < 1 ? 'Updated just now' : minutes < 60 ? `Updated ${minutes} min ago` : 'Updated over an hour ago';
      updated.hidden = false;
    };
    refresh();
    setInterval(refresh, 30000);
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
  initLiveUpdates();
  initBuildLogs();
  initRelativeTimes();
  initCopyButtons();
  initSecretEditor();
  initBulkDelete();
  initRefreshDeadlines();
  initStory();
  initFAQ();
  initOverview();
})();
