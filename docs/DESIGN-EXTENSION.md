# M5 — Drive the user's real Chrome through an extension

## Why

`--attach` over CDP works, but Chrome ≥ 144 gates every new remote-debugging client behind an
"Allow remote debugging?" dialog, and Chrome ≥ 136 refuses `--remote-debugging-port` on the default
profile. Users end up with a second Chrome and a second set of logins. Claude in Chrome, Browser Use
and others solve this the same way: **a Chrome extension is the bridge**. It runs inside the user's
real profile (cookies, sessions, extensions, password manager), needs one install and one click, and
can use `chrome.debugger` — the same CDP surface we already speak — on any tab.

## Prior art

| | transport | tabs | approval |
|---|---|---|---|
| **Claude in Chrome / Claude Code** | Native Messaging: Chrome spawns a host binary registered in `NativeMessagingHosts/*.json`, JSON over stdin/stdout | on demand, per site permission dialog | once per site |
| **OpenClaw browser relay** | extension is a WebSocket **client** to a loopback relay (`127.0.0.1:18792`) | auto-attaches every eligible tab, re-attaches on reload | toolbar toggle |
| **browser-ai (this)** | extension is a ws client to `cmd/agent` (loopback, token) — the OpenClaw shape, zero install steps beyond loading the extension | **only tabs the agent creates** by default; `--tab current` to drive the tab the user is on | one click in the popup per agent session |

Native Messaging is the better long-term default (Chrome launches the bridge, nothing to start by
hand) but needs a host manifest per browser/OS and a host binary; it is scheduled as M6, behind the
same `cdp.Transport` so nothing above it changes.

## Shape

```
┌──────────────────────── user's Chrome ────────────────────────┐
│  extension (MV3)                                               │
│   service worker ── chrome.debugger.attach(tabId) ── CDP ──► tab│
│        │  ws://127.0.0.1:<port>/ext  (extension is the CLIENT)  │
└────────┼──────────────────────────────────────────────────────┘
         ▼
  cmd/agent  ── internal/extbridge (ws server, stdlib) ── cdp.Transport
                                                            │
                                     internal/cdp / browser / agent unchanged
```

- The Go side **listens** (`--via-extension [port]`, default 9223, loopback only); the extension
  connects out. No debugging port on Chrome, no approval dialog, no separate profile.
- The bridge is just another `cdp.Transport`: JSON in, JSON out. Messages carry the same
  `{id, method, params, sessionId}` envelope; the service worker forwards `method/params` to
  `chrome.debugger.sendCommand(target, method, params)` and relays `chrome.debugger.onEvent` back as
  `{method, params, sessionId}`. `Target.*` calls the extension handles itself (create tab, attach,
  close) because `chrome.debugger` is per tab.
- Everything above the transport — snapshot, freshness guards, executor, Jev, agent loop, timing —
  is reused as is. The agent does not know it is talking to an extension.

## Protocol between agent and extension

Text frames, one JSON object each.

| direction | message | meaning |
|---|---|---|
| ext → agent | `{"hello": {"version": "…", "ua": "…"}}` | after connect; agent prints "extension connected" |
| agent → ext | `{"id", "method": "Target.createTarget", "params": {"url", "background"}}` | ext: `chrome.tabs.create({active:false})`, `chrome.debugger.attach({tabId}, "1.3")` → reply `{"id", "result": {"targetId": "<tabId>"}}` |
| agent → ext | `{"id", "method": "Target.attachToTarget", "params": {"targetId"}}` | reply `{"id","result":{"sessionId": "<tabId>"}}` (flat: sessionId == tabId) |
| agent → ext | `{"id", "sessionId", "method": "<CDP method>", "params"}` | `chrome.debugger.sendCommand({tabId}, method, params)` → `{"id", "result"}` or `{"id", "error": {code, message}}` |
| ext → agent | `{"sessionId", "method", "params"}` | forwarded `chrome.debugger.onEvent` |
| agent → ext | `{"id", "method": "Target.closeTarget", "params": {"targetId"}}` | detach + `chrome.tabs.remove` |
| ext → agent | `{"method": "Target.detachedFromTarget", "params": {"sessionId"}}` | user closed the tab or clicked "Cancel" on the debugger bar |

Security: bind `127.0.0.1` only; a random token in the URL (`/ext?token=…`) printed by the CLI and
pasted once into the extension options (stored in `chrome.storage.local`); one client at a time.
Model output still never becomes selectors or code — the extension executes only CDP methods the
agent already uses (`Runtime.evaluate` with our embedded scripts, `Input.*`, `Emulation.*`,
`Page.captureScreenshot`).

## Tab model

- Default: the agent asks for a new background tab (`Target.createTarget`) and closes it at the end.
  The user's tabs are never attached. Chrome shows its "browser-ai is debugging this tab" bar on
  that one tab only.
- `--tab current`: attach the active tab instead — for tasks that need state already on screen.
  The agent still refuses to navigate away unless the goal says so.
- Never `attach` to every tab (OpenClaw does; we don't need it and it multiplies debugger bars).

## Extension (unpacked, MV3, no build step)

```
extension/
  manifest.json      permissions: debugger, tabs, storage; host_permissions: <all_urls>
  background.js      ws client w/ reconnect, chrome.debugger bridge, tab ownership
  options.html/js    agent port + token, connection status, "auto-connect" toggle
  icon.png
```

`chrome.debugger.attach` shows Chrome's yellow "… is debugging this browser" bar — expected, and
the honest signal that an agent is driving a tab. Focus emulation and viewport override work the
same via `Emulation.*`.

## Go side

- `internal/extbridge`: `Listen(addr, token) (*Server)`, `Server.Wait(ctx) (cdp.Transport, Hello, error)`
  — minimal RFC 6455 **server** handshake (mirror of `internal/ws`), single connection, ping/pong.
- `cmd/agent --via-extension[=port]`: start server, print `token` and wait for the extension
  (hint after 3 s: "open the extension popup and click Connect"), then proceed exactly as `--attach`.
- `cmd/bench rtt --via-extension`: RTT through `chrome.debugger` — expect ~0.5–1 ms (extra hop
  through the service worker) vs 0.2 ms direct; snapshot cost unchanged.

## Limits

- `chrome.debugger` cannot attach to `chrome://` pages, the Web Store, or other extensions' pages.
- One debugger client per tab: DevTools open on the tab blocks us (and vice versa).
- MV3 service workers sleep after 30 s idle; the ws connection keeps it alive while a run is active,
  and `chrome.alarms` re-connects otherwise.
- Firefox/Safari have no `debugger` API — Chrome/Edge/Brave only.

## Milestone M5 issues

1. `internal/extbridge`: ws server + Transport + token check + tests with `internal/ws` as client
2. `extension/`: manifest, background bridge, Target.* emulation, reconnect, options page
3. `cmd/agent --via-extension`, `cmd/bench rtt --via-extension`
4. e2e: fixture test driving a real Chrome with the unpacked extension loaded (`--load-extension`)
5. docs: install steps, screenshots of the debugger bar, BENCH row for the extension hop
