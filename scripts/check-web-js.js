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
  user_name: 'Takahashi',
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
  model: 'muse-spark-1.3', mode: 'Muse Spark — Muse mode', user: 'Takahashi',
  workdir: '/tmp', shell: false, base_url: 'https://api.meta.ai/v1',
  tasks: 0, chats: 1, servers: 0, goals: 0, provider: 'spark',
  address: 'http://127.0.0.1:3759', port: 3759, domain: '', tls: false,
  api_key_set: true,
  usage: { calls: 0, prompt_tokens: 0, completion_tokens: 0, estimated_cost_usd: 0, last_prompt_tokens: 0, last_completion_tokens: 0, last_label: '' },
};
const tasksJSON = [
  { id: 'task-1', goal: 'Make a to do list', status: 'running', steps: ['get_time'], summary: 'step 1: get_time', created_at: new Date().toISOString(), updated_at: new Date().toISOString() },
];

let fetchCalls = [];
async function fetchStub(url, opts) {
  fetchCalls.push(String(url));
  let body = {};
  if (String(url).startsWith('/api/status')) body = statusJSON;
  else if (String(url).startsWith('/api/settings')) body = settingsJSON;
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
  fetchModels: typeof fetchModels,
  loadStatus: typeof loadStatus,
  loadChats: typeof loadChats,
  add: typeof add,
  refreshTaskBar: typeof refreshTaskBar,
  renderModelList: typeof renderModelList,
});
;(globalThis.__harness = {
  renderModelList,
  setTestModels(m) { allModels = m; },
  setModelResult(id, r) { modelResults[id] = r; },
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
    fetchModels: 'function', loadStatus: 'function', loadChats: 'function',
    refreshTaskBar: 'function', renderModelList: 'function',
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
  if (getEl('set-base').value !== 'https://api.meta.ai/v1') {
    failures.push(`Settings base URL field is ${JSON.stringify(getEl('set-base').value)}, not the saved provider base URL`);
  }
  if (getEl('active-model').textContent !== 'muse-spark-1.3') {
    failures.push(`Settings active model is ${JSON.stringify(getEl('active-model').textContent)}, not muse-spark-1.3`);
  }
  if (!String(getEl('settings').innerHTML).includes('muse-spark-1.3')) {
    failures.push('Settings summary area is empty — no model shown');
  }
  if (!String(getEl('key-source').textContent).includes('9999')) {
    failures.push('Settings key line does not show the saved key');
  }
  // The background-task bar must show a running task (it polls
  // /api/task on load), so background work is visible from Chat.
  if (getEl('taskbar').style.display === 'none') {
    failures.push('background-task bar is hidden while a task is running');
  }
  if (!String(getEl('taskbar').innerHTML).includes('Make a to do list')) {
    failures.push('background-task bar does not show the running task goal');
  }
  // A key typed for one provider must not follow a provider switch:
  // type a key under spark, switch to openai, and the box must be
  // empty (openai has no key); switching back must restore the
  // typed key instead of losing it.
  try {
    getEl('set-key').value = 'sk-test-typed-key';
    getEl('set-provider').value = 'openai';
    if (typeof getEl('set-provider').onchange === 'function') getEl('set-provider').onchange();
    await new Promise((r) => setTimeout(r, 50));
    if (getEl('set-key').value !== '') {
      failures.push(`key box still holds ${JSON.stringify(getEl('set-key').value)} after switching provider — a typed key would be saved under the wrong provider`);
    }
    if (String(getEl('set-key').placeholder || '').includes('9999')) {
      failures.push('key box placeholder still shows the spark key after switching to openai');
    }
    getEl('set-provider').value = 'spark';
    if (typeof getEl('set-provider').onchange === 'function') getEl('set-provider').onchange();
    await new Promise((r) => setTimeout(r, 50));
    if (getEl('set-key').value !== 'sk-test-typed-key') {
      failures.push(`typed key was lost on switching back, got ${JSON.stringify(getEl('set-key').value)}`);
    }
  } catch (e) {
    failures.push('provider-switch key check threw: ' + (e && e.message));
  }
  // A long model-test error must be cut short in the row (full text
  // only in the hover title), or the row breaks the layout.
  try {
    const h = sandbox.__harness;
    h.setTestModels([{ id: 'gemini-3-flash', owned_by: 'Google' }]);
    const longError = 'E'.repeat(600);
    h.setModelResult('gemini-3-flash', { ok: false, error: longError });
    h.renderModelList();
    const html = String(getEl('model-list').innerHTML);
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
  console.log('page script runs: functions defined, status+chats load, Settings fills in, task bar shows, key stays per-provider, long errors truncate');
  // The page starts a 5s interval (background-task bar). In a
  // browser that is normal; in Node it keeps the event loop alive
  // forever, so exit explicitly instead of hanging here.
  process.exit(0);
})();
