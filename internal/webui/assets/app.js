// The processd console. It is a client of the public REST API like any other:
// there is no private endpoint behind this page, and the token the operator
// pastes in is the same bearer token the CLI uses.
'use strict';

const STATES = [
  'CREATED', 'QUEUED', 'STARTING', 'RUNNING', 'STOPPING',
  'CRASHED', 'RETRYING', 'COMPLETED', 'FAILED', 'CANCELED',
];

const TOKEN_KEY = 'processd.token';
const REFRESH_MS = 5000;

const el = (id) => document.getElementById(id);
const api = new URL('../v1/', window.location.href).pathname;

const state = {
  token: localStorage.getItem(TOKEN_KEY) || '',
  view: 'overview',
  cursor: '',
  filters: { state: '', worker: '' },
  selected: null,
  stream: null,
  workers: [],
};

// ---------------------------------------------------------------- transport

function authHeaders() {
  return state.token ? { Authorization: 'Bearer ' + state.token } : {};
}

async function request(path, options = {}) {
  const response = await fetch(api + path, {
    ...options,
    headers: { Accept: 'application/json', ...authHeaders(), ...(options.headers || {}) },
  });

  if (response.status === 401) {
    throw new Error('unauthorized: paste a valid API token');
  }

  if (!response.ok) {
    let message = 'request failed with status ' + response.status;

    try {
      const body = await response.json();
      if (body.error) message = body.error.code + ': ' + body.error.message;
    } catch (ignored) { /* the body was not the error contract */ }

    throw new Error(message);
  }

  if (response.status === 204 || response.headers.get('Content-Length') === '0') return null;

  const text = await response.text();
  return text ? JSON.parse(text) : null;
}

function fail(err) {
  const banner = el('banner');
  banner.textContent = err.message;
  banner.hidden = false;
}

function clearFail() {
  el('banner').hidden = true;
}

// ---------------------------------------------------------------- rendering

function short(id) {
  return id.length > 18 ? id.slice(0, 12) + '…' + id.slice(-4) : id;
}

function duration(ms) {
  if (ms === null || ms === undefined) return '—';
  if (ms < 1000) return ms + 'ms';
  const seconds = ms / 1000;
  if (seconds < 60) return seconds.toFixed(1) + 's';
  const minutes = Math.floor(seconds / 60);
  return minutes + 'm' + String(Math.floor(seconds % 60)).padStart(2, '0') + 's';
}

function when(value) {
  if (!value) return '—';
  return new Date(value).toLocaleString();
}

function bytes(value) {
  if (!value) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB'];
  let size = value;
  let unit = 0;
  while (size >= 1024 && unit < units.length - 1) { size /= 1024; unit++; }
  return size.toFixed(unit === 0 ? 0 : 1) + ' ' + units[unit];
}

function cell(text, className) {
  const td = document.createElement('td');
  td.textContent = text;
  if (className) td.className = className;
  return td;
}

// A service restarts for as long as it is meant to run, so it reports no
// attempt ceiling at all.
function attemptCeiling(item) {
  return item.max_attempts === null ? '\u221e' : item.max_attempts;
}

function processRow(item, columns) {
  const row = document.createElement('tr');
  row.append(cell(short(item.id), 'id'));
  row.append(cell(item.worker || item.command, ''));

  const status = cell(item.status, 'state state-' + item.status);
  row.append(status);

  row.append(cell(item.attempt + '/' + attemptCeiling(item), ''));
  if (columns.pid) row.append(cell(item.pid ? String(item.pid) : '—', ''));
  row.append(cell(duration(item.duration_ms), ''));
  row.append(cell(when(item.created_at), ''));
  row.addEventListener('click', () => openDrawer(item.id));

  return row;
}

// ---------------------------------------------------------------- overview

async function loadOverview() {
  const [health, stats, page] = await Promise.all([
    request('health?deep=1').catch((err) => ({ status: 'degraded', store: err.message })),
    request('stats'),
    request('processes?limit=10'),
  ]);

  el('version').textContent = health.version ? 'v' + health.version : '';
  el('card-status').textContent = health.status;
  el('card-store').textContent = health.store ? 'store ' + health.store : '';
  el('health').className = 'dot ' + (health.status === 'ok' ? 'ok' : 'bad');

  el('card-slots').textContent = stats.slots_used + ' / ' + stats.slots_max;
  el('card-slots-bar').style.width =
    (stats.slots_max ? (100 * stats.slots_used) / stats.slots_max : 0) + '%';
  el('card-running').textContent = stats.running;
  el('card-queue').textContent = stats.queue_depth;
  el('card-workers').textContent = stats.workers;

  const chips = el('state-chips');
  chips.textContent = '';
  const active = Object.entries(stats.states || {}).filter(([, count]) => count > 0);

  if (active.length === 0) {
    const empty = document.createElement('span');
    empty.className = 'hint';
    empty.textContent = 'nothing active';
    chips.append(empty);
  }

  for (const [name, count] of active.sort()) {
    const chip = document.createElement('span');
    chip.className = 'chip';
    chip.innerHTML = '<b></b> ';
    chip.querySelector('b').textContent = count;
    chip.append(name);
    chips.append(chip);
  }

  const body = el('recent').querySelector('tbody');
  body.textContent = '';
  for (const item of page.items) body.append(processRow(item, { pid: false }));
}

// --------------------------------------------------------------- processes

function processQuery() {
  const params = new URLSearchParams({ limit: '50' });
  if (state.filters.state) params.set('status', state.filters.state);
  if (state.filters.worker) params.set('worker', state.filters.worker);
  if (state.cursor) params.set('cursor', state.cursor);
  return params.toString();
}

async function loadProcesses(append) {
  if (!append) state.cursor = '';

  const page = await request('processes?' + processQuery());
  const body = el('processes').querySelector('tbody');

  if (!append) body.textContent = '';
  for (const item of page.items) body.append(processRow(item, { pid: true }));

  state.cursor = page.next_cursor || '';
  el('load-more').hidden = !state.cursor;
}

// ----------------------------------------------------------------- workers

async function loadWorkers() {
  state.workers = await request('workers');
  const body = el('workers').querySelector('tbody');
  body.textContent = '';

  for (const worker of state.workers) {
    const row = document.createElement('tr');
    row.style.cursor = 'default';
    row.append(cell(worker.name, ''));
    row.append(cell(worker.type, ''));
    row.append(cell(worker.enabled ? 'yes' : 'no', ''));
    row.append(cell(worker.command, 'id'));
    row.append(cell(worker.max_processes || '—', ''));
    row.append(cell(Object.keys(worker.params || {}).join(', ') || '—', ''));

    const action = document.createElement('td');
    const button = document.createElement('button');
    button.type = 'button';
    button.textContent = 'Run';
    button.disabled = !worker.enabled;
    button.addEventListener('click', () => openRun(worker));
    action.append(button);
    row.append(action);

    body.append(row);
  }
}

function openRun(worker) {
  el('run-title').textContent = 'Run ' + worker.name;
  el('run-error').textContent = '';

  const fields = el('run-params');
  fields.textContent = '';

  for (const [name, rule] of Object.entries(worker.params || {})) {
    const label = document.createElement('label');
    label.textContent = name + ' (' + rule + ')';
    const input = document.createElement('input');
    input.type = 'text';
    input.dataset.param = name;
    input.spellcheck = false;
    label.append(input);
    fields.append(label);
  }

  el('run-form').dataset.worker = worker.name;
  el('run-modal').hidden = false;
}

async function submitRun(event) {
  event.preventDefault();

  const params = {};
  for (const input of el('run-params').querySelectorAll('input')) {
    if (input.value !== '') params[input.dataset.param] = input.value;
  }

  try {
    const created = await request('processes', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ worker: el('run-form').dataset.worker, params }),
    });

    el('run-modal').hidden = true;
    show('processes');
    await loadProcesses(false);
    openDrawer(created.id);
  } catch (err) {
    el('run-error').textContent = err.message;
  }
}

// ------------------------------------------------------------------ drawer

async function openDrawer(id) {
  state.selected = id;
  el('drawer').hidden = false;
  el('drawer-title').textContent = id;

  await refreshDrawer();

  const attempts = el('log-attempt');
  attempts.value = attempts.options.length ? attempts.options[attempts.options.length - 1].value : '1';

  showLogs();
}

function closeDrawer() {
  stopStream();
  state.selected = null;
  el('drawer').hidden = true;
}

function field(list, name, value) {
  const dt = document.createElement('dt');
  dt.textContent = name;
  const dd = document.createElement('dd');
  dd.textContent = value;
  list.append(dt, dd);
}

async function refreshDrawer() {
  if (!state.selected) return;

  const item = await request('processes/' + encodeURIComponent(state.selected));
  el('drawer-sub').textContent = item.worker + ' — ' + item.status +
    (item.reason ? ' (' + item.reason + ')' : '');

  const fields = el('drawer-fields');
  fields.textContent = '';
  field(fields, 'command', [item.command, ...(item.args || [])].join(' '));
  field(fields, 'cwd', item.cwd);
  field(fields, 'attempt', item.attempt + ' of ' + attemptCeiling(item));
  field(fields, 'pid', item.pid || '—');
  field(fields, 'exit code', item.exit_code === null ? '—' : item.exit_code);
  if (item.signal) field(fields, 'signal', item.signal);
  if (item.lock) field(fields, 'lock', item.lock);
  field(fields, 'created', when(item.created_at));
  field(fields, 'started', when(item.started_at));
  field(fields, 'finished', when(item.finished_at));
  field(fields, 'duration', duration(item.duration_ms));

  if (item.usage) {
    field(fields, 'cpu', item.usage.cpu_seconds.toFixed(2) + 's');
    field(fields, 'memory', bytes(item.usage.rss_bytes));
    field(fields, 'threads', item.usage.threads);
  }

  if (item.log_truncated) field(fields, 'logs', 'output dropped: size cap or rotation');

  const attempts = el('log-attempt');
  const wanted = Math.max(item.attempt, 1);

  if (attempts.options.length !== wanted) {
    const current = attempts.value;
    attempts.textContent = '';

    for (let i = 1; i <= wanted; i++) {
      const option = document.createElement('option');
      option.value = String(i);
      option.textContent = String(i);
      attempts.append(option);
    }

    attempts.value = current && Number(current) <= wanted ? current : String(wanted);
  }
}

// ------------------------------------------------------------- log streaming

function pushLine(text, className) {
  const view = el('logs');
  const atBottom = view.scrollHeight - view.scrollTop - view.clientHeight < 40;

  const line = document.createElement('span');
  if (className) line.className = className;
  line.textContent = text + '\n';
  view.append(line);

  if (atBottom) view.scrollTop = view.scrollHeight;
}

function stopStream() {
  if (state.stream) {
    state.stream.abort();
    state.stream = null;
  }
}

// showLogs either follows the attempt over SSE or reads what it has written so
// far. A finished attempt is followed too: the stream ends by itself, and the
// end event says how the attempt turned out.
function showLogs() {
  stopStream();

  if (!state.selected) return;

  el('logs').textContent = '';

  if (el('log-follow').checked) {
    followLogs();
    return;
  }

  readLogs().catch((err) => {
    el('log-status').textContent = 'read failed';
    fail(err);
  });
}

function logQuery(tail) {
  return new URLSearchParams({
    stream: el('log-stream').value,
    attempt: el('log-attempt').value || '1',
    tail: String(tail),
  }).toString();
}

async function readLogs() {
  el('log-status').textContent = 'reading…';

  const path = 'processes/' + encodeURIComponent(state.selected) + '/logs?' + logQuery(500);
  const body = await request(path);

  for (const line of body.lines) pushLine(line, line.startsWith('stderr: ') ? 'stderr' : '');

  el('log-status').textContent = body.lines.length ? body.lines.length + ' lines' : 'no output';

  if (body.truncated) pushLine('— some output was dropped: size cap or rotation', 'meta');
}

function followLogs() {
  el('log-status').textContent = 'connecting…';

  const controller = new AbortController();
  state.stream = controller;

  const path = 'processes/' + encodeURIComponent(state.selected) + '/logs/stream?' + logQuery(200);

  consume(path, controller).catch((err) => {
    if (controller.signal.aborted) return;
    el('log-status').textContent = 'stream failed';
    fail(err);
  });
}

async function consume(path, controller) {
  const response = await fetch(api + path, {
    headers: { Accept: 'text/event-stream', ...authHeaders() },
    signal: controller.signal,
  });

  if (!response.ok) throw new Error('log stream failed with status ' + response.status);

  el('log-status').textContent = 'following';

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';

  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;

    buffer += decoder.decode(value, { stream: true });

    let split = buffer.indexOf('\n\n');
    while (split >= 0) {
      handleEvent(buffer.slice(0, split));
      buffer = buffer.slice(split + 2);
      split = buffer.indexOf('\n\n');
    }
  }

  if (el('log-status').textContent === 'following') el('log-status').textContent = 'closed';
}

function handleEvent(frame) {
  let name = 'message';
  let data = '';

  for (const line of frame.split('\n')) {
    if (line.startsWith('event: ')) name = line.slice(7).trim();
    if (line.startsWith('data: ')) data += line.slice(6);
  }

  if (name === 'line') {
    const payload = JSON.parse(data);
    pushLine(payload.text, payload.stream === 'stderr' ? 'stderr' : '');
    return;
  }

  if (name === 'end') {
    const payload = JSON.parse(data);
    pushLine('— attempt ' + payload.attempt + ' ended: ' + payload.status +
      (payload.truncated ? ' (some output dropped)' : ''), 'meta');
    el('log-status').textContent = 'ended';
    refreshDrawer().catch(fail);
  }
}

// ------------------------------------------------------------------ actions

async function stopSelected() {
  const grace = encodeURIComponent(el('stop-grace').value.trim());
  await request('processes/' + encodeURIComponent(state.selected) + '?grace=' + grace, { method: 'DELETE' });
  await refreshDrawer();
}

async function signalSelected() {
  await request('processes/' + encodeURIComponent(state.selected) + '/signal', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ signal: el('signal-name').value }),
  });
}

// -------------------------------------------------------------------- shell

function show(view) {
  state.view = view;

  for (const tab of document.querySelectorAll('.tab')) {
    tab.classList.toggle('active', tab.dataset.view === view);
  }

  for (const name of ['overview', 'processes', 'workers']) {
    el('view-' + name).hidden = name !== view;
  }

  refresh().catch(fail);
}

async function refresh() {
  if (!state.token) {
    fail(new Error('paste an API token to talk to the daemon'));
    return;
  }

  try {
    if (state.view === 'overview') await loadOverview();
    if (state.view === 'processes') await loadProcesses(false);
    if (state.view === 'workers') await loadWorkers();
    if (state.selected) await refreshDrawer();
    clearFail();
  } catch (err) {
    fail(err);
    throw err;
  }
}

function wire() {
  el('token').value = state.token;

  el('auth-form').addEventListener('submit', (event) => {
    event.preventDefault();
    state.token = el('token').value.trim();
    localStorage.setItem(TOKEN_KEY, state.token);
    refresh().catch(() => {});
  });

  for (const tab of document.querySelectorAll('.tab')) {
    tab.addEventListener('click', () => show(tab.dataset.view));
  }

  const states = el('filter-state');
  for (const name of STATES) {
    const option = document.createElement('option');
    option.value = name;
    option.textContent = name;
    states.append(option);
  }

  el('filters').addEventListener('submit', (event) => {
    event.preventDefault();
    state.filters = { state: states.value, worker: el('filter-worker').value.trim() };
    loadProcesses(false).catch(fail);
  });

  el('load-more').addEventListener('click', () => loadProcesses(true).catch(fail));
  el('drawer-close').addEventListener('click', closeDrawer);
  el('action-stop').addEventListener('click', () => stopSelected().catch(fail));
  el('action-signal').addEventListener('click', () => signalSelected().catch(fail));
  el('log-stream').addEventListener('change', showLogs);
  el('log-attempt').addEventListener('change', showLogs);
  el('log-follow').addEventListener('change', showLogs);
  el('run-form').addEventListener('submit', submitRun);
  el('run-cancel').addEventListener('click', () => { el('run-modal').hidden = true; });

  document.addEventListener('keydown', (event) => {
    if (event.key !== 'Escape') return;
    if (!el('run-modal').hidden) el('run-modal').hidden = true;
    else if (!el('drawer').hidden) closeDrawer();
  });

  setInterval(() => {
    if (el('auto-refresh').checked) refresh().catch(() => {});
  }, REFRESH_MS);
}

wire();
show('overview');
