// browser-ai bridge — service worker.
//
// Connects OUT to the local agent (ws://127.0.0.1:<port>/ext?token=…) and
// turns each CDP request into chrome.debugger.sendCommand on a tab the agent
// owns. Target.* is emulated here because chrome.debugger is per tab.
// Only the agent's messages are ever executed; nothing from web pages.

const VERSION = chrome.runtime.getManifest().version;
const DEFAULTS = { port: 9223, token: "", autoconnect: false, allowCurrentTab: false };

let ws = null;
let backoff = 1000;
let wantConnected = false;
const owned = new Set();          // tabIds attached by this worker
const status = { state: "off", detail: "" };

function setStatus(state, detail = "") {
  status.state = state; status.detail = detail;
  chrome.runtime.sendMessage({ type: "status", ...status }).catch(() => {});
  chrome.action.setBadgeText({ text: state === "connected" ? "on" : state === "connecting" ? "…" : "" }).catch(() => {});
  chrome.action.setBadgeBackgroundColor({ color: state === "connected" ? "#0a7" : "#888" }).catch(() => {});
}

async function settings() {
  return { ...DEFAULTS, ...(await chrome.storage.local.get(Object.keys(DEFAULTS))) };
}

function send(obj) {
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(obj));
}

async function connect() {
  wantConnected = true;
  if (ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING)) return;
  const { port, token } = await settings();
  if (!token) { setStatus("off", "no token configured"); return; }
  setStatus("connecting", `127.0.0.1:${port}`);
  ws = new WebSocket(`ws://127.0.0.1:${port}/ext?token=${encodeURIComponent(token)}`);
  ws.onopen = () => {
    backoff = 1000;
    send({ hello: { version: VERSION, ua: navigator.userAgent, browser: "chrome" } });
    setStatus("connected", `127.0.0.1:${port}`);
  };
  ws.onmessage = (ev) => handle(ev.data).catch((e) => console.error("bridge:", e));
  ws.onclose = async () => {
    ws = null;
    await detachAll();
    if (wantConnected) {
      setStatus("connecting", `retry in ${backoff / 1000}s`);
      setTimeout(connect, backoff);
      backoff = Math.min(backoff * 2, 30000);
    } else {
      setStatus("off");
    }
  };
  ws.onerror = () => {};
}

async function disconnect() {
  wantConnected = false;
  if (ws) ws.close();
  await detachAll();
  setStatus("off");
}

async function detachAll() {
  for (const tabId of [...owned]) {
    try { await chrome.debugger.detach({ tabId }); } catch {}
    owned.delete(tabId);
  }
}

async function attach(tabId) {
  await chrome.debugger.attach({ tabId }, "1.3");
  owned.add(tabId);
  return { targetId: String(tabId) };
}

// Emulated Target domain. targetId and sessionId are both the tabId as a string.
const targets = {
  async "Target.createTarget"(p = {}) {
    const tab = await chrome.tabs.create({ url: p.url || "about:blank", active: !p.background });
    return attach(tab.id);
  },
  async "Target.attachToTarget"(p) {
    const tabId = Number(p.targetId);
    if (!owned.has(tabId)) await attach(tabId);
    return { sessionId: String(tabId) };
  },
  async "Target.closeTarget"(p) {
    const tabId = Number(p.targetId);
    try { await chrome.debugger.detach({ tabId }); } catch {}
    owned.delete(tabId);
    try { await chrome.tabs.remove(tabId); } catch {}
    return { success: true };
  },
  async "Target.getTargets"() {
    const tabs = await chrome.tabs.query({});
    return { targetInfos: tabs.map((t) => ({ targetId: String(t.id), type: "page", title: t.title, url: t.url, attached: owned.has(t.id) })) };
  },
  // browser-ai extension: drive the tab the user is looking at (opt-in in the popup).
  async "Ext.currentTab"() {
    const { allowCurrentTab } = await settings();
    if (!allowCurrentTab) throw new Error("driving the current tab is disabled in the extension popup");
    const [tab] = await chrome.tabs.query({ active: true, lastFocusedWindow: true });
    if (!tab) throw new Error("no active tab");
    return attach(tab.id);
  },
  async "Ext.ping"() { return { version: VERSION }; },
};

async function handle(text) {
  let msg;
  try { msg = JSON.parse(text); } catch { return; }
  if (msg.id === undefined || typeof msg.method !== "string") return;
  try {
    let result;
    if (targets[msg.method]) {
      result = await targets[msg.method](msg.params || {});
    } else if (msg.method.startsWith("Browser.") && msg.method === "Browser.getVersion") {
      result = { protocolVersion: "1.3", product: navigator.userAgent.match(/Chrome\/[\d.]+/)?.[0] || "Chrome", userAgent: navigator.userAgent, jsVersion: "" };
    } else {
      const tabId = Number(msg.sessionId);
      if (!owned.has(tabId)) throw new Error(`no attached tab for session ${msg.sessionId}`);
      result = await chrome.debugger.sendCommand({ tabId }, msg.method, msg.params || {});
    }
    send({ id: msg.id, sessionId: msg.sessionId, result: result ?? {} });
  } catch (e) {
    send({ id: msg.id, sessionId: msg.sessionId, error: { code: -32000, message: String(e && e.message || e) } });
  }
}

chrome.debugger.onEvent.addListener((source, method, params) => {
  if (source.tabId && owned.has(source.tabId)) send({ sessionId: String(source.tabId), method, params: params || {} });
});

chrome.debugger.onDetach.addListener((source, reason) => {
  if (!source.tabId || !owned.has(source.tabId)) return;
  owned.delete(source.tabId);
  send({ method: "Target.detachedFromTarget", params: { sessionId: String(source.tabId), targetId: String(source.tabId), reason } });
});

chrome.tabs.onRemoved.addListener((tabId) => {
  if (owned.delete(tabId)) send({ method: "Target.detachedFromTarget", params: { sessionId: String(tabId), targetId: String(tabId), reason: "tab closed" } });
});

// Popup and tests talk to the worker through runtime messages.
chrome.runtime.onMessage.addListener((msg, _sender, reply) => {
  (async () => {
    if (msg.type === "connect") { await connect(); reply(status); }
    else if (msg.type === "disconnect") { await disconnect(); reply(status); }
    else if (msg.type === "status") reply({ ...status, owned: [...owned] });
    else reply(null);
  })();
  return true;
});

// Keep the worker alive and reconnect while a session is wanted.
chrome.alarms.create("bridge-keepalive", { periodInMinutes: 0.5 });
chrome.alarms.onAlarm.addListener(async () => {
  const { autoconnect } = await settings();
  if ((wantConnected || autoconnect) && !(ws && ws.readyState === WebSocket.OPEN)) connect();
});
chrome.runtime.onStartup.addListener(async () => { if ((await settings()).autoconnect) connect(); });
settings().then((s) => { if (s.autoconnect) connect(); });

// Exposed for the e2e test, which configures the worker over CDP.
globalThis.__bridge = { connect, disconnect, status: () => ({ ...status, owned: [...owned] }) };
