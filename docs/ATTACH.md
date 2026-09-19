# Attaching to your real Chrome

`--attach` drives a tab inside the Chrome you already use — same profile, cookies
and logins. The agent opens its own background tab (`Target.createTarget
background:true`) and closes it when done; it never enumerates or touches your tabs.

## macOS, Chrome 144+ (recommended)

1. In Chrome open `chrome://inspect/#remote-debugging`.
2. Turn on **Allow remote debugging**. Chrome writes
   `~/Library/Application Support/Google/Chrome/DevToolsActivePort`.
3. Run:

   ```sh
   go run ./cmd/bench rtt --attach auto          # sanity check, no API key needed
   go run ./cmd/agent --attach auto --url … --goal …
   ```

`auto` reads `DevToolsActivePort` from the default profile dirs (Chrome, Canary, Chromium,
Brave, Edge), verifies the endpoint with `/json/version` so a stale file is skipped, then
falls back to `http://127.0.0.1:9222`.

## Separate Chrome with a fixed port

Chrome 136+ refuses `--remote-debugging-port` on the default profile, so use a dedicated one:

```sh
"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
  --remote-debugging-port=9222 --user-data-dir="$HOME/.browser-ai-profile"
go run ./cmd/agent --attach http://127.0.0.1:9222 --url … --goal …
```

Log in to whatever sites you need in that window first; the profile persists.

Accepted `--attach` values:

| value | meaning |
|---|---|
| `auto` | discover as above |
| `ws://host:port/devtools/browser/<id>` | browser WebSocket URL, used as is |
| `http://host:port` or `host:port` | DevTools HTTP endpoint → `/json/version` |
| `/path/to/user-data-dir` | read that profile's `DevToolsActivePort` |

## Linux / Windows

Same flow. Profile dirs checked by `auto`: `~/.config/google-chrome`, `~/.config/chromium`,
Brave, Edge; on Windows `%LocalAppData%\Google\Chrome\User Data` etc.

## Troubleshooting

- **`no running Chrome with remote debugging found`** — remote debugging is off, or the
  `DevToolsActivePort` file is stale (Chrome was force-quit). Toggle the setting off/on.
- **Fixed port but no `DevToolsActivePort`** — observed with `--headless=new` on Chrome 153;
  pass `http://127.0.0.1:<port>` instead of the profile dir.
- **Handshake fails on a remote host** — Chrome only accepts `Host:` headers that are IPs or
  `localhost`. Tunnel the port (`ssh -L 9222:127.0.0.1:9222 host`) and attach to `127.0.0.1`.
- **Focus / animations** — the agent enables `Emulation.setFocusEmulationEnabled` so menus and
  autocomplete render in the background tab even while you keep working in another tab.
