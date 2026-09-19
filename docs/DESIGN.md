# browser-ai — Jev-driven browser agent, pure Go

Mục tiêu: agent điều khiển Chrome bằng model **Jev** (TypeSafe System One) — viết lại
toàn bộ bằng Go, **zero dependency ngoài stdlib**, để đo tốc độ thật của từng khâu.
Reference tham khảo: `browser-use/jev-ultrafast` (Python, MIT). Không dùng chromedp/rod.

## 1. Jev là gì (contract đã xác nhận)

`POST https://api.typesafe.ai/v1/systemone` · `Authorization: Bearer <key>` · model `jev-latest`

```jsonc
// request
{
  "model": "jev-latest",
  "state": { "page": {url,title,text}, "elements": [...], "recent_actions": [...] },
  "questions": {
    "operation":        { "type":"choice", "criteria": {CLICK:"...",TYPE_TEXT:"...",SELECT:"...",SCROLL_DOWN:"...",WAIT:"...",DONE:"...",BLOCKED:"..."}, "instructions": {...} },
    "click_target":     { "type":"choice", "criteria": {"1":{element,current_value,role,...}, "7":{...}}, "instructions": {...} },
    "type_text_target": { "type":"choice", "criteria": {...} },
    "select_target":    { "type":"choice", "criteria": {"3:1":{...}, "3:2":{...}} }   // index:option
  }
}
// response
{ "model":"jev-...", "answers": { "operation": {choice,probabilities,confidence}, "click_target": {...}, ... }, "usage": {input_tokens,output_tokens} }
```

- Jev **không sinh text**, chỉ chọn 1 option trong tập hữu hạn → rất nhanh, rất rẻ.
- 1 request trả lời đồng thời `operation` + target head cho **mọi** operation (speculative fan-out).
  Executor chỉ đọc head khớp với operation được chọn.
- `TYPE_TEXT` cần một LLM nhỏ (OpenAI-compatible) sinh giá trị điền vào field.
- Choice ≤ 255 option. Lỗi 429/529 → backoff.

## 2. Kiến trúc

```
cmd/
  agent/      CLI: --url --goal [--headless | --attach [ws://host:port]] [--trace out.json]
  bench/      đo RTT CDP thô, Runtime.evaluate, snapshot — KHÔNG cần API key
internal/
  ws/         RFC 6455 client tối giản (handshake, mask, frame, ping/pong, fragment)
  cdp/        Transport interface {Send,Recv}; Conn: id↔reply correlation, flat sessions,
              event fan-out theo sessionId, ctx timeout. Không codegen, params là map/struct.
  chrome/     Launch: --remote-debugging-pipe (fd 3/4, \0-delimited), temp profile, kill on exit
              Attach: Chrome thật đang chạy — dò DevToolsActivePort trong profile dir hoặc
              /json/version; tự bật `chrome://inspect` remote-debugging nếu chưa mở port;
              tạo tab riêng (Target.createTarget background:true), không đụng tab của user
  snapshot/   snapshot.js (go:embed) + struct Page/Action/Guard; fingerprint sha256
  browser/    Browser{Observe, Fresh, Act}: click/fill/select/scroll/wait, settle sau input
  jev/        build questions từ actions (action_space), POST, validate answer chặt
  textgen/    helper OpenAI-compatible cho TYPE_TEXT, JSON {"text": ...} only
  agent/      loop observe → choose → act; history, stale-retry, budget; Timing per phase
```

### 2.0 Attach Chrome thật (first-class, không phải tuỳ chọn phụ)

Chrome ≥ 136 không cho `--remote-debugging-port` trên profile mặc định; đường đi:
1. Nếu Chrome đang chạy **đã** mở DevTools port → đọc `~/Library/Application Support/Google/Chrome/DevToolsActivePort`
   (dòng 1 port, dòng 2 path ws) → nối WebSocket browser endpoint.
2. Nếu chưa → hướng dẫn user bật *Allow remote debugging* tại `chrome://inspect/#remote-debugging`
   (Chrome 144+), hoặc `--attach ws://127.0.0.1:9222` khi user tự khởi động Chrome với port.
3. Luôn `Target.createTarget(about:blank, background:true)` + `Emulation.setFocusEmulationEnabled`
   → tab của agent render bình thường dù không phải tab đang nhìn; đóng tab khi xong.

Dependency graph một chiều: `agent → browser,jev,textgen → cdp,snapshot → ws/chrome`.

### 2.1 Transport — 2 đường, cùng interface

| | pipe (`--remote-debugging-pipe`) | websocket |
|---|---|---|
| Overhead | 0 framing, không TCP | HTTP upgrade + frame/mask |
| Dùng khi | mình launch Chrome (`--headless`/mặc định) | `--attach` Chrome thật đang chạy (profile, cookie, đã login) |
| Implement | `os/exec` + `ExtraFiles`, đọc tới `\0` | tự viết `internal/ws` ~200 dòng |

### 2.2 CDP client

- `Call(ctx, session, method, params) (json.RawMessage, error)` — mỗi call 1 id, reply
  route qua `map[id]chan`. Một goroutine reader duy nhất, decode **chỉ header**
  (`id, sessionId, method`) rồi giao raw payload → không decode toàn bộ event như chromedp.
- Event bus: `Subscribe(session, method) <-chan RawMessage`, buffered, drop khi đầy (không deadlock).
- Không dùng `Target.setAutoAttach`; attach thủ công 1 page target, `flatten:true`.

### 2.3 Snapshot (port 1:1 từ reference, giữ nguyên semantics)

Một `Runtime.evaluate` trả về nguyên tử: `url,title,text(≤6000),actions[≤250],marker,page_key,guards,scroll`.

- `window.__jevFast` cache: `WeakMap<node,id>` + `Map<id,node>` → **node id do code sở hữu**,
  không phải CDP backendNodeId, không phải selector do model sinh.
- `marker` = so sánh ngữ nghĩa toàn trang; `guard(node)` = so sánh cục bộ (form/dialog/row)
  để click không bị stale vì animation không liên quan.
- Chỉ lấy **text visible trong viewport** → context nhỏ → model nhanh.

### 2.4 Execute (an toàn theo thiết kế)

1. `Fresh(page, action)` — guard khớp mới được đi tiếp.
2. Resolve lại geometry ngay trước input: connected, visible, không disabled, tâm nằm trong
   viewport, `elementFromPoint` trúng chính nó (không bị che).
3. Click: `Input.dispatchMouseEvent` pressed/released. Fill: click → ⌘A/^A (`selectAll`) →
   `Input.insertText`. Select: set `.value` + dispatch `input`/`change` trong page.
   Scroll: `mouseWheel` ±560. Wait: 100 ms.
4. Settle sau input: ≤2 rAF hoặc 50 ms; combobox: chờ `[role=option]` visible, cap 200 ms.
5. Ghi history **trước** khi observe lại (navigation không xoá được action đã thực thi).

Model output **không bao giờ** trở thành selector / toạ độ / JS / shell.

### 2.5 Agent loop

```
observe ─► choose(Jev) ─► [TYPE_TEXT? textgen] ─► fresh? ─► act ─► settle ─► observe …
                ▲                                     │ stale
                └─────────────────────────────────────┘ (re-observe, không double-act)
```

- Budget: 60 action / 120 model call. 3 action liên tiếp không đổi trang (không phải WAIT) → `blocked`.
- Text đã sinh được tái sử dụng sau stale **chỉ khi** toàn bộ input helper y hệt.
- `DONE` của model ≠ thành công; CLI in state cuối để người dùng tự kiểm.

### 2.6 Đo tốc độ (điểm chính user muốn thấy)

Mỗi step ghi `Timing{Snapshot, Model, TextGen, Act, Settle}` (ms) + `usage`. Kết thúc in bảng:

```
step  op         target                     snap  model  text   act  settle  total
  1   CLICK      [3] Where to?               4     181     -     9     51     245
  2   TYPE_TEXT  [3] Where to?               3     176   412    12    200     803
...
TOTAL 7 steps  6.9s   jev 1.2s (avg 171ms)  text 0.8s  browser 0.3s  cdp calls: 94
```

`cmd/bench` đo riêng không cần key: RTT `Runtime.evaluate("1")` ×1000 (pipe vs ws), thời gian
snapshot.js trên vài trang, thời gian launch Chrome → cold start.

Tối ưu thêm so với reference:
- Pipe transport (không có trong reference).
- Pre-warm TLS/HTTP2 tới `api.typesafe.ai` + text model **song song** với lúc launch Chrome.
- Reader decode header-only, zero-copy payload.
- Không screenshot trong loop (tuỳ chọn `--trace` mới chụp).

## 3. Config (env)

```
TYPESAFE_API_KEY=        # bắt buộc
TYPESAFE_MODEL=jev-latest
TEXT_MODEL_API_KEY=      # bắt buộc nếu task có TYPE_TEXT
TEXT_MODEL_BASE_URL=https://openrouter.ai/api/v1
TEXT_MODEL=inception/mercury-2.5
TEXT_MODEL_REASONING=none
CHROME_PATH=             # mặc định dò /Applications/Google Chrome.app
```

## 4. Milestones

| # | Việc | Kiểm chứng |
|---|---|---|
| M1 | `ws`, `cdp`, `chrome` (launch **và** attach), `cmd/bench` | RTT in ra, pipe vs ws; attach được Chrome thật |
| M2 | `snapshot`, `browser` + fixture HTML local, test guard/stale | `go test`, `check_guards` |
| M3 | `jev`, `textgen`, `agent`, `cmd/agent` | chạy Wikipedia goal, in bảng timing |
| M4 | README, `--trace` JSON, screenshot tuỳ chọn, benchmark | so số với reference (7.1 s Flights) |

## 5. Giới hạn kế thừa (MVP)

Shadow DOM, iframe, canvas, upload, popup tab, nested scroll, keyboard widget phức tạp — ngoài phạm vi.
Tiếng Anh cho goal (Jev train chủ yếu English).
