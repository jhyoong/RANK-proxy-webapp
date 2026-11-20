# Plan 3 Design: Chat with kagent on the Chosen Cluster

As of 2026-11-20. Builds on Plan 2 (cluster list and kagent access check). Its live-test report is not in yet; this design **assumes Plan 2's tests mostly pass**, in particular L1 (list), L3 (`kagent is ready`, card fetched through Rancher) and L9 (unknown cluster gives 404). If the report contradicts that, revisit the parts marked *(depends on Plan 2)*. The implementation plan written from this design goes in [PLAN3.md](PLAN3.md).

## Intent

On a cluster whose check says `kagent is ready`, the user can **chat with that cluster's kagent agent** from the cluster page. Each message goes to the Rancher site that manages that cluster, through `/k8s/clusters/<id>/`, with the user's own token for that site. This is the same path the agent card fetch already uses. Replies appear as they stream in.

Why: this is the last piece of the base MVP. The portal replaces "open a Rancher shell on the right cluster and run kagent". Plan 4 hardens the portal and Plan 5 polishes the UI, so Plan 3 aims **only to get the chat working** end to end, and to measure the streaming path on site.

As before, the build ends with a short live-test runbook and a report that is typed by hand.

## Facts this design rests on

- All three Rancher sites run v2.13.3. Tokens last 8h.
- Fetching the agent card through Rancher works (checked by hand on site; Plan 2 L3 *(depends on Plan 2)*).
- kagent is v0.9.6, A2A 0.3. The A2A endpoint is `.../proxy/api/a2a/<agent_ns>/<agent_name>/`, with the trailing `/`.
- kagent's streaming timeout on site is **30m** (quick findings).
- The multi-turn bug (#1882) was fixed before v0.9.5, so reusing one `contextId` across messages should work on v0.9.6 (R2, unverified on site).
- **Not yet known (R1):** whether events stream through each site's load balancer and ingress one by one or arrive all at the end, and which timeout cuts off a long or silent investigation. The ingress-nginx default is 60s, and the v2.13 Rancher chart sets 1800s (E3, not checked on site). Plan 3's live test measures this.
- kagent sends no heartbeats while a tool runs. Silences of minutes are normal.
- The portal's own `http.Server` has no `WriteTimeout`, so long responses aren't cut by the portal.
- Almost all users are cluster admins, so the "no access" path still can't be triggered on site.

## Goals

1. On the cluster page, when the outcome is `ready`, a chat panel replaces the disabled placeholder.
2. The user types a message. The portal sends A2A `message/stream` through Rancher and relays the agent's progress and answer to the browser **as they arrive**.
3. Follow-up messages on the same page continue the same conversation (same `contextId`). A **New conversation** button starts a fresh one.
4. A **Stop** button cancels the running reply.
5. Every failure shows a clear message in the chat: session expired, no access, kagent down, cluster unreachable, timed out, or an agent error.
6. After each reply, a small diagnostics line shows time to first event, event count, total time, the final state and the task ID. The live test reads its numbers from here.
7. A live-test runbook and a hand-typed report format, in the style of Plans 1 and 2. The first job of the runbook is to answer R1.

## Non-goals

- **Separate chat history per user (D1).** Deferred (decided 2026-11-20). The portal sends no `X-User-Id` or `user_id`, so kagent files every conversation under its default user (`admin@kagent.dev`). Conversations are separated only by their random `contextId`.
- Chat history that survives a page reload, or a list of past conversations. The conversation lives in the open page only.
- Markdown rendering. Agent text is shown as plain text, with line breaks kept (Plan 5).
- The non-streaming fallback (`message/send` with `blocking=false` plus `tasks/get` polling). It is built only if the live test shows streaming fails (see [If streaming fails on site](#if-streaming-fails-on-site)).
- Cancelling the task inside kagent. Stop only drops the connection.
- Logging message content. The audit line holds metadata only. The content-logging open item is for Plan 4.
- Redis, rate limits, NetworkPolicy, 2 replicas (Plan 4). Visual polish (Plan 5).
- Changing anything in Rancher, RBAC or kagent.

## Anti-goals (failure even if it "works")

- A Rancher token reaching the browser, a page, a log line or the script.
- Any call to a cluster the user wasn't shown in their own cached list.
- Agent output inserted into the page as HTML. It is set with `textContent` only.
- One stuck reply holding a server goroutine forever. Every stream has a total cap and a silence cap.
- A browser script from anywhere except the portal itself. There is no inline script, and the CSP stays strict.
- A state-changing request without the CSRF token.

## Approaches considered

| | Approach | Trade-off | Outcome |
| --- | --- | --- | --- |
| **Browser side** | **A. Small script served by the portal (`/static/chat.js`); CSP `script-src 'self'`** | Live output and a Stop button. The first JavaScript in the portal, but no outside hosts and no inline code. | **Chosen by the user** |
| | B. No JavaScript: a form POST that waits for `message/send` to finish, then renders the page | Nothing appears until the end. A long investigation looks hung and hits every proxy timeout in the chain. | Rejected |
| **Browser ↔ portal** | **A. One `fetch()` POST whose response is a stream of events (read with `response.body.getReader()`)** | One request. The CSRF token travels in a header. No server-side stream registry. The browser can't reconnect automatically, which isn't needed. | **My call** |
| | B. POST the message, get a stream ID, then open `EventSource` (GET) on it (verification D9) | Two requests, plus a per-session registry of pending streams with expiry. EventSource can't send headers. | Rejected as more moving parts for no gain here |
| **User ID to kagent** | **A. Send none; everyone shares kagent's default user** | Simplest. History isn't private, which is already a stated non-goal. | **Chosen by the user** (D1 deferred) |
| | B. A keyed hash of the principal (`HMAC`) | Separates history, but adds the portal's first Secret | Deferred to Plan 4 |
| **Pre-chat check** | **A. Chat only from a cluster page whose check said `ready`; the POST checks only list membership and the connected state** | No extra calls per message. RBAC is still enforced by Kubernetes on every message. | **My call** |
| | B. Re-run the SSAR before every message | One more Rancher call per message, for no added security | Rejected |

## Design

### Configuration (new keys)

```yaml
chat:
  max_duration: 30m        # total cap per reply; matches kagent's streaming timeout on site
  idle_timeout: 10m        # cap on silence between upstream events (kagent sends no heartbeats)
  max_message_bytes: 8192  # longest message a user can send
  heartbeat: 15s           # comment line sent to the browser while upstream is silent
  log_events: false        # log the shape (never content) of every upstream event; on for live tests
```

The keys are validated like the existing ones: unknown keys are rejected, durations must be positive, and `max_message_bytes` must be between 1 and 65536. Every key defaults to the value shown, and `idle_timeout` must not exceed `max_duration`.

### Rancher client (`internal/rancher`)

- A second, **streaming** `http.Client` per site with the same TLS config. It has **no** `Client.Timeout`, because a total timeout would cut the stream (C12). It also doesn't follow redirects. Request lifetime is governed only by the context.
- `PostStream(ctx, bearer, path string, body any) (io.ReadCloser, error)` sends a POST with `Accept: text/event-stream` and `Content-Type: application/json`. A non-2xx answer becomes the existing `*StatusError`, with its truncated body. A 2xx answer returns the body for the caller to read and close. The response must have arrived within `chat.idle_timeout`. This is a context deadline that is lifted once headers arrive, not a transport setting.
- `rancher` knows nothing about A2A. It only performs the POST.

### A2A module (`internal/a2a`, new)

This is the only package that knows kagent's A2A paths and event shapes, so that a kagent 1.x upgrade (C1) only rewrites this package.

- `Path(clusterID string, k config.KagentConfig) string` gives `/k8s/clusters/<id>/api/v1/namespaces/<ns>/services/<service>/proxy/api/a2a/<agent_ns>/<agent_name>/`. It uses the same escaping as `AgentCard`, so the `:` characters stay unescaped and the trailing `/` stays. `AgentCard` keeps its own path code (so `rancher` doesn't import `a2a`), and a test pins that `Path(...) + ".well-known/agent-card.json"` equals the path `AgentCard` requests. *(Builder's call, 2026-11-20.)*
- `StreamRequest(text, contextID string) any` builds the `message/stream` JSON-RPC body with a fresh `messageId` and `id`, `role: "user"`, `kind: "message"` and one text part.
- `Reader` parses an SSE body (lines starting `data:`, multi-line data joined with `\n`, events ending at a blank line, comment lines ignored). It caps a single event at 1 MiB. `Next()` returns the next decoded `Event`.
- `Event` is a portal-side summary of one upstream JSON-RPC message. Parsing is tolerant: unknown kinds and fields are ignored.
  - `Kind`: `status-update`, `artifact-update`, `task`, `message`, or `error` (a JSON-RPC `error` object).
  - `TaskID`, `ContextID`, `State` (e.g. `working`, `completed`, `failed`, `input-required`), `Final`.
  - `Texts []Text`: the text parts, each with a `Partial` flag (`metadata.adk_partial == true`).
  - `Tools []string`: the names of data parts that are function calls (any metadata key ending in `_type` whose value is `function_call`, name from `data.name`).
  - `ErrCode`, `ErrMessage` for JSON-RPC errors.
  - `Shape`: kind, state, part kinds and metadata **keys** (never values). It is used only for the debug log line below.

### Chat service (`internal/chat`, new)

`Send(ctx, sess, row, text, contextID string, emit func(Out))` runs one reply:

1. Find the site token (`ok` status), as `clusters.Check` does. If the site isn't logged in, emit an `expired` error.
2. Build a context capped by `chat.max_duration`. Call `PostStream` with `a2a.Path` and `a2a.StreamRequest`.
3. Map pre-stream errors to the existing messages: 401 → `Your session on <site> has expired: log in again`, 403 → `You don't have kagent access on this cluster`, 404 → `kagent or the agent isn't installed on this cluster`, 503 → `kagent is installed but not running`, 502 or transport → `Cluster unreachable through Rancher`, other → `Unexpected response (HTTP <n>)`.
4. Read events. Each `Next()` gets a fresh `chat.idle_timeout` deadline. The reader runs in a goroutine and the service selects on events, a heartbeat ticker and the deadlines. Emit:
   - `progress {text, partial}` for each text in a non-final `status-update`
   - `tool {name}` for each tool name
   - `answer {text}` for each text in an `artifact-update`, or in a final `status-update` or `message`
   - `state {state}` when the state changes
   - `error {message}` for a JSON-RPC error (`Agent error: <message, truncated to 300 chars>`), or a state of `failed` or `rejected` (`The agent reported the task failed`)
   - `ping` on each heartbeat while upstream is silent
5. The reply ends on a final event, EOF, a JSON-RPC error, the total cap (`Reply took longer than <max_duration> and was stopped`), the silence cap (`No response from the agent for <idle_timeout>; stopped`), or the browser going away. It always finishes with `done {state, task_id, first_event_ms, total_ms, events}`.
6. **Audit log**, one line per reply: `event=chat`, with `site`, `cluster`, `principal`, `context_id`, `task_id`, `http_code`, `first_event_ms`, `total_ms`, `events`, `result` (`completed`, `failed`, `error`, `timeout`, `idle-timeout`, `cancelled`, `expired`, `no-access`, `not-installed`, `not-running`, `unreachable`, `unexpected`) and `rancher_body` on unexpected codes. **No message text and no agent text.**
7. **Debug shapes:** when `chat.log_events: true` (default `false`), one `event=chat_event` line per upstream event, with `context_id`, `n`, `ms` since the start, and the event's `Shape`. This line lets the live test report what kagent actually sends without copying content. It is how the parsing assumptions above are checked.

The text sent to the user is not logged. Agent text never reaches the logs.

### Web (`internal/web`)

**Route:** `POST /clusters/{site}/{id}/chat`

- **Request:** a JSON body `{"text": "...", "context_id": "<uuid>"}`, with the `X-CSRF-Token` header set to the session CSRF token. The body is capped at `max_message_bytes` plus 1 KiB.
- **Checks before streaming**, each answered with a plain HTTP code and no Rancher call:
  - no session → 401
  - bad or missing CSRF → 403
  - `Content-Type` not `application/json` → 415
  - body too large or text too long → 413
  - empty text, or `context_id` not a UUID → 400
  - cluster not in the session's cached list → 404 *(depends on Plan 2's `Find`)*
  - cluster disconnected → 409
  - **another reply already running for this session** → 429
- **One running reply per session.** The server keeps a set of session IDs with an active stream, guarded by a mutex. An ID is added before streaming and removed when the stream ends. Two tabs on one session can't stream at once.
- **Streaming response:** `200`, `Content-Type: text/event-stream`, `X-Accel-Buffering: no`, plus the existing security headers. Each `Out` is written as `event: <type>\ndata: <json>\n\n` and flushed with `http.ResponseController`. `ping` is written as a comment line (`: ping`). The request context is passed down, so closing the tab or pressing Stop cancels the upstream request.

**Cluster page** (`GET /clusters/{site}/{id}`): when the outcome is `ready`, the page renders the chat panel in place of the disabled button:

```
┌ c-abc12 · site-1 ────────────────────────────────────────┐
│ kagent is ready   (agent table, diagnostics as before)    │
│                                                           │
│ You:    Why is pod coredns-x restarting?                  │
│ ·· tool: k8s_get_resources                                │
│ ·· Checking events in kube-system…                        │
│ Agent:  The pod is OOMKilled. Its limit is 170Mi, …       │
│   first event 820 ms · 34 events · 2m13s · completed      │
│   · task 7f3c…                                            │
│                                                           │
│ [ message box                                         ]   │
│ [Send]  [Stop]  [New conversation]                        │
└───────────────────────────────────────────────────────────┘
```

- The page carries `data-chat-url`, `data-csrf` and `data-context-id` (a fresh UUID made by the server on each page load) as data attributes on the panel element. There is no inline script.
- **New conversation** reloads the page, which gives a new `contextId` and a fresh check.
- Other outcomes keep the disabled button, with its text changed to `Chat unavailable: <message>`.

**`/static/chat.js`** (vanilla, no libraries, roughly 150 lines):

- On Send, it disables the input and POSTs with `fetch`. It reads the response body, splits it into events and dispatches them by type.
- Every piece of agent text goes into the DOM through `textContent`. Never through `innerHTML`.
- `progress`: if `partial` is set, append to the current progress line, else start a new one. Progress lines are small and grey.
- `tool`: a small grey `tool: <name>` line.
- `answer`: the main agent block. If `done` arrives without any `answer`, the last progress text is promoted to the answer.
- `error`: a red line with the message.
- `done`: the diagnostics line. Re-enable the input.
- A non-200 answer to the POST maps to a short message by code: 401 → `Session expired: log in again` (with a link to `/`), 403 → `Page expired: reload`, 404/409 → `Cluster no longer available: reload`, 413 → `Message too long`, 429 → `A reply is already running in another tab`, other → `Request failed (HTTP <n>)`.
- **Stop** calls `AbortController.abort()`, and the line `Stopped` is shown.
- Ctrl+Enter or Cmd+Enter sends the message.

**CSP** becomes `default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'`. The header tag changes from `cluster list test` to `chat test`.

### Fake Rancher and local run

`ranchertest.Fake` gains a fake A2A endpoint under the k8s proxy path: `POST .../proxy/api/a2a/<ns>/<name>/`. It does the following:

- It checks that `method` is `message/stream`, that the `contextId` is present, and that there is exactly one text part.
- It records the last request body for tests.
- It answers per cluster with a **script**: a list of `{Delay, Data}` steps written as SSE, or a status code, or a hang. Default script: `working` status-update → a tool function-call data part → two partial text chunks → an artifact-update with the full answer → a final `completed` status-update. The text echoes the user's message and the turn number for that `contextId`, so multi-turn can be seen working in the fake.
- It returns 403 for users in the cluster's `Deny` map. This is the no-access path, which can't be triggered on site.

`cmd/fakerancher` seeds the default script on ready clusters, plus these:

- one cluster whose reply has a 20-second silent gap (to watch heartbeats)
- one that sends a JSON-RPC error
- one that ends with `failed`
- one that hangs forever (to see the idle cap with a short configured `idle_timeout`)

### Live test (`PLAN3-live-test.md`)

The live test uses the same style as Plans 1 and 2: a "before you start" checklist, a test table and a short report format. Set `chat.log_events: true` for the run. Use test clusters, one long test at a time per site. The tests, roughly:

- **C1 Streaming, per site (R1).** Ask a short question on one ready cluster per site. Do lines appear one by one, or all at once at the end? Record first-event ms, events and total time for each site.
- **C2 Multi-turn (R2).** Ask a follow-up that only makes sense with the previous answer ("and the second one?"). Does the agent remember? The agent pod log should show no `SessionNotFoundError`.
- **C3 Long, silent investigation (R1).** Ask something that makes the agent run tools for over 2 minutes, ideally with a single tool call longer than 60s. Does it complete? If it's cut off, record when and the error text. This is what finds a 60s ingress or load balancer timeout.
- **C4 Stop.** Stop a running reply. The chat shows `Stopped`, the input comes back, and the next message works.
- **C5 Two tabs.** Send in two tabs at once. The second shows `A reply is already running in another tab`.
- **C6 New conversation.** The agent no longer knows the previous answer.
- **C7 kagent down or missing** (if any). The cluster page doesn't offer chat.
- **C8 Expired site token** (optional). Log out at Rancher for one site in another tab, then send. The chat shows the expired message for that site only.
- **C9 Event shapes.** From the `event=chat_event` lines of one C1 reply, write down the distinct shapes (kind, state, part kinds, metadata keys). This confirms or corrects the parsing in this design.

The report records, per site: first-event ms, total ms, events, whether streaming was live (`y`/`n`), and the C3 outcome with the cut-off time if there was one.

### If streaming fails on site

C1 and C3 decide this:

| Observed | Meaning | Next step (a follow-up plan, not Plan 3) |
| --- | --- | --- |
| Everything arrives at once at the end | Something in the chain buffers (ingress `proxy-buffering`, or a load balancer) | Ask the Rancher site owners to disable buffering for `/k8s/clusters/` (E3). The portal side already sends `X-Accel-Buffering: no` towards the browser. |
| Cut off at ~60s, or at a load balancer idle value | Ingress or load balancer read timeout | Raise the timeout on that site, or build the non-blocking `message/send` + `tasks/get` fallback |
| Cut off at 30m | The kagent cap, as expected | None. It matches `chat.max_duration`. |

## Left to the builder

- The exact SSE line handling edge cases (CRLF, `event:` and `id:` fields, which are ignored).
- How the reader goroutine and the select loop are structured, as long as the goroutine always exits when the context is cancelled and the body is closed.
- How chat.js splits the stream (a buffer split on `\n\n`) and its CSS, kept minimal.
- UUID generation on the server (`crypto/rand`, version 4 format) and its validation regex.
- Test names and the file split within the packages.

## Repo rules (unchanged)

The implementer stages changes; **the user makes every commit and push**. No secrets in the repo. The example config uses placeholder hosts and names. Image tag for the live test: `v0.3.0`.
