# Bench results

`go run ./cmd/bench <rtt|snapshot|coldstart>` — browser side only, no model calls.

## 2026-09-19 · MacBook (darwin/arm64) · Chrome 153.0.8010.48 · Go 1.26

### rtt — `Runtime.evaluate("1")` round-trip, headless, n=2000

| transport | min | p50 | p95 | p99 | max |
|---|---|---|---|---|---|
| pipe (launched, `--remote-debugging-pipe`) | 72 µs | **156 µs** | 318 µs | 1.01 ms | 5.7 ms |
| ws (launched, `--remote-debugging-port=0`) | 87 µs | **202 µs** | 311 µs | 417 µs | 1.8 ms |
| ws (attached to separate Chrome, port 9333) | 112 µs | **172 µs** | 301 µs | 984 µs | 2.1 ms |

Pipe is ~25% lower at p50; both are far below the cost of one snapshot, so transport is not the bottleneck.

### snapshot — Wikipedia Main Page, 1120×780, n=30

| | p50 | p95 |
|---|---|---|
| `snapshot.js` evaluate (incl. CDP RTT) | **5.7 ms** | 9.4 ms |
| decode + fingerprint (Go) | 0.32 ms | 0.53 ms |

Payload 36.7 KB · 49 actions · 1914 chars visible text · load→readyState 0.34–0.84 s (network).

### coldstart — headless, n=3

| run | exec → `Browser.getVersion` | newPage + ready | total |
|---|---|---|---|
| 1 (cold OS cache) | 6.08 s | 100 ms | 6.18 s |
| 2 | 353 ms | 84 ms | 437 ms |
| 3 | 364 ms | 84 ms | 448 ms |

## Reading

Per agent step the browser costs ≈ 6 ms snapshot + a few CDP calls at ~0.2 ms each.
Everything else in a step is the model round-trip (reference reports ~170 ms for Jev) and
settle waits (50–200 ms). Reference Python run: 101 CDP calls per 7 s task.

## End-to-end — Jev vs a general LLM (2026-09-20)

Same machine, headless Chrome 153 launched over the pipe, identical goals to the reference.
Every ✓ is verified from the final page (URL/title, or "N results returned" in the visible text),
never from the model's DONE. Text for `TYPE_TEXT` comes from `inception/mercury-2.5` in all runs.

### Headline

Three runs per model.

| task | decision maker | pass | median total | per decision | decisions | browser share |
|---|---|---|---|---|---|---|
| Google Flights ZRH→LON one-way, verify results visible | **Jev** (`jev-1.13.0`) | **3/3** | **9.57 s** | **330 ms** | 18–19 | 1.2 s |
| | gemini-2.5-flash (via OpenRouter) | 3/3 | 22.99 s | 1.06–1.16 s | 17–19 | 1.3 s |
| | gemini-2.5-flash-lite | **0/3** (false DONE ×3) | 13.78 s | 0.92–2.28 s | 12–13 | 0.9 s |
| | gpt-5.4-mini, `reasoning_effort: low` (api.openai.com) | 2/3 | 29.41 s | 1.29–1.69 s | 11–35 | 0.5–1.9 s |
| | gpt-5.4-mini, `reasoning_effort: none` | 0/3 | 22.01 s | 0.85–0.91 s | 13–29 | 0.5–1.5 s |
| | gpt-5.4-nano, `none` / `low` | 0/3 · 0/3 | 16.73 s · 37.36 s | 0.80–1.65 s | 12–37 | 0.5–2.1 s |
| Wikipedia → Gödel article | **Jev** | **3/3** | **3.44 s** | **330 ms** | 6 | 0.6 s |
| | gemini-2.5-flash-lite | 3/3 | 7.05 s | 0.9–1.4 s | 4–6 | 0.6 s |
| Reference (browser-use/jev-ultrafast, Python, Jev from Europe) | Jev | 3/3 | 7.09 s / 2.80 s | ~170 ms | — | — |

**Jev is 3–7× faster per decision than a general LLM.** gemini-2.5-flash reaches the same 3/3 at
2.4× the wall time; flash-lite never passes. The gap to the reference's 7.09 s is network: this
machine is in Vietnam.

### Google Flights with general LLMs, run by run

| model | run | result | steps | decisions (stale) | total | model total (avg) | text | browser | CDP |
|---|---|---|---|---|---|---|---|---|---|
| gemini-2.5-flash | 1 | ✓ 21 results | 10 | 17 (6) | 22.70 s | 18.06 s (1062 ms) | 1.64 s | 1.31 s | 209 |
| | 2 | ✓ | 10 | 19 (8) | 25.38 s | 21.94 s (1155 ms) | 1.63 s | 1.29 s | 230 |
| | 3 | ✓ | 10 | 18 (7) | 22.99 s | 19.09 s (1060 ms) | 2.20 s | 1.31 s | 202 |
| gemini-2.5-flash-lite | 1 | ✗ false DONE | 9 | 13 (3) | 33.20 s | 29.61 s (2277 ms) | 0.91 s | 0.92 s | 231 |
| | 2 | ✗ false DONE | 8 | 12 (3) | 12.77 s | 11.07 s (922 ms) | 0.65 s | 0.89 s | 169 |
| | 3 | ✗ false DONE | 8 | 12 (3) | 13.78 s | 11.93 s (994 ms) | 0.76 s | 0.92 s | 183 |

| gpt-5.4-mini · none | 1 | ✗ blocked (loops on "Open Where else?") | 8 | 13 (4) | 13.05 s | 11.14 s (857 ms) | 1.20 s | 0.54 s | 200 |
| | 2 | ✗ false DONE — Zürich, London, one way, 20 Sep all set, never clicked Search | 10 | 20 (9) | 22.01 s | 18.26 s (913 ms) | 1.77 s | 1.50 s | 180 |
| | 3 | ✗ false DONE on "Explore" page | 17 | 29 (11) | 27.97 s | 24.56 s (847 ms) | 1.38 s | 1.46 s | 275 |
| gpt-5.4-mini · low | 1 | ✗ client error: empty reply under a 300-token cap (fixed: 2000 when reasoning is on) | 7 | 11 (4) | 26.57 s | 18.56 s (1687 ms) | 1.18 s | 0.51 s | 188 |
| | 2 | ✓ | 12 | 19 (6) | 29.41 s | 24.42 s (1285 ms) | 3.10 s | 1.58 s | 200 |
| | 3 | ✓ | 23 | 35 (11) | 64.03 s | 58.53 s (1672 ms) | 3.09 s | 1.87 s | 317 |
| gpt-5.4-nano · none | 1–3 | ✗ blocked ×3 (budget / three no-change actions) | 9–25 | 12–37 | 13.4–35.5 s | 0.80–0.85 s/call | | | |
| gpt-5.4-nano · low | 1–3 | ✗ blocked ×3 | 9–25 | 16–28 | 21.4–49.5 s | 1.23–1.65 s/call | | | |

OpenAI models were driven with the cheapest reasoning setting each accepts (`none` for gpt-5.4,
otherwise `minimal`) unless noted; `low` roughly doubles per-decision latency. Same text helper
(mercury-2.5) for every row.

flash-lite took the identical wrong path in all three runs: ticket type → One way → open "Where
from?" → **"Origin, Select multiple airports"** → type "London" into "Where else?" → Done → open
Departure → 20 Sep → DONE. Origin stayed at the geo default, destination was never set. Its lower
wall time is only because it quit early. gemini-2.5-flash followed the same correct sequence as Jev
every time; the difference is purely decision latency.

### Where a Jev decision's 330 ms goes

`curl` timings to `api.typesafe.ai` from here: TCP connect **~200 ms**, TLS +200 ms (paid once,
connection is kept alive), warm call total **350 ms**. So of each decision ≈ 200 ms is the round trip
across the Pacific and **≈ 130 ms is Jev itself** — consistent with the ~170 ms the reference sees
from Europe. Run this from a US/EU box and the Flights run projects to ≈ 6.5 s.

### Google Flights with Jev, run by run

| run | steps | decisions (stale) | total | Jev total (avg) | text | browser | CDP calls | results |
|---|---|---|---|---|---|---|---|---|
| 1 | 10 | 18 (7) | 9.68 s | 5.86 s (326 ms) | 1.31 s | 1.14 s | 229 | 16 ✓ |
| 2 | 10 | 19 (8) | 9.55 s | 6.13 s (323 ms) | 1.15 s | 1.28 s | 235 | 16 ✓ |
| 3 | 10 | 18 (7) | 9.57 s | 6.02 s (334 ms) | 1.28 s | 1.18 s | 201 | 16 ✓ |

Sequence every time: ticket type → One way → type Zurich → pick "Zürich, Switzerland" → type London →
pick "London, United Kingdom" → open Departure → 20 Sep → Done → Search → results → DONE.
Two `TYPE_TEXT` calls to the text helper cost more (1.2 s) than the whole browser side (1.2 s).

### Wikipedia with Jev

| run | decisions (stale) | total | Jev (avg) | text | browser |
|---|---|---|---|---|---|
| 1 | 6 (3) | 4.18 s | 2.15 s (358 ms) | 1.21 s | 0.62 s |
| 2 | 6 (3) | 3.38 s | 1.95 s (326 ms) | 0.57 s | 0.61 s |
| 3 | 6 (3) | 3.44 s | 1.93 s (322 ms) | 0.66 s | 0.62 s |

Run-to-run variance is almost entirely the text helper (0.57–1.21 s for one call).

### What we tried against stale decisions (and what the data said)

A decision is *stale* when the page changed between observation and execution; the executor refuses
it and one more model call is spent. 40 % of Flights decisions were stale.

| change | Flights stale | Flights total | Wikipedia total | kept? |
|---|---|---|---|---|
| baseline | 7–8 / 18–19 | 9.57 s | 3.44 s | — |
| re-read until two markers agree (≤3× after 2 rAF) | 6–8 | 9.65 s | 3.38 s | **no** — off by default (`Options.StableChecks`) |
| WAIT never stale + scoped guard for TYPE_TEXT + retry text helper once | 4–7 | 9.59 s | 3.82 s | yes — same time, honest history |

Neither moved wall time: the page changes *during* the 330 ms model call, and while results load the
model keeps choosing WAIT (330 ms each). Stale is a symptom of network latency to the model, not of
the browser layer. The total is bounded by `decisions × latency` — the only lever left is being closer
to `api.typesafe.ai`.

## Reading

Per agent step the browser costs ≈ 6 ms snapshot + a few CDP calls at ~0.2 ms each, plus a
50–200 ms settle wait that is deliberate. Everything else is model latency.
