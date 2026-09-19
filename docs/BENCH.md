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

## End-to-end with the LLM fallback chooser (`--chooser llm`)

Same date/machine. No TypeSafe key yet, so decisions come from a general LLM via OpenRouter
(`google/gemini-2.5-flash-lite`), text from `inception/mercury-2.5`. **These are not Jev numbers**:
the loop, snapshot and executor are the same, only the decision maker is ~8× slower.

### Wikipedia → "Gödel's incompleteness theorems", headless, launched over pipe

| run | result | steps | decisions (stale) | total | chooser total (avg/call) | text | browser (snap+act+settle) | CDP calls |
|---|---|---|---|---|---|---|---|---|
| 1 | done ✓ | 2 | 4 (1) | 7.55 s | 5.44 s (1.36 s) | 0.77 s | 0.75 s | 63 |
| 2 | done ✓ | 2 | 6 (3) | 7.05 s | 5.31 s (0.89 s) | 0.97 s | 0.55 s | 44 |
| 3 | done ✓ | 2 | 6 (3) | 6.55 s | 5.27 s (0.88 s) | 0.55 s | 0.54 s | 65 |

Median **7.05 s**, 3/3 verified by final URL. Reference (Jev, Python): 2.80 s.
~75 % of wall time is the LLM deciding; the browser side is ≈ 0.6 s per run including a real
page load. Stale decisions are frequent because a 1 s decision gives the page time to change
(autocomplete appearing); each stale costs one more model call.

Read-through: with Jev's ~170 ms decisions the same run would be ≈ 0.6 s browser + 2–4 × 0.17 s
decisions + 0.5–0.8 s text ≈ **2–2.5 s**, in line with the reference's 2.8 s.

### Google Flights, one-way ZRH → LON on 2026-09-20, headless

Goal text identical to the reference. Outcome verified from the final page, not from the model's DONE.

| run | chooser model | result | steps | decisions (stale) | total | chooser (avg/call) | text | browser | CDP |
|---|---|---|---|---|---|---|---|---|---|
| 1 | gemini-2.5-flash-lite | **false DONE ✗** | 9 | 13 (3) | 33.2 s | 29.6 s (2.28 s) | 0.9 s | 0.9 s | 231 |
| 2 | gemini-2.5-flash | done ✓ (21 results, BA/easyJet) | 10 | 17 (6) | 22.7 s | 18.1 s (1.06 s) | 1.6 s | 1.3 s | 209 |

Run 1: the small model opened the *multi-airport origin* dialog and typed "London" into
"Where else?", then declared DONE on the empty search form (origin still the geo default "Da Nang").
The executor did exactly what was chosen; the policy was wrong. This is why DONE is never trusted.

Run 2: correct sequence — ticket type → One way → type Zurich → pick suggestion → type London →
pick suggestion → open date → 20 Sep → Done → Search → results visible → DONE.
Reference (Jev, Python): 7.09 s median for the same task, ~101 CDP calls.

Browser-side cost for the whole 10-step run was **1.3 s** (snapshot 0.13 s, act 0.53 s, settle 0.66 s).
Everything else was model latency. Swap in Jev at ~0.17 s/decision and the same run projects to
≈ 1.3 s + 17 × 0.17 s + 1.6 s text ≈ **5–6 s** — the reference's 7 s is consistent with that.
