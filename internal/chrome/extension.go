package chrome

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/phanngoc/browser-ai/internal/cdp"
)

// ErrExtensionNotLoaded means Chrome ignored --load-extension. Branded Google
// Chrome (137+) no longer honours it; use Chromium or Chrome for Testing
// (CHROME_PATH) for automated runs. Users load the extension unpacked via
// chrome://extensions, which works in every build.
var ErrExtensionNotLoaded = errors.New("chrome: bridge extension was not loaded (branded Google Chrome ignores --load-extension since 137; point CHROME_PATH at Chromium or Chrome for Testing)")

// ConfigureExtension finds the browser-ai bridge service worker in a Chrome we
// launched (Options.LoadExtension) and, over CDP, stores the agent's port and
// token and tells it to connect. This is how tests and benchmarks avoid the
// popup; a user does the same by hand once.
func ConfigureExtension(ctx context.Context, conn *cdp.Conn, port int, token string) error {
	deadline := time.Now().Add(15 * time.Second)
	var swTarget, extID string
	var seen []string
	for time.Now().Before(deadline) && swTarget == "" {
		res, err := conn.Call(ctx, "", "Target.getTargets", nil)
		if err != nil {
			return err
		}
		var out struct {
			TargetInfos []struct {
				TargetID string `json:"targetId"`
				Type     string `json:"type"`
				URL      string `json:"url"`
			} `json:"targetInfos"`
		}
		_ = json.Unmarshal(res, &out)
		seen = seen[:0]
		for _, t := range out.TargetInfos {
			seen = append(seen, t.Type+" "+t.URL)
			if t.Type == "service_worker" && strings.HasPrefix(t.URL, "chrome-extension://") && strings.HasSuffix(t.URL, "/background.js") {
				swTarget = t.TargetID
				extID = strings.TrimSuffix(strings.TrimPrefix(t.URL, "chrome-extension://"), "/background.js")
			}
		}
		if swTarget == "" {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if swTarget == "" {
		return fmt.Errorf("%w; targets seen: %s", ErrExtensionNotLoaded, strings.Join(seen, " | "))
	}
	// Evaluate inside an extension page (the popup), which has the chrome.*
	// APIs; the worker's context is not reliably scriptable right after attach.
	page, err := conn.NewPage(ctx, "chrome-extension://"+extID+"/popup.html", true)
	if err != nil {
		return err
	}
	defer page.Close(context.Background())
	lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := page.WaitReady(lctx, 20*time.Millisecond); err != nil {
		return fmt.Errorf("chrome: extension popup did not load: %w", err)
	}
	expr := fmt.Sprintf(`chrome.storage.local.set({port:%d, token:%q, autoconnect:true, allowCurrentTab:true})
	  .then(() => new Promise(r => chrome.runtime.sendMessage({type:"connect"}, r))).then(s => JSON.stringify(s))`, port, token)
	if _, err := page.Evaluate(ctx, expr, true); err != nil {
		return fmt.Errorf("chrome: configure extension: %w", err)
	}
	return nil
}
