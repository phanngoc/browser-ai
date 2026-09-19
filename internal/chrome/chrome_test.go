package chrome

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/phanngoc/browser-ai/internal/snapshot"
)

func requireChrome(t *testing.T) {
	t.Helper()
	if _, err := FindBinary(); err != nil {
		t.Skip("no Chrome:", err)
	}
	if os.Getenv("BROWSER_AI_SKIP_CHROME") != "" {
		t.Skip("BROWSER_AI_SKIP_CHROME set")
	}
}

func TestLaunchPipeAndWS(t *testing.T) {
	requireChrome(t)
	for _, useWS := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		b, err := Launch(ctx, Options{Headless: true, UseWS: useWS})
		if err != nil {
			cancel()
			t.Fatalf("useWS=%v: %v", useWS, err)
		}
		if b.Version.Product == "" {
			t.Errorf("useWS=%v: empty product", useWS)
		}
		s, err := b.Conn.NewPage(ctx, "about:blank", true)
		if err != nil {
			t.Fatal(err)
		}
		v, err := s.Evaluate(ctx, "1+1", false)
		if err != nil || string(v) != "2" {
			t.Fatalf("useWS=%v: evaluate %s %v", useWS, v, err)
		}
		if err := s.Navigate(ctx, "data:text/html,<button id=b>Go</button><input placeholder=Name>"); err != nil {
			t.Fatal(err)
		}
		if err := s.WaitReady(ctx, 10*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		raw, err := s.Evaluate(ctx, snapshot.Expr, false)
		if err != nil {
			t.Fatal(err)
		}
		p, err := snapshot.Decode(raw)
		if err != nil {
			t.Fatal(err)
		}
		var kinds []string
		for _, a := range p.Actions {
			kinds = append(kinds, a.Kind+":"+a.Label)
		}
		want := map[string]bool{"click:Go": false, "fill:Name": false, "wait:Wait for the page to update": false}
		for _, k := range kinds {
			if _, ok := want[k]; ok {
				want[k] = true
			}
		}
		for k, seen := range want {
			if !seen {
				t.Errorf("useWS=%v: missing action %q in %v", useWS, k, kinds)
			}
		}
		if p.Fingerprint == "" {
			t.Error("no fingerprint")
		}
		_ = s.Close(ctx)
		dir := b.dir
		if err := b.Close(); err != nil {
			t.Error(err)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("temp profile not removed: %s", dir)
		}
		cancel()
	}
}

func TestAttachViaProfileDir(t *testing.T) {
	requireChrome(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	b, err := Launch(ctx, Options{Headless: true, UseWS: true})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	// Attach a second, independent connection to the same Chrome three ways.
	for _, target := range []string{b.dir, b.WSURL, "http://" + hostOf(b.WSURL)} {
		a, err := Attach(ctx, target)
		if err != nil {
			t.Fatalf("attach %q: %v", target, err)
		}
		s, err := a.Conn.NewPage(ctx, "about:blank", true)
		if err != nil {
			t.Fatal(err)
		}
		if v, err := s.Evaluate(ctx, "40+2", false); err != nil || string(v) != "42" {
			t.Fatalf("attach %q: evaluate %s %v", target, v, err)
		}
		_ = s.Close(ctx)
		a.Close()
	}
}

func TestReadActivePort(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "DevToolsActivePort"), []byte("9222\n/devtools/browser/abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	u, err := ReadActivePort(dir)
	if err != nil || u != "ws://127.0.0.1:9222/devtools/browser/abc" {
		t.Fatalf("got %q %v", u, err)
	}
}

func hostOf(wsURL string) string {
	s := wsURL[len("ws://"):]
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return s[:i]
		}
	}
	return s
}
