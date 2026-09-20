package chrome

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/phanngoc/browser-ai/internal/cdp"
	"github.com/phanngoc/browser-ai/internal/ws"
)

// Attached is a connection to a Chrome we do not own.
type Attached struct {
	Conn    *cdp.Conn
	WSURL   string
	Source  string // how the endpoint was found
	Version Version
}

// ErrNoDebugPort is returned when no running Chrome exposes DevTools.
var ErrNoDebugPort = errors.New(`chrome: no running Chrome with remote debugging found.

Enable it in your real Chrome (Chrome 144+): open chrome://inspect/#remote-debugging
and turn on "Allow remote debugging", then rerun with --attach.

Or start a separate Chrome with a debugging port (Chrome 136+ refuses the default profile):
  "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
    --remote-debugging-port=9222 --user-data-dir="$HOME/.browser-ai-profile"
and rerun with --attach http://127.0.0.1:9222`)

// Attach connects to a running Chrome. target may be:
//   - ""                       auto-discover via DevToolsActivePort, then http://127.0.0.1:9222
//   - "ws://…/devtools/browser/…"  a browser WebSocket URL
//   - "http://host:port" / "host:port"  a DevTools HTTP endpoint (/json/version)
//   - a user-data-dir path   read its DevToolsActivePort
func Attach(ctx context.Context, target string) (*Attached, error) {
	wsURL, source, err := Discover(ctx, target)
	if err != nil {
		return nil, err
	}
	c, err := ws.Dial(ctx, wsURL)
	if err != nil {
		return nil, fmt.Errorf("chrome: dial %s: %w", wsURL, err)
	}
	a := &Attached{Conn: cdp.NewConn(c), WSURL: wsURL, Source: source}
	vctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, err := a.Conn.Call(vctx, "", "Browser.getVersion", nil)
	if err != nil {
		a.Conn.Close()
		return nil, fmt.Errorf("chrome: getVersion: %w", err)
	}
	_ = json.Unmarshal(res, &a.Version)
	return a, nil
}

// Close drops the connection. The user's Chrome keeps running.
func (a *Attached) Close() error { return a.Conn.Close() }

// Discover resolves target to a browser WebSocket URL.
func Discover(ctx context.Context, target string) (wsURL, source string, err error) {
	switch {
	case strings.HasPrefix(target, "ws://") || strings.HasPrefix(target, "wss://"):
		return target, "explicit ws url", nil
	case strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://"):
		u, err := versionEndpoint(ctx, target)
		return u, "/json/version at " + target, err
	case target != "":
		if st, err := os.Stat(target); err == nil && st.IsDir() {
			u, err := ReadActivePort(target)
			return u, "DevToolsActivePort in " + target, err
		}
		u, err := versionEndpoint(ctx, "http://"+target)
		return u, "/json/version at " + target, err
	}
	for _, dir := range profileDirs() {
		if u, err := ReadActivePort(dir); err == nil {
			if u, err := probe(ctx, u); err == nil {
				return u, "DevToolsActivePort in " + dir, nil
			}
		}
	}
	if u, err := versionEndpoint(ctx, "http://127.0.0.1:9222"); err == nil {
		return u, "/json/version at 127.0.0.1:9222", nil
	}
	return "", "", ErrNoDebugPort
}

// probe checks a discovered URL is live (the file may be stale). It asks the
// HTTP side for /json/version first; Chrome's built-in "Allow remote
// debugging" (chrome://inspect) serves only the WebSocket, so it falls back
// to a real handshake on the discovered URL.
func probe(ctx context.Context, wsURL string) (string, error) {
	rest := strings.TrimPrefix(wsURL, "ws://")
	host := rest
	if i := strings.Index(rest, "/"); i >= 0 {
		host = rest[:i]
	}
	if u, err := versionEndpoint(ctx, "http://"+host); err == nil {
		return u, nil
	}
	dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	c, err := ws.Dial(dctx, wsURL)
	if err != nil {
		return "", err
	}
	c.Close()
	return wsURL, nil
}

func versionEndpoint(ctx context.Context, base string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/json/version", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("chrome: %s: HTTP %d", req.URL, resp.StatusCode)
	}
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	if v.WebSocketDebuggerURL == "" {
		return "", errors.New("chrome: /json/version has no webSocketDebuggerUrl")
	}
	return v.WebSocketDebuggerURL, nil
}

func profileDirs() []string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		base := filepath.Join(home, "Library", "Application Support")
		return []string{
			filepath.Join(base, "Google", "Chrome"),
			filepath.Join(base, "Google", "Chrome Canary"),
			filepath.Join(base, "Chromium"),
			filepath.Join(base, "BraveSoftware", "Brave-Browser"),
			filepath.Join(base, "Microsoft Edge"),
		}
	case "linux":
		cfg := filepath.Join(home, ".config")
		return []string{
			filepath.Join(cfg, "google-chrome"), filepath.Join(cfg, "chromium"),
			filepath.Join(cfg, "BraveSoftware", "Brave-Browser"), filepath.Join(cfg, "microsoft-edge"),
		}
	case "windows":
		local := os.Getenv("LocalAppData")
		return []string{
			filepath.Join(local, "Google", "Chrome", "User Data"),
			filepath.Join(local, "Chromium", "User Data"),
			filepath.Join(local, "Microsoft", "Edge", "User Data"),
		}
	}
	return nil
}
