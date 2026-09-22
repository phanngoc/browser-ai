# Driving your real Chrome through the bridge extension

The extension in `extension/` lets `cmd/agent` drive a tab **inside the Chrome you already use** —
your profile, cookies, logins, password manager — with no debugging port, no "Allow remote
debugging?" dialog and no second browser. It works the way Claude in Chrome and OpenClaw do: an
MV3 extension uses `chrome.debugger` (the same CDP the agent already speaks) and relays messages
to the agent over a loopback WebSocket.

## Install (once)

1. Chrome → `chrome://extensions` → turn on **Developer mode** (top right).
2. **Load unpacked** → pick the `extension/` folder of this repo.
3. Pin "browser-ai bridge" to the toolbar if you like.

## Run

```sh
go run ./cmd/agent --via-extension --url https://tiki.vn --goal "…"
```

The agent prints:

```
bridge: listening on 127.0.0.1:9223 — in the browser-ai extension popup set port 9223 and token 3f9c…, then click Connect
```

Open the extension popup, paste the port and token (first time only — they are remembered; the
token lives in `~/.browser-ai/token`), tick **Connect automatically** if you want, click
**Connect**. The badge turns green, the agent opens a background tab in your Chrome, works, and
closes the tab when done.

Chrome shows a yellow bar — *"browser-ai bridge started debugging this browser"* — on the tab
being driven. That is Chrome's honest signal that an extension holds a debugger session; it goes
away when the run ends. Clicking **Cancel** on it stops the agent's control immediately.

### Drive the tab you are looking at

```sh
go run ./cmd/agent --via-extension --tab --goal "Add the first result to the cart"
```

Requires **Allow driving the current tab** in the popup. Without `--url` the agent starts from the
page you are on; with `--url` it navigates that tab there first.

## What the extension can and cannot do

- It talks only to `127.0.0.1` and only after you enter the token; one agent at a time; the agent
  rejects any connection whose `Origin` is not `chrome-extension://…`.
- It executes only CDP methods the agent sends (`Runtime.evaluate` with the agent's embedded
  scripts, `Input.*`, `Emulation.*`, `Page.captureScreenshot`, `Target.*` emulated per tab).
  Web pages never reach it; the model's output never becomes a selector or code.
- `chrome.debugger` cannot attach to `chrome://` pages, the Web Store or other extensions.
- If DevTools is open on the driven tab, Chrome refuses a second debugger — close it.
- Chrome may sleep the extension's service worker after 30 s of silence; the agent pings every
  20 s while connected and the extension reconnects with backoff if the agent restarts.

## Automated runs (tests, benchmarks)

Branded Google Chrome ≥ 137 ignores `--load-extension`, so `go test` skips the extension e2e and
`bench rtt --via-extension` fails unless `CHROME_PATH` points at Chromium or Chrome for Testing:

```sh
npx @puppeteer/browsers install chrome@stable --path ~/.cache/cft
export CHROME_PATH="$(find ~/.cache/cft -type f -name 'Google Chrome for Testing' | head -1)"
go test ./internal/extbridge -run TestRealExtension -v
go run ./cmd/bench rtt --headless --via-extension
```

The test launches that Chrome with the extension, configures port/token through the extension's
own popup page over CDP, and then runs the real executor (type, click, close) through the bridge.

## Compared with `--attach`

| | `--attach` (CDP port) | `--via-extension` |
|---|---|---|
| Profile | separate Chrome, or your Chrome after an approval dialog **per connection** | your Chrome, one click per session |
| Setup | `--remote-debugging-port` or chrome://inspect toggle | load unpacked once |
| RTT | ~0.2 ms | ~0.28 ms (one hop through the service worker) |
| Tabs touched | new tab | new tab, or the current one on request |

Next step (M6): a Native Messaging host so Chrome starts the bridge itself and the popup step
disappears.
