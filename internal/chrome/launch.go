// Package chrome launches Chrome over --remote-debugging-pipe, or attaches to
// a running Chrome over WebSocket, and hands back a cdp.Conn.
package chrome

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/phanngoc/browser-ai/internal/cdp"
	"github.com/phanngoc/browser-ai/internal/ws"
)

// Options controls Launch.
type Options struct {
	Path        string   // binary; empty → FindBinary
	Headless    bool     // --headless=new
	UseWS       bool     // --remote-debugging-port=0 + WebSocket instead of the pipe (for benchmarks)
	UserDataDir string   // empty → fresh temp profile removed on Close
	Args        []string // extra flags
	Width       int
	Height      int
	// LoadExtension loads an unpacked extension directory (and disables all
	// others), for tests and benchmarks of the extension bridge.
	LoadExtension string
}

// Browser is a Chrome we own.
type Browser struct {
	Conn      *cdp.Conn
	Transport string // "pipe" or "ws"
	WSURL     string
	Version   Version
	// Startup is exec → first Browser.getVersion reply.
	Startup time.Duration

	cmd     *exec.Cmd
	dir     string
	ownsDir bool
	waitErr chan error
	stderr  *tailBuffer
}

// tailBuffer keeps the last few KB of Chrome's stderr for error messages.
type tailBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if t.buf.Len() > 4096 {
		b := t.buf.Bytes()
		t.buf = *bytes.NewBuffer(append([]byte(nil), b[len(b)-4096:]...))
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(t.buf.String())
}

// Version is the Browser.getVersion result.
type Version struct {
	ProtocolVersion string `json:"protocolVersion"`
	Product         string `json:"product"`
	Revision        string `json:"revision"`
	UserAgent       string `json:"userAgent"`
	JSVersion       string `json:"jsVersion"`
}

var baseArgs = []string{
	"--no-first-run",
	"--no-default-browser-check",
	"--disable-background-timer-throttling",
	"--disable-renderer-backgrounding",
	"--disable-backgrounding-occluded-windows",
	"--disable-features=Translate,MediaRouter,OptimizationHints",
	"--disable-sync",
	"--metrics-recording-only",
	"--password-store=basic",
	"--use-mock-keychain",
}

// Launch starts Chrome and connects. The returned Browser must be Closed.
func Launch(ctx context.Context, opts Options) (*Browser, error) {
	bin := opts.Path
	if bin == "" {
		var err error
		if bin, err = FindBinary(); err != nil {
			return nil, err
		}
	}
	b := &Browser{waitErr: make(chan error, 1)}
	b.dir = opts.UserDataDir
	if b.dir == "" {
		d, err := os.MkdirTemp("", "browser-ai-chrome-")
		if err != nil {
			return nil, err
		}
		b.dir, b.ownsDir = d, true
	}
	w, h := opts.Width, opts.Height
	if w == 0 {
		w = 1120
	}
	if h == 0 {
		h = 780
	}
	args := append([]string{}, baseArgs...)
	args = append(args, "--user-data-dir="+b.dir, fmt.Sprintf("--window-size=%d,%d", w, h))
	if opts.Headless {
		args = append(args, "--headless=new")
	}
	// Chrome for Testing has no SUID sandbox helper; on CI runners with
	// restricted user namespaces it aborts at startup without this.
	if os.Getenv("BROWSER_AI_NO_SANDBOX") != "" || os.Getenv("CI") != "" {
		args = append(args, "--no-sandbox")
	}
	if opts.LoadExtension != "" {
		abs, err := filepath.Abs(opts.LoadExtension)
		if err != nil {
			b.cleanupDir()
			return nil, err
		}
		args = append(args, "--load-extension="+abs, "--disable-extensions-except="+abs)
	}
	if opts.UseWS {
		args = append(args, "--remote-debugging-port=0")
	} else {
		args = append(args, "--remote-debugging-pipe")
	}
	args = append(args, opts.Args...)
	args = append(args, "about:blank")

	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "GOOGLE_API_KEY=no", "GOOGLE_DEFAULT_CLIENT_ID=no", "GOOGLE_DEFAULT_CLIENT_SECRET=no")
	// Chrome's stderr goes through our own pipe: with a plain io.Writer,
	// exec.Wait would block until every renderer (which inherits the fd)
	// exits. We read the tail ourselves and never wait on it.
	b.stderr = &tailBuffer{}
	if errR, errW, err := os.Pipe(); err == nil {
		cmd.Stderr = errW
		go func() {
			buf := make([]byte, 4096)
			for {
				n, err := errR.Read(buf)
				if n > 0 {
					_, _ = b.stderr.Write(buf[:n])
				}
				if err != nil {
					errR.Close()
					return
				}
			}
		}()
		defer errW.Close() // parent copy; Chrome keeps its own
	}
	b.cmd = cmd

	var transport cdp.Transport
	var toChromeR, fromChromeW *os.File
	if !opts.UseWS {
		// fd 3: Chrome reads from it; fd 4: Chrome writes to it.
		var toChromeW, fromChromeR *os.File
		var err error
		toChromeR, toChromeW, err = os.Pipe()
		if err != nil {
			b.cleanupDir()
			return nil, err
		}
		fromChromeR, fromChromeW, err = os.Pipe()
		if err != nil {
			b.cleanupDir()
			return nil, err
		}
		cmd.ExtraFiles = []*os.File{toChromeR, fromChromeW}
		transport = newPipeTransport(fromChromeR, toChromeW)
		b.Transport = "pipe"
	}

	started := time.Now()
	if err := cmd.Start(); err != nil {
		b.cleanupDir()
		return nil, fmt.Errorf("chrome: start: %w", err)
	}
	go func() { b.waitErr <- cmd.Wait() }()
	if toChromeR != nil {
		toChromeR.Close()
		fromChromeW.Close()
	}

	if opts.UseWS {
		wsURL, err := waitActivePort(ctx, b.dir, b.waitErr)
		if err != nil {
			b.kill()
			return nil, err
		}
		c, err := ws.Dial(ctx, wsURL)
		if err != nil {
			b.kill()
			return nil, fmt.Errorf("chrome: dial %s: %w", wsURL, err)
		}
		transport = c
		b.Transport, b.WSURL = "ws", wsURL
	}
	b.Conn = cdp.NewConn(transport)

	vctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	res, err := b.Conn.Call(vctx, "", "Browser.getVersion", nil)
	if err != nil {
		tail := b.stderr.String()
		b.Close()
		if tail != "" {
			return nil, fmt.Errorf("chrome: getVersion: %w\nchrome stderr:\n%s", err, tail)
		}
		return nil, fmt.Errorf("chrome: getVersion: %w", err)
	}
	b.Startup = time.Since(started)
	_ = json.Unmarshal(res, &b.Version)
	return b, nil
}

// waitActivePort polls <dir>/DevToolsActivePort until Chrome writes it.
func waitActivePort(ctx context.Context, dir string, exited <-chan error) (string, error) {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if u, err := ReadActivePort(dir); err == nil {
			return u, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case err := <-exited:
			return "", fmt.Errorf("chrome: exited before opening a debugging port: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	return "", errors.New("chrome: DevToolsActivePort not written within 15s")
}

// ReadActivePort parses <dir>/DevToolsActivePort into a browser WebSocket URL.
func ReadActivePort(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "DevToolsActivePort"))
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 2 {
		return "", errors.New("chrome: malformed DevToolsActivePort")
	}
	return "ws://127.0.0.1:" + strings.TrimSpace(lines[0]) + strings.TrimSpace(lines[1]), nil
}

// Close asks Chrome to quit, kills it if it lingers, and removes a temp profile.
func (b *Browser) Close() error {
	if b.Conn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, _ = b.Conn.Call(ctx, "", "Browser.close", nil)
		cancel()
		b.Conn.Close()
	}
	select {
	case <-b.waitErr:
	case <-time.After(2 * time.Second):
		b.kill()
	}
	b.cleanupDir()
	return nil
}

func (b *Browser) kill() {
	if b.cmd != nil && b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
		select {
		case <-b.waitErr:
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *Browser) cleanupDir() {
	if b.ownsDir && b.dir != "" {
		_ = os.RemoveAll(b.dir)
	}
}
