# browser-ai

Jev-driven browser agent in pure Go (stdlib only). Drives Chrome over CDP — launched (pipe) or
attached to your real Chrome (WebSocket) — and lets [TypeSafe's Jev](https://docs.typesafe.ai) pick
one action per step from an indexed element table. Built to measure real end-to-end speed.

Design: [docs/DESIGN.md](docs/DESIGN.md) · Reference: [browser-use/jev-ultrafast](https://github.com/browser-use/jev-ultrafast) (Python, MIT)

Status: design done, implementation tracked in issues.
