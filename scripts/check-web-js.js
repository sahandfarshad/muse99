// check-web-js.js — run the Muse99 web UI page script with stubbed
// browser globals and prove it loads, defines its functions, and
// fills the Settings pane. This exists because v6 shipped a page
// whose script had a duplicate `let allModels` declaration: the
// whole script failed to parse, every button on the page was dead,
// and Settings stayed empty — and no test caught it, because the
// tests only ever curled the JSON endpoints, never the page itself.
// It also guards the v10 fixes: a typed API key must not follow a
// provider switch (it used to leak the Gemini key into every other
// provider), a long model-test error must not break the row layout,
// and the background-task bar must show a running task.
//
// Usage: node scripts/check-web-js.js <path-to-extracted-script.js>
// Exit 0 when the page works, non-zero with the reason on failure.
'use strict';
const fs = require('fs');
const vm = require('vm');

const scriptPath = process.argv[2];
if (!scriptPath) {
  console.error('usage: node scripts/check-web-js.js <script.js>');
  process.exit(2);
}
const code = fs.readFileSync(scriptPath, 'utf8');

function makeEl() {
  return {
    value: '',
    textContent: '',
    innerHTML: '',
    style: {},
    dataset: {},
    checked: false,
    disabled: false,
    classList: { add() {}, remove() {}, toggle() {} },
    addEventListener() {},
    appendChild() {},
    click() {},
    focus() {},
    closest() { return null; },
  };
}
const els = {};
function getEl(id) {
  if (!els[id]) els[id] = makeEl();
  return els[id];
}

const settingsJSON = {
  base_url: 'https://api.meta.ai/v1',
  model: 'muse-spark-1.3',
  user_name: 'Sahand',
  api_key_masked: '••••••••9999',
  api_key_source: 'saved',
  config_path: '/home/test/.muse99/config.json',
  provider: 'spark',
  providers: [
    { name: 'spark', base_url: 'https://api.meta.ai/v1', model: 'muse-spark-1.3', api_key_masked: '••••••••9999', api_key_source: 'saved' },
    { name: 'openai', base_url: 'https://api.openai.com/v1', model: 'gpt-5', api_key_masked: '(not set)', api_key_source: 'none' },
  ],
  port: 3759, domain: '', tls_enabled: false, allow_shell: false,
  usage: { calls: 0, prompt_tokens: 0, completion_tokens: 0, estimated_cost_usd: 0, last_prompt_tokens: 0, last_completion_tokens: 0, last_label: '' },
  models: [], models_source: 'cached', models_fetched_at: '',
};
const statusJSON = {
  model: 'muse-spark-1.3', mode: 'Muse Spark — Muse mode', user: 'Sahand',
  workdir: '/tmp', shell: false, base_url: 'https://api.meta.ai/v1',
  tasks: 0, chats: 1, servers: 0, goals: 0, provider: 'spark',
  address: 'http://127.0.0.1:3759', port: 3759, domain: '', tls: false,
  api_key_set: true,
  usage: { calls: 0, prompt_tokens: 0, completion_tokens: 0, estimated_cost_usd: 0, last_prompt_tokens: 0, last_completion_tokens: 0, last_label: '' },
};
const providersJSON = {
  ok: true, active: 'spark', model: 'muse-spark-1.3',
  favorites: [{ provider: 'gemini', model: 'gemini-3-flash' }],
  providers: [
    { name: 'gemini', label: 'Gemini', base_url: 'https://generativelanguage.googleapis.com/v1beta/openai/', key_masked: '••••••••1111', has_key: true, model: '', preset: true, removable: false, active: false },
    { name: 'openai', label: 'OpenAI', base_url: 'https://api.openai.com/v1', key_masked: '(not set)', has_key: false, model: 'gpt-5', preset: true, removable: false, active: false },
    { name: 'anthropic', label: 'Anthropic', base_url: 'https://api.anthropic.com/v1', key_masked: '(not set)', has_key: false, model: '', preset: true, removable: false, active: false },
    { name: 'spark', label: 'Muse Spark', base_url: 'https://api.meta.ai/v1', key_masked: '••••••••9999', has_key: true, model: 'muse-spark-1.3', preset: true, removable: false, active: true },
  ],
};
const modelsJSON = { models: [{ id: 'gemini-3-flash', owned_by: 'Google' }, { id: 'gemini-2.5-pro', owned_by: 'Google' }], source: 'live' };
const tasksJSON = [
  { id: 'task-1', goal: 'Make a to do list', status: 'running', steps: ['get_time'], summary: 'step 1: get_time', created_at: new Date().toISOString(), updated_at: new Date().toISOString() },
];

let fetchCalls = [];
async function fetchStub(url, opts) {
  fetchCalls.push(String(url));
  let body = {};
  if (String(url).startsWith('/api/status')) body = statusJSON;
  else if (String(url).startsWith('/api/settings')) body = settingsJSON;
  else if (String(url).startsWith('/api/providers')) body = providersJSON;
  else if (String(url).startsWith('/api/models')) body = modelsJSON;
  else if (String(url).startsWith('/api/task')) body = tasksJSON;
  else if (String(url).startsWith('/api/chats')) body = [];
  return { ok: true, status: 200, json: async () => body };
}

const sandbox = {
  console,
  setTimeout, clearTimeout, setInterval, clearInterval,
  JSON, Promise, Date, Math, Number, String, Array, Object,
  window: { location: { href: 'http://127.0.0.1:3759/' }, scrollTo() {} },
  document: {
    getElementById: (id) => getEl(id),
    querySelectorAll: () => [],
    createElement: () => makeEl(),
    body: { scrollHeight: 0 },
  },
  fetch: fetchStub,
  alert() {}, prompt() { return null; }, confirm() { return false; },
};
sandbox.window.document = sandbox.document;
sandbox.globalThis = sandbox;
vm.createContext(sandbox);

let unhandled = null;
process.on('unhandledRejection', (e) => { if (!unhandled) unhandled = e; });

const check = `
;(globalThis.__check = {
  tab: typeof tab,
  loadSettings: typeof loadSettings,
  saveSettings: typeof saveSettings,
  loadProviders: typeof loadProviders,
  loadStatus: typeof loadStatus,
  loadChats: typeof loadChats,
  add: typeof add,
  refreshTaskBar: typeof refreshTaskBar,
  renderPvList: typeof renderPvList,
});
;(globalThis.__harness = {
  renderPvList,
  openTab(n) { curProv = n; renderProvTabs(); renderProvPane(); },
  setTestModels(n, m) { pd(n).list = m; pd(n).loading = false; },
  setModelResult(n, id, r) { pd(n).res[id] = r; },
});`;

try {
  vm.runInContext(code + check, sandbox, { filename: 'muse99-page.js' });
} catch (e) {
  console.error('PAGE SCRIPT FAILED TO RUN:', e && e.message);
  process.exit(1);
}

(async () => {
  await new Promise((r) => setTimeout(r, 100));
  const failures = [];
  const c = sandbox.__check || {};
  for (const [name, want] of Object.entries({
    tab: 'function', loadSettings: 'function', saveSettings: 'function',
    loadProviders: 'function', loadStatus: 'function', loadChats: 'function',
    refreshTaskBar: 'function', renderPvList: 'function',
  })) {
    if (c[name] !== want) failures.push(`page function ${name} is ${c[name]}, not a function`);
  }
  if (unhandled) failures.push('unhandled rejection while loading: ' + unhandled);
  if (!fetchCalls.some((u) => u.startsWith('/api/status'))) {
    failures.push('page never called /api/status on load');
  }
  if (!fetchCalls.some((u) => u.startsWith('/api/chats'))) {
    failures.push('page never called /api/chats on load');
  }
  // Click the Settings tab, the way a user does, and prove the API
  // settings space actually fills in.
  try {
    const fakeBtn = makeEl();
    await sandbox.tab('settings', fakeBtn);
    await new Promise((r) => setTimeout(r, 50));
  } catch (e) {
    failures.push('opening the Settings tab threw: ' + (e && e.message));
  }
  if (!String(getEl('settings').innerHTML).includes('muse-spark-1.3')) {
    failures.push('Settings summary area is empty — no model shown');
  }
  const tabsHTML = String(getEl('ptabs').innerHTML);
  for (const lab of ['Gemini', 'OpenAI', 'Anthropic', 'Muse Spark', '+ New']) {
    if (!tabsHTML.includes(lab)) failures.push('provider tabs miss ' + lab);
  }
  if (!/class="ptab on" data-tab="spark"/.test(tabsHTML)) {
    failures.push('the provider in use (spark) is not the open tab');
  }
  if (!String(getEl('ppane').innerHTML).includes('9999')) {
    failures.push('spark tab does not show its saved key ending');
  }
  // The background-task bar must show a running task (it polls
  // /api/task on load), so background work is visible from Chat.
  if (getEl('taskbar').style.display === 'none') {
    failures.push('background-task bar is hidden while a task is running');
  }
  if (!String(getEl('taskbar').innerHTML).includes('Make a to do list')) {
    failures.push('background-task bar does not show the running task goal');
  }
  // Each tab has its own key box: openai (no key) must not show the
  // spark key, and the add tab shows the add form.
  try {
    const h = sandbox.__harness;
    h.openTab('openai');
    const pane = String(getEl('ppane').innerHTML);
    if (pane.includes('9999')) failures.push('openai tab shows the spark key');
    if (!pane.includes('Paste your OpenAI API key')) failures.push('openai tab does not ask for its key');
    h.openTab('__new');
    if (!String(getEl('ppane').innerHTML).includes('pn-base')) failures.push('+ New tab has no API address field');
  } catch (e) {
    failures.push('provider tab check threw: ' + (e && e.message));
  }
  // A long model-test error must be cut short in the row (full text
  // only in the hover title), or the row breaks the layout.
  try {
    const h = sandbox.__harness;
    h.openTab('gemini');
    h.setTestModels('gemini', [{ id: 'gemini-3-flash', owned_by: 'Google' }]);
    const longError = 'E'.repeat(600);
    h.setModelResult('gemini', 'gemini-3-flash', { ok: false, error: longError });
    h.renderPvList();
    const html = String(getEl('pv-list').innerHTML);
    if (!html.includes('class="star on"')) failures.push('starred model gemini-3-flash is not shown as starred');
    // The full error belongs in the hover title only — the visible
    // row must be short. Strip title attributes before checking.
    const visible = html.replace(/title="[^"]*"/g, '');
    if (visible.includes(longError)) {
      failures.push('model list renders the full 600-char error in the visible row — the row breaks the layout');
    }
    if (!html.includes('…')) {
      failures.push('model list error is not truncated with an ellipsis');
    }
    if (!html.includes('title="') || !html.includes(longError)) {
      failures.push('model list error should keep the full text in a hover title');
    }
  } catch (e) {
    failures.push('model error truncation check threw: ' + (e && e.message));
  }
  if (failures.length) {
    for (const f of failures) console.error('FAIL:', f);
    process.exit(1);
  }
  console.log('page script runs: functions defined, status+chats load, Settings fills in, provider tabs show, key stays per tab, stars show, long errors truncate');
  // The page starts a 5s interval (background-task bar). In a
  // browser that is normal; in Node it keeps the event loop alive
  // forever, so exit explicitly instead of hanging here.
  process.exit(0);
})();
