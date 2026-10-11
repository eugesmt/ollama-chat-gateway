(() => {
  'use strict';

  const API_ROOT = '/api/v1';
  const POLL_INTERVAL_MS = 5000;
  const MAX_INPUT_CHARS = 8000;
  const DEMO_LONG = 'Составь подробное, структурированное объяснение того, как работает Docker: образы, контейнеры, слои, сеть, тома и типичный жизненный цикл приложения. Добавь практический пример и список распространённых ошибок.';
  const DEMO_SHORT = 'В двух предложениях объясни, что такое HTTP-код 404.';

  const panels = new Map();
  const panelNodes = new Map();
  let queueTimer = null;

  function $(selector, root = document) {
    return root.querySelector(selector);
  }

  function $all(selector, root = document) {
    return Array.from(root.querySelectorAll(selector));
  }

  function createPanel(node) {
    return {
      id: node.dataset.client,
      node,
      history: [],
      active: null,
      source: null,
      busy: false,
      metrics: { wait: null, ttft: null, total: null },
    };
  }

  function apiKey() {
    return ($('#api-key')?.value || '').trim();
  }

  function authHeaders(json = false) {
    const headers = {};
    if (json) headers['Content-Type'] = 'application/json';
    const key = apiKey();
    if (key) headers.Authorization = `Bearer ${key}`;
    return headers;
  }

  function parsePayload(raw) {
    if (!raw) return {};
    try {
      return JSON.parse(raw);
    } catch {
      return { message: raw };
    }
  }

  async function responseError(response) {
    const payload = parsePayload(await response.text());
    const message = payload.error || payload.message || payload.detail;
    if (response.status === 401 || response.status === 403) return 'API key не принят backend-сервисом.';
    if (response.status === 413) return 'Запрос слишком большой: превышен лимит контекста.';
    if (response.status === 429) return 'Слишком много запросов. Попробуйте чуть позже.';
    if (response.status >= 500) return 'Backend или Ollama временно недоступны.';
    return payload.message ? String(payload.message) : (message ? String(message) : `Backend вернул HTTP ${response.status}.`);
  }

  async function postJob(messages, clientId) {
    const response = await fetch(`${API_ROOT}/jobs`, {
      method: 'POST',
      headers: authHeaders(true),
      body: JSON.stringify({ messages, client_id: clientId }),
    });
    if (!response.ok) throw new Error(await responseError(response));
    return response.json();
  }

  function setGlobalNotice(message, isError = false) {
    const notice = $('[data-global-notice]');
    notice.textContent = message;
    notice.classList.toggle('notice-error', isError);
    notice.hidden = !message;
  }

  function setPanelError(panel, message = '') {
    const error = $('[data-role="error"]', panel.node);
    error.textContent = message;
    error.hidden = !message;
  }

  function setStatus(panel, status, details = {}) {
    const chip = $('[data-role="status"]', panel.node);
    const normalized = String(status || 'idle').toLowerCase();
    const labels = {
      idle: 'Готов',
      queued: 'В очереди',
      running: 'Выполняется',
      completed: 'Готово',
      error: 'Ошибка',
      connecting: 'Подключение…',
      reconnecting: 'Переподключение…',
    };
    const visualStatus = normalized === 'connecting' || normalized === 'reconnecting' ? 'running' : normalized;
    chip.className = `status-chip status-${visualStatus}`;
    chip.textContent = labels[normalized] || normalized;

    const rawPosition = details.position ?? details.queue_position;
    if (rawPosition !== undefined) {
      const position = Number(rawPosition);
      $('[data-role="position"]', panel.node).textContent = Number.isFinite(position) && position > 0 ? String(position) : '—';
    }
    updateMetrics(panel, details);
  }

  function updateMetrics(panel, details = {}) {
    const timings = details.timings && typeof details.timings === 'object' ? details.timings : {};
    const source = { ...timings, ...details };
    const aliases = {
      wait: ['wait_ms', 'wait', 'queue_wait_ms', 'queue_wait'],
      ttft: ['ttft_ms', 'ttft', 'time_to_first_token_ms', 'time_to_first_token'],
      total: ['total_ms', 'total', 'duration_ms', 'duration'],
    };
    for (const [name, keys] of Object.entries(aliases)) {
      const value = firstNumber(source, keys);
      if (value !== null) panel.metrics[name] = normalizeMilliseconds(value, keys.some((key) => key.endsWith('_ms')));
      const node = $(`[data-role="${name}"]`, panel.node);
      node.textContent = formatDuration(panel.metrics[name]);
    }
  }

  function firstNumber(source, keys) {
    for (const key of keys) {
      if (source[key] === undefined || source[key] === null || source[key] === '') continue;
      const value = Number(source[key]);
      if (Number.isFinite(value)) return value;
    }
    return null;
  }

  function normalizeMilliseconds(value, explicitMilliseconds) {
    if (explicitMilliseconds) return value;
    // API timings without a unit are treated as milliseconds, which is the
    // most useful representation for the UI and matches the backend contract.
    return value;
  }

  function formatDuration(value) {
    if (value === null || value === undefined || !Number.isFinite(Number(value))) return '—';
    const ms = Number(value);
    if (ms < 1000) return `${Math.max(0, Math.round(ms))} ms`;
    return `${(ms / 1000).toFixed(ms < 10000 ? 1 : 0)} с`;
  }

  function renderMessages(panel) {
    const list = $('[data-role="messages"]', panel.node);
    list.textContent = '';

    if (!panel.history.length && !panel.active?.text) {
      const empty = document.createElement('div');
      empty.className = 'empty-chat';
      const icon = document.createElement('span');
      icon.className = 'empty-icon';
      icon.setAttribute('aria-hidden', 'true');
      icon.textContent = '✦';
      const title = document.createElement('p');
      title.textContent = 'Начните разговор';
      const hint = document.createElement('span');
      hint.textContent = 'Ответ модели появится здесь потоково';
      empty.append(icon, title, hint);
      list.append(empty);
      return;
    }

    for (const message of panel.history) appendMessage(list, message.role, message.content);
    if (panel.active?.text) appendMessage(list, 'assistant', panel.active.text, true);
    list.scrollTop = list.scrollHeight;
  }

  function appendMessage(list, role, content, streaming = false) {
    const row = document.createElement('div');
    row.className = `message-row ${role === 'user' ? 'user' : 'assistant'}${streaming ? ' streaming' : ''}`;
    const author = document.createElement('span');
    author.className = 'message-author';
    author.textContent = role === 'user' ? 'Вы' : 'Модель';
    const bubble = document.createElement('div');
    bubble.className = 'message-bubble';
    bubble.textContent = content || (streaming ? '…' : '');
    row.append(author, bubble);
    list.append(row);
  }

  function updateCounter(panel) {
    const input = $('[data-role="input"]', panel.node);
    const counter = $('[data-role="counter"]', panel.node);
    counter.textContent = `${input.value.length.toLocaleString('ru-RU')} / ${MAX_INPUT_CHARS.toLocaleString('ru-RU')}`;
    counter.style.color = input.value.length > MAX_INPUT_CHARS * .9 ? 'var(--amber)' : '';
  }

  function setBusy(panel, busy) {
    panel.busy = busy;
    $('[data-role="input"]', panel.node).disabled = busy;
    $('[data-role="composer"] button[type="submit"]', panel.node).disabled = busy;
  }

  function closeStream(panel) {
    if (panel.source) {
      panel.source.close();
      panel.source = null;
    }
  }

  function eventsURL(rawURL) {
    const url = new URL(rawURL || '', window.location.href);
    const key = apiKey();
    if (key) url.searchParams.set('api_key', key);
    return url.toString();
  }

  function openEvents(panel, rawURL) {
    closeStream(panel);
    let source;
    try {
      source = new EventSource(eventsURL(rawURL));
    } catch {
      finishWithError(panel, 'Не удалось открыть SSE-поток событий.');
      return;
    }
    panel.source = source;
    const active = panel.active;

    source.addEventListener('status', (event) => {
      if (!panel.active || panel.active !== active) return;
      const payload = parsePayload(event.data);
      setStatus(panel, payload.status || 'queued', payload);
    });

    source.addEventListener('token', (event) => {
      if (!panel.active || panel.active !== active) return;
      const payload = parsePayload(event.data);
      const text = payload.text ?? payload.content ?? '';
      if (typeof text !== 'string') return;
      if (!active.firstTokenAt) {
        active.firstTokenAt = performance.now();
        if (panel.metrics.ttft === null) {
          panel.metrics.ttft = active.firstTokenAt - active.startedAt;
          $('[data-role="ttft"]', panel.node).textContent = formatDuration(panel.metrics.ttft);
        }
      }
      active.text += text;
      renderMessages(panel);
    });

    source.addEventListener('done', (event) => {
      if (!panel.active || panel.active !== active) return;
      const payload = parsePayload(event.data);
      const finalText = payload.text ?? payload.content ?? payload.response;
      if (typeof finalText === 'string' && finalText.length >= active.text.length) active.text = finalText;
      updateMetrics(panel, payload);
      if (payload.status === 'completed' || !payload.status) {
        finishSuccess(panel);
      } else {
        finishWithError(panel, payload.error || `Job завершён со статусом: ${payload.status}.`);
      }
    });

    source.addEventListener('error', (event) => {
      if (!panel.active || panel.active !== active) return;
      const payload = parsePayload(event.data);
      if (payload.message || payload.error) {
        finishWithError(panel, payload.error || payload.message);
        return;
      }
      if (source.readyState === EventSource.CLOSED) {
        finishWithError(panel, 'SSE-поток неожиданно завершился.');
      } else {
        setStatus(panel, 'reconnecting');
      }
    });
  }

  function finishSuccess(panel) {
    if (!panel.active) return;
    const active = panel.active;
    closeStream(panel);
    if (active.text) panel.history.push({ role: 'assistant', content: active.text });
    panel.active = null;
    setBusy(panel, false);
    setStatus(panel, 'completed');
    renderMessages(panel);
    setGlobalNotice('Ответы поступают потоково; метрики обновились.');
  }

  function finishWithError(panel, message) {
    if (!panel.active) return;
    closeStream(panel);
    panel.active = null;
    setBusy(panel, false);
    setStatus(panel, 'error');
    setPanelError(panel, friendlyMessage(message));
    renderMessages(panel);
  }

  function friendlyMessage(message) {
    const text = String(message || 'Неизвестная ошибка.');
    if (/failed to fetch|networkerror|load failed/i.test(text)) return 'Не удалось подключиться к backend. Проверьте адрес сервиса и сеть.';
    return text;
  }

  async function sendPanel(panel, forcedText = '') {
    if (panel.busy) return false;
    const input = $('[data-role="input"]', panel.node);
    const text = (forcedText || input.value).trim();
    if (!text) {
      setPanelError(panel, 'Введите сообщение перед отправкой.');
      input.focus();
      return false;
    }
    if (text.length > MAX_INPUT_CHARS) {
      setPanelError(panel, `Сообщение слишком длинное. Максимум — ${MAX_INPUT_CHARS.toLocaleString('ru-RU')} символов.`);
      return false;
    }

    setPanelError(panel);
    panel.history.push({ role: 'user', content: text });
    input.value = '';
    updateCounter(panel);
    panel.metrics = { wait: null, ttft: null, total: null };
    panel.active = { text: '', startedAt: performance.now(), firstTokenAt: null };
    setBusy(panel, true);
    setStatus(panel, 'connecting');
    renderMessages(panel);

    try {
      const job = await postJob(panel.history, panel.id);
      if (!job.job_id) throw new Error('Backend не вернул job_id.');
      panel.active.jobId = job.job_id;
      panel.active.eventsURL = job.events_url;
      $('[data-role="job"]', panel.node).textContent = String(job.job_id);
      setStatus(panel, job.status || 'queued', job);
      if (!job.events_url) throw new Error('Backend не вернул events_url.');
      openEvents(panel, job.events_url);
      refreshQueue();
      return true;
    } catch (error) {
      finishWithError(panel, error.message || error);
      return false;
    }
  }

  function clearPanel(panel) {
    closeStream(panel);
    panel.history = [];
    panel.active = null;
    panel.metrics = { wait: null, ttft: null, total: null };
    const input = $('[data-role="input"]', panel.node);
    input.value = '';
    $('[data-role="job"]', panel.node).textContent = '—';
    $('[data-role="position"]', panel.node).textContent = '—';
    setPanelError(panel);
    setBusy(panel, false);
    setStatus(panel, 'idle');
    updateCounter(panel);
    renderMessages(panel);
  }

  function fillDemo() {
    const a = panels.get('client-a');
    const b = panels.get('client-b');
    $('[data-role="input"]', a.node).value = DEMO_LONG;
    $('[data-role="input"]', b.node).value = DEMO_SHORT;
    updateCounter(a);
    updateCounter(b);
    setGlobalNotice('Демо подготовлено: длинный запрос в Клиенте A, короткий — в Клиенте B. Нажмите «Запустить A + B».');
    $('[data-role="input"]', a.node).focus();
  }

  async function runBoth() {
    const a = panels.get('client-a');
    const b = panels.get('client-b');
    if (!a.busy && !b.busy) {
      if (!String($('[data-role="input"]', a.node).value).trim()) $('[data-role="input"]', a.node).value = DEMO_LONG;
      if (!String($('[data-role="input"]', b.node).value).trim()) $('[data-role="input"]', b.node).value = DEMO_SHORT;
      updateCounter(a);
      updateCounter(b);
    }
    await Promise.all([sendPanel(a), sendPanel(b)]);
  }

  async function runRateLimitTest(button) {
    if (button.disabled) return;
    button.disabled = true;
    setGlobalNotice('Отправляю 6 быстрых запросов с одного IP…');
    try {
      const responses = await Promise.all(Array.from({ length: 6 }, (_, index) => fetch(`${API_ROOT}/jobs`, {
        method: 'POST',
        headers: authHeaders(true),
        body: JSON.stringify({ messages: [{ role: 'user', content: `Проверка rate limit #${index + 1}` }], client_id: 'rate-limit-test' }),
      })));
      const accepted = responses.filter((response) => response.ok).length;
      const limited = responses.filter((response) => response.status === 429).length;
      setGlobalNotice(`Rate limit: принято ${accepted}, отклонено с HTTP 429 — ${limited}. Остальные ответы: ${responses.length - accepted - limited}.`);
      refreshQueue();
    } catch (error) {
      setGlobalNotice(`Rate limit: ${friendlyMessage(error.message || error)}`, true);
    } finally {
      button.disabled = false;
    }
  }

  async function runContextTest(button) {
    if (button.disabled) return;
    button.disabled = true;
    setGlobalNotice('Отправляю сообщение на 16 000 символов…');
    try {
      const response = await fetch(`${API_ROOT}/jobs`, {
        method: 'POST',
        headers: authHeaders(true),
        body: JSON.stringify({
          messages: [{ role: 'user', content: 'Контекстный тест. '.repeat(900) }],
          client_id: 'max-context-test',
        }),
      });
      if (response.status === 413 || response.status === 400 || response.status === 422) {
        setGlobalNotice(`Max context: backend корректно отклонил слишком большой запрос (HTTP ${response.status}).`);
      } else if (response.ok) {
        setGlobalNotice('Max context: backend принял запрос. Лимит контекста пока не сработал или выше 16 000 символов.', true);
      } else {
        setGlobalNotice(`Max context: backend вернул HTTP ${response.status}.`, true);
      }
    } catch (error) {
      setGlobalNotice(`Max context: ${friendlyMessage(error.message || error)}`, true);
    } finally {
      button.disabled = false;
    }
  }

  function getNested(source, names) {
    const candidates = [source, source?.queue, source?.stats, source?.data];
    for (const candidate of candidates) {
      if (!candidate || typeof candidate !== 'object') continue;
      for (const name of names) {
        if (candidate[name] !== undefined && candidate[name] !== null) return candidate[name];
      }
    }
    return null;
  }

  function numberOrDash(value) {
    const number = Number(value);
    return Number.isFinite(number) ? String(number) : '—';
  }

  async function refreshQueue() {
    try {
      const response = await fetch(`${API_ROOT}/queue`, { headers: authHeaders() });
      if (!response.ok) throw new Error(await responseError(response));
      const data = await response.json();
      const queued = getNested(data, ['queued', 'queue_size', 'pending', 'waiting', 'queued_count']);
      const running = getNested(data, ['running', 'active', 'active_jobs', 'running_count']);
      const capacity = getNested(data, ['capacity', 'queue_capacity', 'max_queue', 'limit']);
      $('[data-stat="queued"]').textContent = numberOrDash(queued);
      $('[data-stat="running"]').textContent = numberOrDash(running);
      $('[data-stat="capacity"]').textContent = numberOrDash(capacity);
      const total = (Number(queued) || 0) + (Number(running) || 0);
      $('[data-stat="total"]').textContent = Number.isFinite(total) ? String(total) : '—';
      $('[data-queue-updated]').textContent = `обновлено в ${new Date().toLocaleTimeString('ru-RU', { hour: '2-digit', minute: '2-digit', second: '2-digit' })}`;
    } catch (error) {
      $('[data-queue-updated]').textContent = 'очередь пока недоступна';
      // Do not cover a useful chat error with a polling error on every tick.
      if (!queueTimer) setGlobalNotice(`Статистика очереди: ${friendlyMessage(error.message || error)}`, true);
    }
  }

  function bindPanel(panel) {
    const input = $('[data-role="input"]', panel.node);
    $('[data-role="composer"]', panel.node).addEventListener('submit', (event) => {
      event.preventDefault();
      sendPanel(panel);
    });
    input.addEventListener('input', () => updateCounter(panel));
    input.addEventListener('keydown', (event) => {
      if (event.key === 'Enter' && event.ctrlKey) {
        event.preventDefault();
        sendPanel(panel);
      }
    });
    updateCounter(panel);
  }

  function bindActions() {
    $('[data-action="fill-demo"]').addEventListener('click', fillDemo);
    $('[data-action="run-both"]').addEventListener('click', runBoth);
    $('[data-action="clear-all"]').addEventListener('click', () => {
      panels.forEach(clearPanel);
      setGlobalNotice('Оба мини-чата очищены. Уже запущенные jobs продолжают выполняться на backend.');
    });
    $('[data-action="rate-test"]').addEventListener('click', (event) => runRateLimitTest(event.currentTarget));
    $('[data-action="context-test"]').addEventListener('click', (event) => runContextTest(event.currentTarget));
    $('[data-action="toggle-key"]').addEventListener('click', (event) => {
      const input = $('#api-key');
      input.type = input.type === 'password' ? 'text' : 'password';
      event.currentTarget.setAttribute('aria-label', input.type === 'password' ? 'Показать API key' : 'Скрыть API key');
    });
    $('#api-key').addEventListener('input', () => {
      try { sessionStorage.setItem('llm-ui-api-key', $('#api-key').value); } catch { /* storage may be disabled */ }
    });
  }

  function init() {
    $all('[data-client]').forEach((node) => {
      const panel = createPanel(node);
      panels.set(panel.id, panel);
      panelNodes.set(panel.id, node);
      bindPanel(panel);
    });
    try { $('#api-key').value = sessionStorage.getItem('llm-ui-api-key') || ''; } catch { /* storage may be disabled */ }
    bindActions();
    refreshQueue();
    queueTimer = window.setInterval(refreshQueue, POLL_INTERVAL_MS);
    window.addEventListener('beforeunload', () => {
      panels.forEach(closeStream);
      if (queueTimer) window.clearInterval(queueTimer);
    });
  }

  init();
})();
