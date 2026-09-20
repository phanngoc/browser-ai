// bench measures the browser side of the agent with no model calls:
//
//	bench rtt       [--n 1000] [--attach [target]] [--headless]   Runtime.evaluate round-trips, pipe vs ws
//	bench snapshot  --url U [--n 20] [--attach [target]]           snapshot.js cost per observation
//	bench coldstart [--n 5] [--headless]                            launch → getVersion → about:blank ready
//	bench churn     --url U [--n 10] [--interval 300ms]             which freshness inputs change on an untouched page
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/phanngoc/browser-ai/internal/cdp"
	"github.com/phanngoc/browser-ai/internal/chrome"
	"github.com/phanngoc/browser-ai/internal/snapshot"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	n := fs.Int("n", 0, "iterations")
	attach := fs.String("attach", "", "attach to a running Chrome (ws://…, http://host:port, user-data-dir); 'auto' to discover")
	headless := fs.Bool("headless", false, "launch headless")
	url := fs.String("url", "https://en.wikipedia.org/wiki/Main_Page", "page for snapshot bench")
	asJSON := fs.Bool("json", false, "machine-readable output")
	interval := fs.Duration("interval", 300*time.Millisecond, "gap between churn observations")
	fill := fs.String("fill", "", "churn: type TEXT into the field whose label contains LABEL first, as LABEL=TEXT")
	check := fs.String("check", "", "churn: after --fill, run the executor's freshness check for the click whose label contains this, once per interval")
	_ = fs.Parse(os.Args[2:])

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var err error
	switch os.Args[1] {
	case "rtt":
		err = benchRTT(ctx, or(*n, 1000), *attach, *headless, *asJSON)
	case "snapshot":
		err = benchSnapshot(ctx, or(*n, 20), *url, *attach, *headless, *asJSON)
	case "coldstart":
		err = benchColdstart(ctx, or(*n, 5), *headless, *asJSON)
	case "churn":
		err = benchChurn(ctx, or(*n, 10), *interval, *url, *attach, *headless, *fill, *check)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: bench rtt|snapshot|coldstart|churn [flags]")
	os.Exit(2)
}

func or(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}

// endpoint is a connection under test.
type endpoint struct {
	name    string
	conn    *cdp.Conn
	version string
	close   func()
}

func launch(ctx context.Context, useWS, headless bool) (*endpoint, error) {
	b, err := chrome.Launch(ctx, chrome.Options{UseWS: useWS, Headless: headless})
	if err != nil {
		return nil, err
	}
	name := "pipe (launched)"
	if useWS {
		name = "ws (launched, port=0)"
	}
	return &endpoint{name: name, conn: b.Conn, version: b.Version.Product, close: func() { b.Close() }}, nil
}

func attachTo(ctx context.Context, target string) (*endpoint, error) {
	if target == "auto" {
		target = ""
	}
	a, err := chrome.Attach(ctx, target)
	if err != nil {
		return nil, err
	}
	return &endpoint{name: "ws (attached: " + a.Source + ")", conn: a.Conn, version: a.Version.Product, close: func() { a.Close() }}, nil
}

type stats struct {
	N                       int
	Min, P50, P95, P99, Max time.Duration
	Mean                    time.Duration
	Total                   time.Duration
}

func summarize(d []time.Duration) stats {
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	var sum time.Duration
	for _, x := range d {
		sum += x
	}
	q := func(p float64) time.Duration { return d[min(len(d)-1, int(float64(len(d))*p))] }
	return stats{N: len(d), Min: d[0], P50: q(0.5), P95: q(0.95), P99: q(0.99), Max: d[len(d)-1], Mean: sum / time.Duration(len(d)), Total: sum}
}

func (s stats) row(name string) string {
	return fmt.Sprintf("%-32s n=%-5d min %-9s p50 %-9s p95 %-9s p99 %-9s max %-9s", name, s.N, s.Min, s.P50, s.P95, s.P99, s.Max)
}

func benchRTT(ctx context.Context, n int, attach string, headless, asJSON bool) error {
	var eps []*endpoint
	if attach != "" {
		e, err := attachTo(ctx, attach)
		if err != nil {
			return err
		}
		eps = append(eps, e)
	} else {
		for _, useWS := range []bool{false, true} {
			e, err := launch(ctx, useWS, headless)
			if err != nil {
				return err
			}
			eps = append(eps, e)
		}
	}
	defer func() {
		for _, e := range eps {
			e.close()
		}
	}()
	out := map[string]any{}
	for _, e := range eps {
		s, err := e.conn.NewPage(ctx, "about:blank", true)
		if err != nil {
			return err
		}
		for i := 0; i < 50; i++ { // warm-up
			if _, err := s.Evaluate(ctx, "1", false); err != nil {
				return err
			}
		}
		durs := make([]time.Duration, 0, n)
		for i := 0; i < n; i++ {
			t := time.Now()
			if _, err := s.Evaluate(ctx, "1", false); err != nil {
				return err
			}
			durs = append(durs, time.Since(t))
		}
		st := summarize(durs)
		_ = s.Close(ctx)
		if asJSON {
			out[e.name] = st
		} else {
			fmt.Println(st.row(e.name), " ", e.version)
		}
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	return nil
}

func benchSnapshot(ctx context.Context, n int, url, attach string, headless, asJSON bool) error {
	var e *endpoint
	var err error
	if attach != "" {
		e, err = attachTo(ctx, attach)
	} else {
		e, err = launch(ctx, false, headless)
	}
	if err != nil {
		return err
	}
	defer e.close()
	s, err := e.conn.NewPage(ctx, "about:blank", true)
	if err != nil {
		return err
	}
	defer s.Close(context.Background())
	if _, err := s.Call(ctx, "Emulation.setDeviceMetricsOverride", map[string]any{"width": 1120, "height": 780, "deviceScaleFactor": 1, "mobile": false}); err != nil {
		return err
	}
	_, _ = s.Call(ctx, "Emulation.setFocusEmulationEnabled", map[string]any{"enabled": true})
	tNav := time.Now()
	if err := s.Navigate(ctx, url); err != nil {
		return err
	}
	lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.WaitReady(lctx, 20*time.Millisecond); err != nil {
		return fmt.Errorf("page load: %w", err)
	}
	loadMS := time.Since(tNav)
	var evalD, decodeD []time.Duration
	var bytes, actions, textLen int
	var last *snapshot.Page
	for i := 0; i < n; i++ {
		t := time.Now()
		raw, err := s.Evaluate(ctx, snapshot.Expr, false)
		if err != nil {
			return err
		}
		evalD = append(evalD, time.Since(t))
		t = time.Now()
		p, err := snapshot.Decode(raw)
		if err != nil {
			return err
		}
		decodeD = append(decodeD, time.Since(t))
		bytes, actions, textLen, last = len(raw), len(p.Actions), len(p.Text), p
	}
	ev, dc := summarize(evalD), summarize(decodeD)
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"endpoint": e.name, "url": url, "title": last.Title, "load_ms": loadMS.Milliseconds(),
			"evaluate": ev, "decode": dc, "bytes": bytes, "actions": actions, "text_len": textLen,
		})
	}
	fmt.Printf("%s  %s\n", e.name, e.version)
	fmt.Printf("url: %s\ntitle: %s\nload → readyState complete: %s\n", url, last.Title, loadMS)
	fmt.Printf("payload: %d bytes, %d actions, %d chars visible text\n", bytes, actions, textLen)
	fmt.Println(ev.row("snapshot.js evaluate (CDP RTT)"))
	fmt.Println(dc.row("decode + fingerprint (Go)"))
	fmt.Println(strings.Repeat("-", 40))
	for i, a := range last.Actions {
		if i >= 12 {
			fmt.Printf("  … %d more\n", len(last.Actions)-12)
			break
		}
		fmt.Printf("  [%s] %-8s %-9s %s\n", a.ID, a.Kind, a.Role, trunc(a.Label, 60))
	}
	return nil
}

func benchColdstart(ctx context.Context, n int, headless, asJSON bool) error {
	type run struct {
		Startup, Page, Total time.Duration
	}
	var runs []run
	for i := 0; i < n; i++ {
		t0 := time.Now()
		b, err := chrome.Launch(ctx, chrome.Options{Headless: headless})
		if err != nil {
			return err
		}
		t1 := time.Now()
		s, err := b.Conn.NewPage(ctx, "about:blank", true)
		if err == nil {
			err = s.WaitReady(ctx, 5*time.Millisecond)
		}
		if err != nil {
			b.Close()
			return err
		}
		r := run{Startup: b.Startup, Page: time.Since(t1), Total: time.Since(t0)}
		runs = append(runs, r)
		if !asJSON {
			fmt.Printf("run %d: exec→getVersion %-9s newPage+ready %-9s total %-9s  %s\n", i+1, r.Startup, r.Page, r.Total, b.Version.Product)
		}
		b.Close()
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(runs)
	}
	return nil
}

func trunc(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}
