// agent runs one natural-language goal against a page and prints where the
// time went.
//
//	agent --url https://en.wikipedia.org/wiki/Main_Page --goal "Open the article about Go (programming language)."
//	agent --attach auto --url … --goal …          # use your running Chrome (chrome://inspect → Allow remote debugging)
//	agent --headless --trace run.json --screenshots shots/ …
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/phanngoc/browser-ai/internal/agent"
	"github.com/phanngoc/browser-ai/internal/browser"
	"github.com/phanngoc/browser-ai/internal/cdp"
	"github.com/phanngoc/browser-ai/internal/chrome"
	"github.com/phanngoc/browser-ai/internal/extbridge"
	"github.com/phanngoc/browser-ai/internal/jev"
	"github.com/phanngoc/browser-ai/internal/llmchooser"
	"github.com/phanngoc/browser-ai/internal/textgen"
)

func main() {
	url := flag.String("url", "", "start page (required)")
	goal := flag.String("goal", "", "natural-language goal (required)")
	headless := flag.Bool("headless", false, "launch Chrome headless")
	attach := flag.String("attach", "", "attach to a running Chrome: 'auto', ws://…, http://host:port, or a user-data-dir")
	viaExt := flag.Bool("via-extension", false, "drive your real Chrome through the browser-ai bridge extension (extension/)")
	extPort := flag.Int("ext-port", 9223, "loopback port the extension connects to (with --via-extension)")
	tabCurrent := flag.Bool("tab", false, "with --via-extension: drive the tab you are on instead of opening a new one (enable it in the extension popup)")
	keepOpen := flag.Bool("keep-open", false, "leave the tab/browser open after the run")
	trace := flag.String("trace", "", "write a JSON trace (steps, decisions, timings, requests) to this file")
	shots := flag.String("screenshots", "", "save a JPEG per step into this directory")
	maxSteps := flag.Int("max-steps", 60, "action budget")
	envFile := flag.String("env", ".env", "env file to load if present")
	quiet := flag.Bool("quiet", false, "print only the final table")
	chooserFlag := flag.String("chooser", "", "jev (TypeSafe) or llm (general LLM via TEXT_MODEL_* credentials); default: jev if TYPESAFE_API_KEY is set, else llm")
	chooserModel := flag.String("chooser-model", "", "model for --chooser llm (default google/gemini-2.5-flash-lite; env CHOOSER_MODEL)")
	chooserBase := flag.String("chooser-base-url", "", "OpenAI-compatible base URL for --chooser llm (env CHOOSER_BASE_URL, else TEXT_MODEL_BASE_URL)")
	flag.Parse()
	if (*url == "" && !*tabCurrent) || *goal == "" {
		flag.Usage()
		os.Exit(2)
	}
	loadEnv(*envFile)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var tc *textgen.Client
	if key := os.Getenv("TEXT_MODEL_API_KEY"); key != "" {
		tc = textgen.New(key, os.Getenv("TEXT_MODEL_BASE_URL"), os.Getenv("TEXT_MODEL"), os.Getenv("TEXT_MODEL_REASONING"))
	}
	which := *chooserFlag
	if which == "" {
		which = "llm"
		if os.Getenv("TYPESAFE_API_KEY") != "" {
			which = "jev"
		}
	}
	var chooser agent.Chooser
	var warmChooser func(context.Context) time.Duration
	var chooserName string
	switch which {
	case "jev":
		jc := jev.New(os.Getenv("TYPESAFE_API_KEY"), os.Getenv("TYPESAFE_MODEL"))
		if ep := os.Getenv("TYPESAFE_ENDPOINT"); ep != "" {
			jc.Endpoint = ep
		}
		chooser, warmChooser, chooserName = jc, jc.Warm, "jev "+jc.Model
	case "llm":
		model := *chooserModel
		if model == "" {
			model = os.Getenv("CHOOSER_MODEL")
		}
		base := *chooserBase
		if base == "" {
			base = firstEnv("CHOOSER_BASE_URL", "TEXT_MODEL_BASE_URL")
		}
		lc := llmchooser.New(firstEnv("CHOOSER_API_KEY", "TEXT_MODEL_API_KEY"), base, model)
		lc.Reasoning = os.Getenv("CHOOSER_REASONING")
		chooser, warmChooser, chooserName = lc, lc.Warm, "llm "+lc.Model
		if lc.Reasoning != "" {
			chooserName += " (reasoning " + lc.Reasoning + ")"
		}
	default:
		fatal(fmt.Errorf("unknown --chooser %q", which))
	}

	// Warm the model connections while Chrome starts.
	var wg sync.WaitGroup
	var warmJev, warmText time.Duration
	wg.Add(1)
	go func() { defer wg.Done(); warmJev = warmChooser(ctx) }()
	if tc != nil {
		wg.Add(1)
		go func() { defer wg.Done(); warmText = tc.Warm(ctx) }()
	}

	t0 := time.Now()
	var err error
	var conn *cdp.Conn
	var mode string
	var closeBrowser func()
	var br *browser.Browser
	if *viaExt {
		conn, mode, closeBrowser, err = connectExtension(ctx, *extPort, *quiet)
		if err != nil {
			fatal(err)
		}
		if !*keepOpen {
			defer closeBrowser()
		}
		if *tabCurrent {
			res, err := conn.Call(ctx, "", "Ext.currentTab", nil)
			if err != nil {
				fatal(err)
			}
			var t struct {
				TargetID string `json:"targetId"`
			}
			_ = json.Unmarshal(res, &t)
			sess, err := conn.AttachExisting(ctx, t.TargetID)
			if err != nil {
				fatal(err)
			}
			// Stay on the user's current document unless a start URL was given explicitly.
			startURL := ""
			if flagSet("url") {
				startURL = *url
			}
			br, err = browser.NewOnSession(ctx, sess, startURL, browser.Options{})
			if err != nil {
				fatal(err)
			}
			mode += ", current tab"
		}
	} else {
		conn, mode, closeBrowser, err = connect(ctx, *attach, *headless)
		if err != nil {
			fatal(err)
		}
		if !*keepOpen {
			defer closeBrowser()
		}
	}
	if br == nil {
		br, err = browser.New(ctx, conn, *url, browser.Options{})
		if err != nil {
			fatal(err)
		}
	}
	if !*keepOpen {
		defer br.Close(context.Background())
	}
	browserReady := time.Since(t0)
	wg.Wait()
	if !*quiet {
		fmt.Printf("browser: %s ready in %s · chooser %s warm %s", mode, browserReady.Round(time.Millisecond), chooserName, warmJev.Round(time.Millisecond))
		if tc != nil {
			fmt.Printf(" · text warm %s", warmText.Round(time.Millisecond))
		}
		fmt.Println()
	}

	var textGen agent.TextGen
	if tc != nil {
		textGen = tc
	}
	out := bufio.NewWriter(os.Stdout)
	a := agent.New(br, chooser, textGen, agent.Options{
		Goal: *goal, MaxSteps: *maxSteps, Screenshots: *shots != "", KeepRequest: *trace != "",
		OnStep: func(s agent.Step) {
			if !*quiet {
				fmt.Fprintln(out, stepLine(s))
				out.Flush()
			}
			if *shots != "" && s.Screenshot != nil {
				_ = os.MkdirAll(*shots, 0o755)
				_ = os.WriteFile(filepath.Join(*shots, fmt.Sprintf("%06d.jpg", s.Elapsed.Milliseconds())), s.Screenshot, 0o644)
			}
		},
	})
	res := a.Run(ctx)
	printTable(res)
	if *trace != "" {
		if err := writeTrace(*trace, res); err != nil {
			fmt.Fprintln(os.Stderr, "trace:", err)
		} else if !*quiet {
			fmt.Println("trace:", *trace)
		}
	}
	switch res.Status {
	case "done":
		os.Exit(0)
	case "blocked":
		os.Exit(2)
	default:
		os.Exit(1)
	}
}

func flagSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// connectExtension starts the loopback bridge and waits for the extension.
func connectExtension(ctx context.Context, port int, quiet bool) (*cdp.Conn, string, func(), error) {
	token, err := bridgeToken()
	if err != nil {
		return nil, "", nil, err
	}
	srv, err := extbridge.Listen(fmt.Sprintf("127.0.0.1:%d", port), token)
	if err != nil {
		return nil, "", nil, err
	}
	if !quiet {
		fmt.Fprintf(os.Stderr, "bridge: listening on %s — in the browser-ai extension popup set port %d and token %s, then click Connect\n", srv.Addr, port, token)
	}
	hint := time.AfterFunc(3*time.Second, func() {
		fmt.Fprintln(os.Stderr, "bridge: waiting for the extension to connect… (chrome://extensions → Load unpacked → extension/)")
	})
	client, err := srv.Accept(ctx)
	hint.Stop()
	if err != nil {
		srv.Close()
		return nil, "", nil, err
	}
	conn := cdp.NewConn(client)
	product := "Chrome"
	if res, err := conn.Call(ctx, "", "Browser.getVersion", nil); err == nil {
		var v struct {
			Product string `json:"product"`
		}
		_ = json.Unmarshal(res, &v)
		if v.Product != "" {
			product = v.Product
		}
	}
	return conn, "via extension v" + client.Hello.Version + " (" + product + ")", func() { conn.Close(); srv.Close() }, nil
}

// bridgeToken reads or creates ~/.browser-ai/token so the extension is
// configured once.
func bridgeToken() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".browser-ai")
	path := filepath.Join(dir, "token")
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) >= 16 {
		return strings.TrimSpace(string(b)), nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tok := extbridge.NewToken()
	return tok, os.WriteFile(path, []byte(tok+"\n"), 0o600)
}

func connect(ctx context.Context, attach string, headless bool) (*cdp.Conn, string, func(), error) {
	if attach != "" {
		if attach == "auto" {
			attach = ""
		}
		a, err := chrome.Attach(ctx, attach)
		if err != nil {
			return nil, "", nil, err
		}
		return a.Conn, "attached (" + a.Source + ", " + a.Version.Product + ")", func() { a.Close() }, nil
	}
	b, err := chrome.Launch(ctx, chrome.Options{Headless: headless})
	if err != nil {
		return nil, "", nil, err
	}
	return b.Conn, "launched over " + b.Transport + " (" + b.Version.Product + ")", func() { b.Close() }, nil
}

func stepLine(s agent.Step) string {
	target := s.Action
	if s.Target != "" {
		target = "[" + s.Target + "] " + s.Action
	}
	line := fmt.Sprintf("%3d  %-10s %-40s %s", s.N, s.Operation, trunc(target, 40), fmtTiming(s.Timing))
	if s.Text != nil {
		line += fmt.Sprintf("  text=%q", *s.Text)
	}
	if s.PageChanged != nil && !*s.PageChanged {
		line += "  (no change)"
	}
	return line
}

func fmtTiming(t agent.Timing) string {
	ms := func(d time.Duration) string {
		if d == 0 {
			return "-"
		}
		return fmt.Sprintf("%d", d.Milliseconds())
	}
	return fmt.Sprintf("snap %4s  model %4s  text %4s  act %3s  settle %4s  total %5s",
		ms(t.Snapshot), ms(t.Model), ms(t.Text), ms(t.Act), ms(t.Settle), ms(t.Total))
}

func printTable(r *agent.Result) {
	var sum agent.Timing
	var in, outTok, tin, tout int
	texts := 0
	for _, s := range r.Steps {
		sum.Snapshot += s.Timing.Snapshot
		sum.Model += s.Timing.Model
		sum.Text += s.Timing.Text
		sum.Act += s.Timing.Act
		sum.Settle += s.Timing.Settle
		in += s.Usage.InputTokens
		outTok += s.Usage.OutputTokens
		tin += s.TextUsage.InputTokens
		tout += s.TextUsage.OutputTokens
		if s.Timing.Text > 0 {
			texts++
		}
	}
	var modelTotal time.Duration
	for _, d := range r.Decisions {
		modelTotal += d.Latency
	}
	fmt.Println(strings.Repeat("─", 100))
	fmt.Printf("%-8s %d steps · %d decisions (%d stale) · %s\n", strings.ToUpper(r.Status), len(r.Steps), len(r.Decisions), r.Stale, r.Elapsed.Round(time.Millisecond))
	if r.Reason != "" {
		fmt.Println("reason: ", r.Reason)
	}
	avg := func(d time.Duration, n int) string {
		if n == 0 {
			return "-"
		}
		return (d / time.Duration(n)).Round(time.Millisecond).String()
	}
	fmt.Printf("chooser  %s total · avg %s/call · %d in / %d out tokens\n", modelTotal.Round(time.Millisecond), avg(modelTotal, len(r.Decisions)), in, outTok)
	if texts > 0 {
		fmt.Printf("text     %s total · avg %s/call · %d calls · %d in / %d out tokens\n", sum.Text.Round(time.Millisecond), avg(sum.Text, texts), texts, tin, tout)
	}
	fmt.Printf("browser  snapshot %s · act %s · settle %s · %d CDP calls · setup %s\n",
		sum.Snapshot.Round(time.Millisecond), sum.Act.Round(time.Millisecond), sum.Settle.Round(time.Millisecond), r.CDPCalls, r.Setup.Round(time.Millisecond))
	fmt.Printf("final    %s\n         %s\n", r.FinalTitle, r.FinalURL)
	if r.Err != nil {
		fmt.Printf("error    %v\n", r.Err)
	}
}

func writeTrace(path string, r *agent.Result) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// loadEnv reads KEY=VALUE lines; existing environment wins.
func loadEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		if v != "" && os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

// firstEnv returns the first non-empty environment variable among names.
func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

func trunc(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
