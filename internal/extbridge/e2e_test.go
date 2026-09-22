package extbridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/phanngoc/browser-ai/internal/browser"
	"github.com/phanngoc/browser-ai/internal/cdp"
	"github.com/phanngoc/browser-ai/internal/chrome"
)

// TestRealExtension loads the unpacked extension into a headless Chrome,
// configures it over the pipe, and then drives a page through the bridge
// exactly as cmd/agent --via-extension does.
func TestRealExtension(t *testing.T) {
	if _, err := chrome.FindBinary(); err != nil || os.Getenv("BROWSER_AI_SKIP_CHROME") != "" {
		t.Skip("no Chrome")
	}
	html, err := os.ReadFile("../browser/testdata/fixture.html")
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(html)
	}))
	defer web.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	srv := listen(t)
	_, portStr, _ := strings.Cut(srv.Addr, ":")
	port, _ := strconv.Atoi(portStr)

	ch, err := chrome.Launch(ctx, chrome.Options{Headless: true, LoadExtension: "../../extension"})
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	if err := chrome.ConfigureExtension(ctx, ch.Conn, port, srv.Token); err != nil {
		if errors.Is(err, chrome.ErrExtensionNotLoaded) {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	client, err := srv.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(client.Origin, "chrome-extension://") || client.Hello.Version == "" {
		t.Fatalf("hello %+v origin %q", client.Hello, client.Origin)
	}
	conn := cdp.NewConn(client)
	defer conn.Close()

	// Browser.getVersion is answered by the extension itself.
	res, err := conn.Call(ctx, "", "Browser.getVersion", nil)
	if err != nil || !strings.Contains(string(res), "Chrome") {
		t.Fatalf("getVersion %s %v", res, err)
	}

	// Whole executor over the bridge.
	br, err := browser.New(ctx, conn, web.URL, browser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	p, tm, err := br.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "Fixture" || len(p.Actions) < 10 {
		t.Fatalf("snapshot over bridge: %q %d actions", p.Title, len(p.Actions))
	}
	t.Logf("snapshot via extension: %s", tm.Snapshot)
	var name, submit *browser.Point
	for _, a := range p.Actions {
		if a.Kind == "fill" && a.Label == "Name" {
			if _, err := br.Act(ctx, a, p, "Zurich"); err != nil {
				t.Fatal(err)
			}
			name = &browser.Point{}
		}
	}
	p, _, err = br.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range p.Actions {
		if a.Kind == "click" && a.Label == "Submit" {
			if _, err := br.Act(ctx, a, p, ""); err != nil {
				t.Fatal(err)
			}
			submit = &browser.Point{}
		}
	}
	if name == nil || submit == nil {
		t.Fatal("actions not found")
	}
	v, err := br.Session().Evaluate(ctx, "document.getElementById('name').value+'|'+String(window.__submitted)", false)
	if err != nil || string(v) != `"Zurich|true"` {
		t.Fatalf("state %s %v", v, err)
	}

	// RTT through the extension hop.
	for i := 0; i < 20; i++ {
		_, _ = br.Session().Evaluate(ctx, "1", false)
	}
	start := time.Now()
	for i := 0; i < 100; i++ {
		if _, err := br.Session().Evaluate(ctx, "1", false); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("Runtime.evaluate RTT via extension: %s avg", time.Since(start)/100)

	// Closing the target detaches and closes the tab; the extension reports it.
	sub := conn.Subscribe("", "Target.detachedFromTarget", 4)
	if err := br.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-sub.C:
		var d struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(ev.Params, &d)
		t.Logf("detached: %s", d.Reason)
	case <-time.After(3 * time.Second):
		// Chrome may fire onDetach before onRemoved or not at all for owner-initiated detach; not fatal.
		t.Log("no detach event (owner-initiated detach)")
	}
}
