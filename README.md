# browser-ai

A browser agent in pure Go (stdlib only) driven by **Jev**, TypeSafe's System One model.
Jev does not generate text — it picks one option from a finite, indexed action space, so a
decision is one cheap round-trip. A small OpenAI-compatible LLM writes field values only when
the chosen operation is `TYPE_TEXT`.

Chrome is driven over CDP with a hand-rolled client: **launched over `--remote-debugging-pipe`**
(no TCP, no framing) or **attached to your real running Chrome** over a minimal WebSocket
client. Everything is instrumented so you can see where each step's time goes.

Reference implementation: [browser-use/jev-ultrafast](https://github.com/browser-use/jev-ultrafast)
(Python, MIT). The DOM snapshot script and the policy instructions are ported from it.

## Performance

Real runs, headless Chrome 153, this repo, 2026-09-20. Every pass is verified from the final page,
not from the model saying DONE. Full tables and method in [docs/BENCH.md](docs/BENCH.md).

Three runs per model, same goal text, pass = verified from the final page.

| task | decision maker | pass | median | per decision |
|---|---|---|---|---|
| **Google Flights** ZRH→LON one-way, until results visible (10 steps) | **Jev** | **3/3** | **9.6 s** | **330 ms** |
| | gemini-2.5-flash | 3/3 | 23.0 s | 1.06–1.16 s |
| | gpt-5.4-mini, reasoning low | 2/3 | 29.4 s | 1.3–1.7 s |
| | gpt-5.4-mini, reasoning none | 0/3 — one run had everything set but declared DONE before Search | 22.0 s | 0.85–0.91 s |
| | gemini-2.5-flash-lite | **0/3** — same wrong path every time, then a false DONE | 13.8 s | 0.9–2.3 s |
| | gpt-5.4-nano (none / low) | 0/3 · 0/3 | 16.7 s · 37.4 s | 0.8–1.7 s |
| **Wikipedia** → Gödel's incompleteness theorems (2 steps) | **Jev** | **3/3** | **3.4 s** | **330 ms** |
| | gemini-2.5-flash-lite | 3/3 | 7.1 s | 0.9–1.4 s |

Jev is a decision model, not a text generator: it picks one of the offered operations and one of
the offered element indices, and returns a probability over each. That is why a decision costs
**330 ms from Vietnam — about 200 ms of which is the round trip to `api.typesafe.ai`; Jev itself is
≈ 130 ms** — versus 1–2 s for a general LLM asked the same question. On Flights, gemini-2.5-flash also
went 3/3 but took 2.4× longer; gpt-5.4-mini needed reasoning turned on to pass 2/3 at 3× the time;
the small models (flash-lite, gpt-5.4-nano) never passed — they take the multi-airport dialog wrong
turn or loop on a closed dropdown, then declare DONE with no destination set.

Where the 9.6 s of a Flights run goes:

```
Jev          6.0 s   18–19 decisions × 330 ms (≈ 3.7 s of it is network from VN)
text model   1.2 s   2 × TYPE_TEXT via mercury-2.5
browser      1.2 s   snapshot 0.13 s · input 0.45 s · settle waits 0.6 s · ~220 CDP calls
```

The browser side — hand-rolled CDP over a pipe, one atomic DOM snapshot per step, hit-tested input —
is ~12 % of the run. The reference Python implementation reports 7.09 s for the same task from Europe
(~170 ms per Jev call); from the same continent this run projects to ≈ 6.5 s.

## Quick start

```sh
cp .env.example .env            # TYPESAFE_API_KEY, TEXT_MODEL_API_KEY (OpenRouter by default)
go run ./cmd/agent --headless \
  --url https://en.wikipedia.org/wiki/Main_Page \
  --goal "Find and open the Wikipedia article about Gödel's incompleteness theorems."
```

Use your own Chrome (profile, cookies, logins) — see [docs/ATTACH.md](docs/ATTACH.md):

```sh
go run ./cmd/agent --attach auto --url … --goal …
```

### No TypeSafe key yet? `--chooser llm`

When `TYPESAFE_API_KEY` is empty the agent falls back to `internal/llmchooser`: a general chat
model (default `google/gemini-2.5-flash-lite` via the `TEXT_MODEL_*` credentials, override with
`CHOOSER_MODEL` / `--chooser-model`) picks the operation and target from the **same** indexed
action space, and its answer is validated the same way — only offered indices can execute. It is
the same loop, just a slower decision maker: ~1.4 s per decision instead of Jev's ~0.17 s.

See the Performance table above for how it compares: same loop, 3–7× slower decisions, and the
small model got Google Flights wrong.

Output, one line per executed action, then a summary:

```
browser: launched over pipe (Chrome/153.0.8010.48) ready in 1.16s · jev warm 153ms · text warm 1ms
  1  TYPE_TEXT  [1] Name        snap    1  model  152  text  301  act  49  settle   29  total   584  text="Zurich"
  2  CLICK      [10] Submit     snap    1  model  151  text    -  act   9  settle   21  total   223
────────────────────────────────────────────────────────────────────────────────────────
DONE     2 steps · 3 decisions (0 stale) · 964ms
jev      455ms total · avg 152ms/call · 1800 in / 4 out tokens
text     301ms total · avg 301ms/call · 1 calls · 400 in / 4 out tokens
browser  snapshot 4ms · act 60ms · settle 51ms · 30 CDP calls · setup 26ms
final    Fixture
         http://127.0.0.1:8765/fixture
```

(All numbers in ms. This sample used a scripted stand-in for the models. A real Jev run on Google
Flights looks like this:)

```
  1  CLICK      [12] Change ticket type. Round trip   snap  6  model 370  text   -  act 33  settle  23  total  501
  2  CLICK      [14] One way                          snap  6  model 269  text   -  act 63  settle  32  total  446
  3  TYPE_TEXT  [15] Where from?                      snap  6  model 314  text 739  act 34  settle 203  total 1359  text="Zurich"
  4  CLICK      [3] Zürich, Switzerland               snap 14  model 370  text   -  act 23  settle  30  total  476
  5  TYPE_TEXT  [16] Where to?                        snap 12  model 329  text 574  act 45  settle 157  total 1174  text="London"
  6  CLICK      [3] London, United Kingdom            snap 15  model 354  text   -  act 36  settle  38  total  520
  7  CLICK      [18] Open Departure                   snap 10  model 305  text   -  act 57  settle  48  total  497
  8  CLICK      [3] Sunday, September 20, 2026        snap 21  model 303  text   -  act 41  settle  37  total  435
  9  CLICK      [52] Done. Search for one-way flights snap 22  model 301  text   -  act 28  settle  29  total  477
 10  CLICK      [19] Search                           snap  7  model 293  text   -  act 37  settle  15  total  804
DONE     10 steps · 18 decisions (7 stale) · 9.684s
chooser  5.865s total · avg 326ms/call · 44023 in / 3240 out tokens
text     1.314s total · avg 657ms/call · 2 calls
browser  snapshot 124ms · act 402ms · settle 615ms · 229 CDP calls
final    Zürich to London | Google Flights
```

### Flags

| flag | |
|---|---|
| `--url`, `--goal` | required |
| `--headless` | launch Chrome headless (default: launch a visible window) |
| `--attach auto\|ws://…\|http://host:port\|<user-data-dir>` | drive a running Chrome instead of launching |
| `--keep-open` | leave the tab open after the run |
| `--trace run.json` | full trace: steps, every decision with request/answers, timings |
| `--screenshots dir/` | JPEG after each step, named by elapsed ms |
| `--max-steps 60` | action budget (model calls capped at 2×) |
| `--chooser jev\|llm` | decision backend; default `jev` when `TYPESAFE_API_KEY` is set, else `llm` |
| `--chooser-model` | model for `--chooser llm` |
| `--quiet` | summary only |

Exit code: `0` done · `2` blocked · `1` error. `DONE` is the model's opinion — verify the final
URL/title/page yourself.

### Environment (`.env` is loaded if present)

```
TYPESAFE_API_KEY=            # required
TYPESAFE_MODEL=jev-latest
TEXT_MODEL_API_KEY=          # required for TYPE_TEXT; any OpenAI-compatible endpoint
TEXT_MODEL_BASE_URL=https://openrouter.ai/api/v1
TEXT_MODEL=inception/mercury-2.5
TEXT_MODEL_REASONING=none
CHOOSER_MODEL=google/gemini-2.5-flash-lite   # only for --chooser llm
CHROME_PATH=                 # optional
```

## Benchmarks without an API key

```sh
go run ./cmd/bench rtt --headless          # CDP round-trip, pipe vs ws
go run ./cmd/bench snapshot --url https://www.google.com/travel/flights
go run ./cmd/bench coldstart
```

On an M-series Mac with Chrome 153: pipe RTT **156 µs** p50, snapshot of Wikipedia **5.7 ms**,
Chrome cold start **~440 ms**. Details in [docs/BENCH.md](docs/BENCH.md).

## How a step works

```
observe ─► choose (1 Jev request: operation + a target per operation) ─► [TYPE_TEXT → small LLM]
   ▲                                                                              │
   └──────────── stale? re-observe, never retry a mutation ◄──── fresh? ─► act ─► settle
```

### Data flow of one step, with the real payloads

Taken from a recorded Google Flights run (`--trace`), step 3: the agent has already switched to
"One way" and must now fill the origin.

**1. observe — one CDP call, ~6 ms**

`Runtime.evaluate(snapshot.js)` returns the whole page state atomically:

```jsonc
{
  "url": "https://www.google.com/travel/flights?…", "title": "Find Cheap Flights … - Google Flights",
  "text": "Skip to main content\nExplore\nFlights\nHotels\n…",        // viewport-visible text only, ≤ 6000 chars
  "actions": [                                                          // ≤ 250 executable candidates
    {"id":"e15","kind":"fill", "node":31,"role":"combobox","label":"Where from?","value":"Da Nang","expanded":"false"},
    {"id":"e16","kind":"click","node":31,"role":"combobox","label":"Open Where from?", …},
    {"id":"e17","kind":"fill", "node":32,"role":"combobox","label":"Where to? ", …},
    …, {"id":"scroll_down","kind":"scroll","delta":560}, {"id":"wait","kind":"wait"}
  ],
  "marker":  [timeOrigin, url, scrollX, scrollY, w, h, title, text, actions-without-geometry, formValues],
  "page_key":[timeOrigin, url, scroll…, [[nodeId, value, checked, selectedIndex, disabled, readOnly], …]],
  "guards":  {"31": [31,"combobox","Where from?","Da Nang",…, "<innerText of enclosing form/dialog>"], …}
}
```

`node` is an id the script assigned through a `WeakMap` — not a CDP node id, never a selector. `marker`,
`page_key` and `guards` are what the freshness checks compare later.

**2. choose — one HTTPS request to Jev, ~330 ms (≈ 200 ms of it network from VN)**

`POST https://api.typesafe.ai/v1/systemone` · `Authorization: Bearer $TYPESAFE_API_KEY`

```jsonc
{
  "model": "jev-latest",
  "state": {
    "page": {"url": "…", "title": "…", "text": "…"},
    "elements": [                                      // one index per node, both operations listed
      {"index":"15","label":"Where from?","role":"combobox","value":"Da Nang","expanded":"false","operations":["TYPE_TEXT","CLICK"]},
      {"index":"16","label":"Where to? ","role":"combobox","expanded":"false","operations":["TYPE_TEXT","CLICK"]},
      {"index":"17","label":"Departure","role":"textbox","operations":["TYPE_TEXT","CLICK"]}, …   // 24 elements
    ],
    "recent_actions": [
      {"action":"Change ticket type. Round trip","kind":"click","text":null,"page_changed":true},
      {"action":"One way","kind":"click","text":null,"page_changed":true}
    ]
  },
  "questions": {                                       // answered together, in one round trip
    "operation": {"type":"choice",
      "criteria": {"CLICK":"Click an element, button, menu option, autocomplete suggestion, or calendar day.",
                   "TYPE_TEXT":"Enter or replace text in an editable field. A small LLM will supply the value from the goal.",
                   "SCROLL_DOWN":"Scroll down","WAIT":"Wait for the page to update",
                   "DONE":"Every requirement is visibly satisfied.","BLOCKED":"No supported operation can progress."},
      "instructions": {"goal":"Find one-way flights from Zurich to London on September 20, 2026 …","rules":"<policy>"}},
    "click_target":     {"type":"choice","criteria": {"1":{…},…,"15":{"element":"[15] Where from?","current_value":"Da Nang","role":"combobox","expanded":"false"},…}, "instructions": {…,"operation":"CLICK"}},
    "type_text_target": {"type":"choice","criteria": {"15":{…},"16":{…},"17":{…}},                              "instructions": {…,"operation":"TYPE_TEXT"}}
  }
}
```

Response (314 ms):

```jsonc
{
  "model": "jev-1.13.0",
  "answers": {
    "operation":        {"type":"choice","choice":"TYPE_TEXT","confidence":0.92,"probabilities":{"TYPE_TEXT":0.93,"CLICK":0.04,"BLOCKED":0.02,…}},
    "type_text_target": {"type":"choice","choice":"15",       "confidence":0.95,"probabilities":{"15":0.97,"16":0.02,"17":0.01}},
    "click_target":     {"type":"choice","choice":"15",       "confidence":0.85,"probabilities":{"15":0.87,"16":0.10,"17":0.02,…}}   // computed but unused
  },
  "usage": {"input_tokens": 2400, "output_tokens": 180}
}
```

The agent reads `operation`, then **only** the matching head (`type_text_target`), validates it
(choice ∈ offered ids, probabilities over exactly those ids, sum ≈ 1, argmax = choice) and maps
index `15` back to action `e15` (`kind: fill`, node 31). `click_target` is never looked at.

**3. TYPE_TEXT only — one request to the text helper, ~600–900 ms**

`POST $TEXT_MODEL_BASE_URL/chat/completions` (OpenAI-compatible, `response_format: json_object`):

```jsonc
{"model":"inception/mercury-2.5","max_tokens":1024,"response_format":{"type":"json_object"},"reasoning":{"enabled":false},
 "messages":[
   {"role":"system","content":"Return a JSON object with exactly one key, text: the exact string to enter in the selected field. … If a required value is missing, return {\"text\": null}."},
   {"role":"user","content":"{\"goal\":\"Find one-way flights from Zurich to London …\",\"field\":{\"label\":\"Where from?\",\"role\":\"combobox\",\"value\":\"Da Nang\"},\"page\":{\"title\":\"…\",\"text\":\"…\"},\"recent_actions\":[…]}"}
 ]}
```

Response content: `{"text":"Zurich"}` (433 in / 78 out tokens, 739 ms). Anything other than a single
non-empty `text` string → nothing is typed. The value is cached and reused only if the *entire* helper
input is byte-identical after a stale retry.

**4. fresh? → act — 5–7 CDP calls, ~35 ms**

```
Runtime.evaluate  [pageKey(), guard(node 31)]      == observation's page_key + guards["31"]?  else ErrStale
Runtime.evaluate  resolve(e15)                      connected · visible · not disabled/readonly ·
                                                    centre inside viewport · elementFromPoint hits it → {x, y}
Input.dispatchMouseEvent mousePressed  (x, y)
Input.dispatchMouseEvent mouseReleased (x, y)
Input.dispatchKeyEvent   keyDown ⌘A / ^A, commands:["selectAll"]
Input.dispatchKeyEvent   keyUp
Input.insertText         "Zurich"
```

Coordinates come from the live DOM at this instant, never from the model. A click/select/fill whose
guard changed, or whose target is covered, returns `ErrStale`: the decision is dropped, the page is
re-observed, and the loop continues — a mutation is never retried.

**5. settle — one CDP call, 50–200 ms**

`Runtime.evaluate(awaitPromise)`: ≤ 2 animation frames or 50 ms; after typing into an editable
combobox, until a `[role=option]` is visible, capped at 200 ms. Then the step is logged
(`history` gets `{"action":"Where from?","kind":"fill","text":"Zurich","page_changed":true}`) and the
loop goes back to **1**.

Per step: **≈ 8 CDP calls, 1 Jev call, 0–1 text-helper call.** Per run (Flights): ~220 CDP calls,
18–19 Jev calls, 2 text calls.

### Design rules

- **Snapshot** (`internal/snapshot/snapshot.js`) is atomic and viewport-only, so the model's context
  stays small and every candidate is something the user could actually see and hit.
- **Decision** (`internal/jev`): speculative target heads mean one round trip per decision.
- **Execute** (`internal/browser`): re-check freshness (scoped guard for click/fill/select, full
  marker for DONE/BLOCKED), re-resolve geometry, hit-test, then real input events. Native `<select>`
  is set in page and its change event confirmed.
- **Budgets**: 60 actions, 120 decisions, three consecutive no-change actions → `blocked`.

Model output never becomes a selector, coordinate, script or shell command.

## Layout

```
cmd/agent        CLI
cmd/bench        browser-side benchmarks
internal/cdp     CDP client: header-only decode, flat sessions, bounded event bus
internal/ws      RFC 6455 client (for --attach)
internal/chrome  launch (pipe / port=0) and attach (DevToolsActivePort, /json/version)
internal/snapshot  snapshot.js + typed Page/Action + fingerprint
internal/browser Observe / Fresh / Act / Screenshot
internal/jev     action space, questions, TypeSafe client, validation
internal/llmchooser  fallback decision backend on a general LLM (same action space, same validation)
internal/textgen OpenAI-compatible helper for TYPE_TEXT
internal/agent   the loop, history, stale handling, timings
```

## Development

```sh
go test -race ./...      # Chrome-dependent tests skip when no Chrome is installed
```

Tests run against real headless Chrome with a local fixture page
(`internal/browser/testdata/fixture.html`) and scripted stand-ins for both models.

## Limits

Shadow DOM, iframes, canvas, file uploads, pop-up tabs, nested scrolling and complex keyboard
widgets are out of scope. Jev is trained mostly on English; write goals in English.
The DOM reader covers common HTML/ARIA controls, not the full accessible-name algorithm.

## Plan

[Milestones and issues](https://github.com/phanngoc/browser-ai/milestones) · [Design](docs/DESIGN.md)

MIT.
