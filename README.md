# browser-ai

Jev-driven browser agent in pure Go (stdlib only). Drives Chrome over CDP — launched (pipe) or
attached to your real Chrome (WebSocket) — and lets [TypeSafe's Jev](https://docs.typesafe.ai) pick
one action per step from an indexed element table. Built to measure real end-to-end speed.

Design: [docs/DESIGN.md](docs/DESIGN.md) · Reference: [browser-use/jev-ultrafast](https://github.com/browser-use/jev-ultrafast) (Python, MIT)

Status: design done, implementation tracked in issues.

## Plan

Milestones and issues: https://github.com/phanngoc/browser-ai/milestones

| Milestone | Issues |
|---|---|
| M1 Transport & CDP core | #1 ws · #2 cdp · #3 chrome launch (pipe) · #4 chrome attach · #5 bench · #16 CI |
| M2 Snapshot & Browser | #6 snapshot · #7 browser · #8 fixture tests |
| M3 Model & Agent | #9 jev · #10 textgen · #11 agent · #12 cli |
| M4 Polish & Benchmark | #13 docs · #14 screenshots/trace · #15 e2e benchmark |

Order: 3 → 2 → 1 → 4 → 5 (bench measurable with no API key), then 6 → 7 → 8, then 9 → 10 → 11 → 12.
