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
