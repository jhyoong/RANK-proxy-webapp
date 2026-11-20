# Plan 3: Chat with kagent Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On a cluster page whose check says `kagent is ready`, the user chats with that cluster's kagent agent over A2A `message/stream`, sent through Rancher with the user's own token. Replies are relayed live to the browser. A live-test runbook that answers R1 (streaming through Rancher) goes with it.

**Architecture:** `rancher.Client` gains a streaming POST with no total timeout. A new `internal/a2a` package owns the kagent A2A path, the request body, an SSE reader and tolerant event parsing. A new `internal/chat` package runs one reply: it posts, reads events under a total cap and a silence cap, turns them into a small set of browser events, and writes one audit line. `internal/web` adds `POST /clusters/{site}/{id}/chat`, which streams `text/event-stream` back to a small vanilla script, `/static/chat.js`, served by the portal.

**Tech Stack:** Go 1.27.4 standard library, `go.yaml.in/yaml/v3`, and plain browser JavaScript (no libraries). The distroless image and CI are the same as Plans 1 and 2.

**Spec:** [PLAN3-design.md](PLAN3-design.md). Read [PLAN2.md](PLAN2.md) for the code it builds on, and [kagent-chat-via-rancher.md](../kagent-chat-via-rancher.md) for the kagent event shapes and the timeouts along the path.

## Global Constraints

- Every Rancher call uses the logged-in user's own token for that site. The portal holds no credentials.
- A2A endpoint: `POST <site>/k8s/clusters/<id>/api/v1/namespaces/<kagent.namespace>/services/<kagent.service>/proxy/api/a2a/<kagent.agent_namespace>/<kagent.agent_name>/`. The trailing `/` is required, and the `:` characters in `http:kagent-controller:8083` must reach Rancher unescaped.
- Method `message/stream` only, A2A 0.3 body: `{"jsonrpc":"2.0","id":"<uuid>","method":"message/stream","params":{"message":{"kind":"message","messageId":"<uuid>","contextId":"<uuid>","role":"user","parts":[{"kind":"text","text":"..."}]}}}`.
- **No `X-User-Id` header and no `user_id` query parameter** are sent to kagent (D1 deferred).
- The chat POST only reaches Rancher for a cluster that is in the session's cached list and connected. An unknown `{site}/{id}` gets 404 and no Rancher call.
- At most **one running reply per session**. A second one gets 429.
- Agent text goes into the DOM only through `textContent`. No `innerHTML`, no inline script, and no third-party hosts.
- CSP: `default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'`.
- Never log or render a token. Never log message text or agent text. Log lines: `event=chat` (per reply) and `event=chat_event` (per upstream event, only when `chat.log_events`), with only the fields listed in Task 5.
- Repo rule: the implementer stages changes. **The user makes every commit and push.** No secrets. The example config uses placeholders.

**Config defaults:** `chat.max_duration 30m`, `chat.idle_timeout 10m`, `chat.max_message_bytes 8192`, `chat.heartbeat 15s`, `chat.log_events false`.

**Exact user-facing copy (chat `error` events, from `internal/chat`):**

| Cause | Text |
| --- | --- |
| site not logged in, or 401 | `Your session on <site> has expired: log in again` |
| 403 | `You don't have kagent access on this cluster` |
| 404 | `kagent or the agent isn't installed on this cluster` |
| 503 | `kagent is installed but not running` |
| 502 or transport error | `Cluster unreachable through Rancher` |
| other HTTP code | `Unexpected response (HTTP <n>)` |
| JSON-RPC error | `Agent error: <message, max 300 chars>` |
| state `failed` or `rejected` | `The agent reported the task failed` |
| final state `input-required` | `The agent is asking for approval, which the portal doesn't support yet` |
| stream ended (EOF) before any `final: true` event | `The connection to the agent was cut before the reply finished` |
| unparseable or oversized event | `Unexpected event from the agent` |
| total cap | `Reply took longer than <max_duration> and was stopped` |
| silence cap | `No response from the agent for <idle_timeout>; stopped` |

Durations are formatted with `time.Duration.String()` (e.g. `30m0s`).

**Browser copy (in chat.js, for non-200 responses to the POST):** 401 → `Session expired: log in again` (with a link to `/`); 403 → `Page expired: reload`; 404 or 409 → `Cluster no longer available: reload`; 413 → `Message too long`; 429 → `A reply is already running in another tab`; other → `Request failed (HTTP <n>)`. Stop → `Stopped`.

## Review Focus

1. **The user closes the tab or presses Stop mid-reply.** The upstream request is cancelled, the reader goroutine exits, and the session's running-reply slot is freed, so the next send is not answered with 429. Tests: `TestSend_CancelStopsUpstream` (Task 5), `TestChat_CancelFreesSlot` (Task 6).
2. **The agent goes silent** during a long tool call. Heartbeats keep flowing to the browser while the silence is shorter than `idle_timeout`, and the reply ends with the silence-cap message when it is longer. Tests: `TestSend_HeartbeatDuringSilence`, `TestSend_IdleTimeout` (Task 5).
3. **Agent text that contains newlines, `\n\nevent: done`, or HTML.** It must not break SSE framing or reach the DOM as markup. Tests: `TestWriteEvent_FramingSafe` (Task 6). Code review of `chat.js`: only `textContent`.
4. **One site's token expires mid-session.** The chat shows the expired message for that site, and the portal session and other sites keep working. Test: `TestSend_PreStreamCodes` 401 row (Task 5), `TestChat_401KeepsSession` (Task 6).
5. **kagent sends something unexpected:** a malformed JSON event, an event over 1 MiB, or an unknown kind. The reply ends with `Unexpected event from the agent` for the first two, ignores the third, and always ends with `done`. Tests: `TestReader_Oversized`, `TestParse_UnknownKindIgnored` (Task 3), `TestSend_MalformedEvent` (Task 5).
6. **Text appears exactly once.** The `submitted` echo of the user's message is not shown as agent output. A complete step replaces its partial chunks rather than adding to them, and an answer that repeats the last step replaces that line. Tests: `TestParse_UserEcho` (Task 3), `TestSend_DefaultScript` (Task 5). Code review of `chat.js`.
7. **A stream cut by a timeout in the chain** (EOF with no final event) is reported as `cut-off`, never as `completed`. Test: `TestSend_EOFWithoutFinal` (Task 5).

---

## File Structure

| Path | Responsibility |
| --- | --- |
| `internal/config/config.go` | `chat` section |
| `internal/rancher/client.go`, `internal/rancher/stream.go` | Streaming HTTP client and `PostStream` |
| `internal/a2a/a2a.go` | `Path`, `StreamRequest`, `NewUUID`, `ValidUUID` |
| `internal/a2a/events.go` | SSE `Reader`, `Event`, parsing |
| `internal/rancher/ranchertest/fake.go`, `internal/rancher/ranchertest/a2a.go` | Fake A2A streaming endpoint |
| `internal/chat/chat.go` | `Service.Send`: one reply, caps, mapping, audit log |
| `internal/web/server.go`, `internal/web/chat.go` | Chat route, SSE writer, one-reply-per-session, CSP |
| `internal/web/templates/cluster.html`, `layout.html`, `static/chat.js`, `static/style.css` | Chat panel and script |
| `cmd/portal/main.go` | Wiring |
| `cmd/fakerancher/main.go`, `config.example.yaml`, `deploy/portal.yaml`, `README.md` | Seeds, examples, docs |
| `docs/plans/PLAN3-live-test.md` | On-site runbook and report format |

Run every test with `go test -race ./...`.

---

### Task 1: Config `chat` section

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`, `config.example.yaml`

**Interfaces:**
- Produces:
  ```go
  // added to Config
  Chat ChatConfig `yaml:"chat"`

  type ChatConfig struct {
      MaxDuration     time.Duration `yaml:"max_duration"`      // default 30m
      IdleTimeout     time.Duration `yaml:"idle_timeout"`      // default 10m
      MaxMessageBytes int           `yaml:"max_message_bytes"` // default 8192
      Heartbeat       time.Duration `yaml:"heartbeat"`         // default 15s
      LogEvents       bool          `yaml:"log_events"`        // default false
  }
  ```

- [ ] **Step 1: Write failing tests.**
  - `TestLoad_ChatDefaults`: the minimal fixture gives the five defaults above.
  - Add rows to `TestLoad_Rejects`: negative `max_duration`, `idle_timeout` or `heartbeat`; `max_message_bytes: 70000`; `max_message_bytes: -1`; `idle_timeout: 40m` with `max_duration: 30m`; an unknown key `chat: {mode: send}`.
- [ ] **Step 2:** Run `go test ./internal/config/` and expect FAIL.
- [ ] **Step 3:** Implement the defaults in `applyDefaults` (only when zero) and the checks in `validate`. `max_message_bytes` must be between 1 and 65536 after defaults, and `idle_timeout <= max_duration`.
- [ ] **Step 4:** Add a commented `chat:` block to `config.example.yaml`, with the comments from the design's config block. The existing example test must still pass.
- [ ] **Step 5:** Run `go test -race ./...` and expect PASS.
- [ ] **Step 6:** Stage the files and ask the user to commit: `feat(config): chat settings`.

---

### Task 2: Streaming POST and A2A request basics

**Files:**
- Create: `internal/rancher/stream.go`, `internal/rancher/stream_test.go`, `internal/a2a/a2a.go`, `internal/a2a/a2a_test.go`
- Modify: `internal/rancher/client.go`

**Interfaces:**
- Produces:
  ```go
  // rancher
  // NewClient also builds c.stream: same Transport (TLS), no Timeout, same CheckRedirect.
  // PostStream POSTs body as JSON with Accept: text/event-stream. Non-2xx -> *StatusError
  // (body truncated to 200 bytes, response closed). 2xx -> the open body; caller closes it.
  // headerTimeout bounds the wait for response headers only (0 = none).
  func (c *Client) PostStream(ctx context.Context, bearer, path string, body any, headerTimeout time.Duration) (io.ReadCloser, error)

  // a2a
  func Path(clusterID string, k config.KagentConfig) string // ends in "/"
  func StreamRequest(text, contextID string) any            // message/stream body, fresh id + messageId
  func NewUUID() string                                      // v4, crypto/rand
  func ValidUUID(s string) bool                              // 8-4-4-4-12 lowercase hex
  ```
- Header timeout: start a timer that cancels a derived context if headers haven't arrived within `headerTimeout`. Stop the timer once `Do` returns. The returned body stays tied to the caller's `ctx` only, so cancelling `ctx` later still aborts the read.

- [ ] **Step 1: Write failing tests** (plain `httptest` servers):
  - `TestPostStream_SendsHeadersAndBody`: the server sees `POST`, `Authorization: Bearer tok`, `Accept: text/event-stream`, `Content-Type: application/json` and the JSON body. The test reads 3 flushed chunks written 50ms apart, **receiving the first before the server writes the last** (proves there is no buffering).
  - `TestPostStream_NoTotalTimeout`: client built with `NewClient(site, 100*time.Millisecond)`; the server streams for 400ms and the read succeeds.
  - `TestPostStream_StatusError`: 403 with a body gives `*StatusError{Code:403}`.
  - `TestPostStream_HeaderTimeout`: the server sleeps 2s before headers; `headerTimeout` 100ms returns an error in under 1s.
  - `TestPostStream_CtxCancelAbortsRead`: cancel ctx while blocked in `Read`; `Read` returns an error in under 1s.
  - `TestPostStream_NoRedirect`: a 307 gives `*StatusError{Code:307}`.
  - `TestPath`: for cluster `c-abc12` and the default kagent config with `AgentName "my-agent"` it equals `/k8s/clusters/c-abc12/api/v1/namespaces/kagent/services/http:kagent-controller:8083/proxy/api/a2a/kagent/my-agent/`.
  - `TestPath_MatchesCardPath`: `Path(...)+".well-known/agent-card.json"` equals `ranchertest.Fake.LastCardPath()` after an `AgentCard` call.
  - `TestStreamRequest`: marshalled, it has `method=="message/stream"`, `params.message.role=="user"`, `kind=="message"`, the given `contextId`, one part `{kind:text,text:...}`, and `ValidUUID` ids. Two calls give different `messageId`s.
  - `TestValidUUID`: `NewUUID()` passes; `""`, `"x"`, uppercase and `../` fail.
- [ ] **Step 2:** Run `go test ./internal/rancher/ ./internal/a2a/` and expect FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test -race ./...` and expect PASS.
- [ ] **Step 5:** Stage the files and ask the user to commit: `feat(a2a): streaming POST through Rancher and A2A request`.

---

### Task 3: SSE reader and event parsing

**Files:**
- Create: `internal/a2a/events.go`, `internal/a2a/events_test.go`

**Interfaces:**
- Produces:
  ```go
  type Text struct{ Text string; Partial bool } // Partial: part or message metadata kagent_adk_partial or adk_partial == true
  type Event struct {
      Kind               string // "status-update","artifact-update","task","message","error"; "" if unknown
      TaskID, ContextID  string
      Role               string // role of status.message (or of the message), "" if none
      State              string // status.state, if any
      Final              bool
      Texts              []Text
      Tools              []string // function-call data part names
      ErrCode            int
      ErrMessage         string
      Shape              string // e.g. "status-update state=working final=false parts=text,data meta=kagent_adk_partial,kagent_type"
  }
  var ErrEventTooLarge = errors.New("a2a: event exceeds 1 MiB")
  var ErrBadEvent = errors.New("a2a: event is not valid JSON-RPC")
  func NewReader(r io.Reader) *Reader
  func (r *Reader) Next() (Event, error) // io.EOF at clean end
  ```
- SSE: lines split on `\n` (strip a trailing `\r`). `data:` lines are joined with `\n` (one optional leading space stripped after the colon). A blank line ends an event. Lines starting with `:` and the fields `event:`, `id:` and `retry:` are ignored. Events with no data are skipped. Use a `bufio.Reader` and count bytes per event, returning `ErrEventTooLarge` above 1 MiB.
- JSON-RPC: `{"jsonrpc","id","result":{...}}` or `{"error":{"code","message"}}`. `result.kind` selects the shape:
  - `status-update`: `taskId`, `contextId`, `status.state`, `status.message.parts`, `final`.
  - `artifact-update`: `taskId`, `contextId`, `artifact.parts`.
  - `task`: `id`, `contextId`, `status.state`, `status.message.parts`.
  - `message`: `contextId`, `parts`. Treat as final.
- Parts:
  - `kind:"text"` → `Texts`.
  - `kind:"data"` → a tool name when any metadata key ending in `_type` has the value `function_call`, taking the name from `data.name`.
  - Other data parts, including `function_response`, are ignored.
- The partial flag is read from `kagent_adk_partial` (Python runtime) or `adk_partial` (Go runtime), in the metadata of the part or of the enclosing message.
- `Role` comes from `status.message.role`, or from `role` for a `message`.
- `Shape` lists metadata **keys only**, sorted and de-duplicated, never values.

- [ ] **Step 1: Write failing tests** (inline SSE fixtures):
  - `TestReader_Basic`: two events separated by a blank line, with a `: ping` comment between them, give 2 events.
  - `TestReader_MultiLineDataAndCRLF`: `data: {"a":\r\ndata: 1}\r\n\r\n` decodes.
  - `TestReader_Oversized`: one `data:` line of 1 MiB + 1 gives `ErrEventTooLarge`.
  - `TestReader_EOF`: an empty stream gives `io.EOF`. A final event with no trailing blank line is still returned, then `io.EOF`.
  - `TestParse_StatusUpdateWithTextAndTool`: `Kind`, `TaskID`, `State=="working"`, `Texts[0].Text`, `Tools==["k8s_get_resources"]` (metadata `{"kagent_type":"function_call"}`), and `Shape` contains `meta=kagent_type` and no values.
  - `TestParse_PartialText` (table):
    - `kagent_adk_partial:true` and `adk_partial:true` on the message metadata both give `Partial==true`;
    - the same keys on the part do too;
    - `false` or missing gives `false`.
  - `TestParse_UserEcho`: a `submitted` status-update whose `status.message` has `role:"user"` gives `Role=="user"` and its text.
  - `TestParse_ArtifactUpdate`, `TestParse_FinalCompleted` (`Final==true`, `State=="completed"`), `TestParse_Message` (`Final==true`).
  - `TestParse_JSONRPCError`: gives `Kind=="error"`, `ErrCode`, `ErrMessage`.
  - `TestParse_UnknownKindIgnored`: `{"result":{"kind":"something"}}` gives `Kind==""` and no error.
  - `TestParse_BadJSON`: gives `ErrBadEvent`.
- [ ] **Step 2:** Run `go test ./internal/a2a/` and expect FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test -race ./...` and expect PASS.
- [ ] **Step 5:** Stage the files and ask the user to commit: `feat(a2a): SSE reader and tolerant event parsing`.

---

### Task 4: Fake A2A endpoint

**Files:**
- Create: `internal/rancher/ranchertest/a2a.go`, `internal/rancher/ranchertest/a2a_test.go`
- Modify: `internal/rancher/ranchertest/fake.go` (route the POST from `k8sProxy`; add the fields)

**Interfaces:**
- Produces:
  ```go
  type Step struct {
      Delay time.Duration
      Data  string // one SSE data payload (JSON); "{{text}}" and "{{turn}}" are substituted
      Raw   string // if set, written verbatim instead of "data: <Data>\n\n"
  }
  // added to FakeCluster:
  ChatStatus int    // if non-zero, the A2A POST returns this code
  ChatSteps  []Step // nil -> DefaultChat()
  ChatHang   bool   // send headers, then block until the request ends

  func DefaultChat() []Step // submitted echo -> working -> tool call -> 2 partial texts -> complete step -> artifact -> final completed
  // added to Fake:
  func (f *Fake) ChatCalls() int
  func (f *Fake) LastChatBody() map[string]any
  func (f *Fake) LastChatHeaders() http.Header
  func (f *Fake) ChatCancelled() int // count of chat requests whose context ended before all steps were sent
  ```
- Routing: `POST` on the A2A path (the card path's segments minus `.well-known/agent-card.json`, ending in `/`). It returns 403 if the user is in the cluster's `Deny`, else `ChatStatus` if set. Otherwise it checks that `method=="message/stream"`, the `contextId` is non-empty and there is exactly one text part (else 400). It then writes `Content-Type: text/event-stream` and each step after its `Delay`, flushing after each, and stops early when `r.Context()` is done. `{{text}}` is the user's text (JSON-escaped). `{{turn}}` counts messages per `contextId` (1, 2, ...).
- `DefaultChat()` uses the kagent shapes seen in the test lab (see `kagent-chat-via-rancher.md` §3). It has 8 events, each with a 20ms delay:
  1. A `status-update` with `state: submitted` whose `status.message` has `role: "user"` and the text `{{text}}`.
  2. A `status-update` with `state: working`, no message, and `metadata: {kagent_session_id, kagent_user_id}`.
  3. A `working` status-update whose message (`role: "agent"`, `metadata: {kagent_adk_partial: false}`) has the data part `{kind:data, data:{id:"call_1", name:"k8s_get_resources", args:{}}, metadata:{kagent_type:"function_call"}}`.
  4. The partial chunk `Answer to "{{text}}"`, in a `working` status-update whose message metadata has `kagent_adk_partial: true`.
  5. The partial chunk ` (turn {{turn}})`, sent the same way.
  6. The complete step: a `working` status-update with `kagent_adk_partial: false` and the full text `Answer to "{{text}}" (turn {{turn}})`.
  7. An `artifact-update` with the same full text.
  8. A final `status-update` with `state: completed` and `final: true`.

- [ ] **Step 1: Write failing tests:**
  - `TestFakeChat_DefaultStreams`: a raw HTTP POST through `NewTLS` reads 8 SSE events, the artifact contains the text and `turn 1`, and a second POST with the same `contextId` says `turn 2`.
  - `TestFakeChat_Deny403`, `TestFakeChat_Status` (`ChatStatus: 503`), `TestFakeChat_BadMethod400`.
  - `TestFakeChat_HangCancelled`: with `ChatHang`, cancelling the client ctx makes `ChatCancelled()==1` within 1s.
- [ ] **Step 2:** Run `go test ./internal/rancher/...` and expect FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test -race ./...` and expect PASS.
- [ ] **Step 5:** Stage the files and ask the user to commit: `test(ranchertest): fake A2A streaming endpoint`.

---

### Task 5: Chat service

**Files:**
- Create: `internal/chat/chat.go`, `internal/chat/chat_test.go`

**Interfaces:**
- Consumes: `rancher.Client.PostStream`, `a2a.Path`, `a2a.StreamRequest`, `a2a.NewReader`, `a2a.Event`, `clusters.Row`, `auth.SiteResult`, `config.ChatConfig`, `config.KagentConfig`.
- Produces:
  ```go
  type Out struct {
      Type string // "progress","tool","answer","state","error","ping","done"
      Data any    // nil for ping; see below
  }
  // Data shapes (JSON field names):
  // progress {text, partial}   tool {name}   answer {text}   state {state}   error {message}
  // done {state, task_id, first_event_ms, total_ms, events}
  type Service struct{ /* clients by site name, kagent, chat cfg, log, now */ }
  func New(clients []*rancher.Client, k config.KagentConfig, c config.ChatConfig, log *slog.Logger) *Service
  func (s *Service) Send(ctx context.Context, sites []auth.SiteResult, row clusters.Row, principal, text, contextID string, emit func(Out))
  ```
- `Send` always emits exactly one `done` last, and returns only after the reader goroutine has exited and the body is closed.
- Flow: find the site's `ok` token (else an `expired` error, then done, with no call). Derive `ctx` with `MaxDuration`. Call `PostStream(..., headerTimeout = IdleTimeout)`. Map pre-stream errors with the copy table. Then loop:
  - A goroutine calls `Next()` and sends results on a channel.
  - The main loop selects on: the event channel; a `Heartbeat` ticker (emit `ping` only if no event arrived since the last tick); an idle timer reset on every event (`IdleTimeout`); and `ctx.Done()`.
- Event mapping:
  - texts of an event with `Role=="user"` (the `submitted` echo) are skipped. Its state still counts.
  - non-final `status-update`: each text → `progress` (with its `Partial` flag); each tool → `tool`.
  - final `status-update`, `message` or `artifact-update`: each text → `answer`. Tools in any event → `tool`.
  - Emit `state` when `State` is non-empty and differs from the last one.
  - `failed`/`rejected` → error `The agent reported the task failed`.
  - final `input-required` → error `The agent is asking for approval, which the portal doesn't support yet`.
  - `Kind=="error"` → `Agent error: ...`, then stop.
  - `ErrBadEvent`/`ErrEventTooLarge` → `Unexpected event from the agent`, then stop.
  - Stop on `Final` or `io.EOF`. `io.EOF` before any `Final` event → error `The connection to the agent was cut before the reply finished`.
- `result` values for the audit line: `completed`, `failed`, `input-required`, `error`, `cut-off` (EOF before a final event), `timeout`, `idle-timeout`, `cancelled` (the parent ctx was cancelled, i.e. browser gone or Stop), `expired`, `no-access`, `not-installed`, `not-running`, `unreachable`, `unexpected`. `done.state` is the last state seen, whatever the result.
- Log `event=chat` once per call (Info), with `site`, `cluster`, `principal`, `context_id`, `task_id`, `http_code`, `first_event_ms`, `total_ms`, `events`, `result`, and `rancher_body` for codes other than 401/403/404. When `LogEvents`, log `event=chat_event` per upstream event with `context_id`, `n`, `ms` and `shape`.

- [ ] **Step 1: Write failing tests** (fake through `ranchertest.NewTLS`; collect `Out`s in a slice; a `syncBuf` logger as in `clusters_test.go`):
  - `TestSend_DefaultScript`: the types in order are:
    1. `state(submitted)`
    2. `state(working)`
    3. `tool`
    4. `progress(partial)`
    5. `progress(partial)`
    6. `progress(complete, full text)`
    7. `answer`
    8. `state(completed)`
    9. `done`

    No `progress` or `answer` equals the bare user text, so the echo is skipped. The answer contains the text, and `done.state=="completed"`. The fake's last request has **no** `X-User-Id` header and no `user_id` query parameter.
  - `TestSend_MultiTurnSameContext`: two `Send`s with one `contextID` give answers with `turn 1` then `turn 2`.
  - `TestSend_PreStreamCodes` (table): 401/403/404/503/502/418 give the exact copy, then `done`. The 401 row's message names the site.
  - `TestSend_SiteNotLoggedIn`: an `expired` error, `ChatCalls()==0`.
  - `TestSend_JSONRPCError`, `TestSend_FailedState`, `TestSend_InputRequired`: exact copy, then done. The log has `result=error`, `failed` and `input-required` respectively.
  - `TestSend_EOFWithoutFinal`: a script with only the `submitted` and `working` steps (no final) gives `The connection to the agent was cut before the reply finished`, then `done` with `state=="working"`. The log has `result=cut-off`.
  - `TestSend_MalformedEvent`: a step with `Raw: "data: {not json\n\n"` gives `Unexpected event from the agent`, then done.
  - `TestSend_IdleTimeout`: `IdleTimeout 200ms`, `ChatHang`: `No response from the agent for 200ms; stopped`, done within 1s, and the fake sees the cancellation.
  - `TestSend_TotalTimeout`: `MaxDuration 300ms`, steps every 100ms for 2s: `Reply took longer than 300ms and was stopped`.
  - `TestSend_HeartbeatDuringSilence`: `Heartbeat 50ms`, one step with a 300ms delay: at least 3 `ping`s before the answer, no error.
  - `TestSend_CancelStopsUpstream`: cancel the parent ctx after the first event. `Send` returns within 1s, the last `Out` is `done`, `result=cancelled` is in the log, and `ChatCancelled()==1`.
  - `TestSend_LogFields`: the `event=chat` line has every listed key and contains neither the bearer, the user's text nor the answer text. With `LogEvents`, there are 8 `event=chat_event` lines and none contains the answer text.
- [ ] **Step 2:** Run `go test ./internal/chat/` and expect FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test -race ./...` and expect PASS. Run the chat tests with `-count=20` to shake out timing flakes.
- [ ] **Step 5:** Stage the files and ask the user to commit: `feat(chat): relay one A2A reply with caps and audit log`.

---

### Task 6: Chat route, page, script and wiring

**Files:**
- Create: `internal/web/chat.go`, `internal/web/static/chat.js`
- Modify: `internal/web/server.go`, `internal/web/server_test.go`, `internal/web/templates/cluster.html`, `internal/web/templates/layout.html`, `internal/web/static/style.css`, `cmd/portal/main.go`

**Interfaces:**
- Consumes: `chat.Out`, `chat.Service.Send`, `a2a.NewUUID`, `a2a.ValidUUID`, `clusters.Listing.Find`.
- Produces:
  ```go
  type Chat interface {
      Send(ctx context.Context, sites []auth.SiteResult, row clusters.Row, principal, text, contextID string, emit func(chat.Out))
  }
  func New(cfg *config.Config, a Authn, c Clusters, ch Chat, store *session.Store, version string, log *slog.Logger) (http.Handler, error)
  func writeEvent(w io.Writer, o chat.Out) error // "event: <type>\ndata: <json>\n\n"; ping -> ": ping\n\n"
  ```
- `POST /clusters/{site}/{id}/chat`: checks run in this order, and each one answers without calling `Send`:
  1. no session → 401
  2. `X-CSRF-Token` mismatch → 403 (constant-time compare)
  3. `Content-Type` not `application/json` (media type, params ignored) → 415
  4. body over `MaxMessageBytes+1024` (`MaxBytesReader`) or `len(text) > MaxMessageBytes` → 413
  5. bad JSON, `strings.TrimSpace(text)==""` or `!ValidUUID(context_id)` → 400
  6. `listing(r, sess).Find` misses → 404
  7. `!row.Connected` → 409
  8. session already in the server's `active map[string]bool` (mutex) → 429
- Then: mark the session active and `defer` the delete. Set `Content-Type: text/event-stream`, `X-Accel-Buffering: no`, status 200. Call `Send` with `r.Context()`, writing each `Out` with `writeEvent` then `http.NewResponseController(w).Flush()`.
- `writeEvent` writes JSON with `json.Marshal` (which escapes `\n`, `<` and `>`), so the data is always one line.
- The cluster page, when `Check.Outcome=="ready"`, renders `<section id="chat" data-chat-url="/clusters/{site}/{id}/chat" data-csrf="..." data-context-id="<a2a.NewUUID()>">`. Inside it: a `#log` div, a `<textarea id="msg" maxlength="<MaxMessageBytes>">`, and the buttons `#send`, `#stop` (disabled) and `#new` (a link to the same page). After the panel, `<script src="/static/chat.js" defer></script>`. Other outcomes render `<button disabled>Chat unavailable: {{.Check.Message}}</button>`.
- `layout.html`: the tag text becomes `chat test`. The `csp` const becomes the Global Constraints value.
- `chat.js` behaviour is as in the design's "/static/chat.js" section and the Global Constraints browser copy. In particular:
  - a complete (non-partial) `progress` replaces the open partial line instead of starting a new one;
  - an `answer` whose text equals the most recent progress line removes that line. CSS classes: `.you`, `.agent`, `.progress`, `.tool`, `.err`, `.diag`.
- `main.go`: `chat.New(apiClients, cfg.Kagent, cfg.Chat, log)`, passed to `web.New`. Update the package comment.

- [ ] **Step 1: Write failing tests** (a `stubChat` that records calls and emits scripted `Out`s; it can block on `ctx.Done()`; use `httptest.NewServer` where real streaming is needed):
  - `TestChat_StreamsEvents`: a valid POST gives 200, `text/event-stream`, and a body containing `event: answer` and `event: done` in order. `stubChat` saw the right row, principal, text and contextID.
  - `TestChat_Rejects` (table, `Send` calls == 0 in every row): no cookie → 401; bad CSRF → 403; `text/plain` → 415; text of `MaxMessageBytes+1` bytes → 413; whitespace-only text → 400; `context_id "abc"` → 400; unknown cluster → 404; `local` → 404; disconnected row → 409.
  - `TestChat_OneReplyPerSession`: the first POST blocks in `stubChat` (real server). A second POST on the same session gives 429. A POST on a different session gives 200.
  - `TestChat_CancelFreesSlot`: the first POST's client ctx is cancelled; `stubChat` sees `ctx.Done()`, and a new POST then gives 200, not 429.
  - `TestChat_401KeepsSession`: `stubChat` emits the expired error; afterwards `GET /home` still gives 200.
  - `TestWriteEvent_FramingSafe`: an `answer` with text `"a\n\nevent: done\n<script>"` produces exactly one `data:` line, containing no raw `<` and no raw newline, and the output ends with `\n\n`. `ping` produces `: ping\n\n`.
  - `TestCluster_ReadyHasChatPanel`: the page has `id="chat"`, `data-csrf` equal to the session CSRF token, a `data-context-id` that passes `ValidUUID`, `src="/static/chat.js"`, and no `<script>` with inline content. Two loads give different context IDs.
  - `TestCluster_NotReadyNoChat`: a `not-running` outcome gives `Chat unavailable: kagent is installed but not running` and no `id="chat"`.
  - `TestSecurityHeaders`: updated for the new CSP.
  - `TestStaticAndHealth`: `/static/chat.js` gives 200 with a JavaScript content type.
  - `TestChatJS_NoInnerHTML`: the embedded `chat.js` source doesn't contain `innerHTML`, `outerHTML`, `insertAdjacentHTML`, `eval(` or `document.write`.
- [ ] **Step 2:** Run `go test ./internal/web/` and expect FAIL.
- [ ] **Step 3:** Implement the route, the template, the script, the CSS and the wiring.
- [ ] **Step 4:** Run `go test -race ./...` and `go vet ./...` and expect PASS and clean output.
- [ ] **Step 5:** Stage the files and ask the user to commit: `feat(web): chat panel streaming kagent replies`.

---

### Task 7: Fake Rancher seed, examples and docs

**Files:**
- Modify: `cmd/fakerancher/main.go`, `config.example.yaml` (if not done in Task 1), `deploy/portal.yaml`, `README.md`

- [ ] **Step 1:** In `cmd/fakerancher`, ready clusters get `DefaultChat()` with 300ms step delays, so streaming is visible. Per site, add:
  - one cluster whose script has a 20s silent gap before the answer
  - one whose script sends a JSON-RPC error
  - one whose script ends with `state: failed`
  - one with `ChatHang`
  - one whose script ends after the `working` step with no final event (cut-off)

  Add `chat: {idle_timeout: 30s, heartbeat: 5s, log_events: true}` to the generated config. Update the doc comment and the startup log.
- [ ] **Step 2: Manual check in a browser:** run `go run ./cmd/fakerancher` and `go run ./cmd/portal -config dev/config.yaml`. As `admin1` on a ready cluster:
  - Progress, tool and answer lines appear one by one.
  - No text appears twice. Your own message isn't repeated as agent output. The partial chunks merge into one line, and the answer doesn't repeat the last progress line.
  - A follow-up says `turn 2`. New conversation resets to `turn 1`.
  - Stop shows `Stopped`, and the next send works.
  - A second tab sending at the same time gets the 429 message.
  - The silent-gap cluster completes, with pings in the browser's network view.
  - The hang cluster ends with the silence-cap message after about 30s.
  - The error, failed and cut-off clusters show their copy.
  - As `limited` on a prod cluster, there is no chat panel.
  - The browser console shows no CSP violations.
- [ ] **Step 3:** Add the `chat:` block to the ConfigMap in `deploy/portal.yaml`, with `log_events: true` for the live test. Update the README: the chat section, the new fake clusters, and a note that any ingress in front of the portal must disable response buffering and allow reads of at least `chat.max_duration`.
- [ ] **Step 4:** Run `go test -race ./...` and expect PASS. Optionally run `kubectl apply --dry-run=client -f deploy/portal.yaml`.
- [ ] **Step 5:** Stage the files and ask the user to commit: `chore: fake A2A seeds, example config, docs for plan 3`.

---

### Task 8: Live-test runbook

**Files:**
- Create: `docs/plans/PLAN3-live-test.md`

- [ ] **Step 1:** Write it in the same structure and tone as `PLAN2-live-test.md`:
  - "Before you start":
    - image `v0.3.0` by digest;
    - `chat.log_events: true`;
    - note one ready test cluster per site;
    - agent pod log access for C2;
    - watch for `event=chat` and `event=chat_event`;
    - one long test at a time per site.
    - **Per site, record the Rancher version** (`https://<site>/rancherversion`). Below v2.13.9 is worth flagging to the site owners.
    - **If anyone can read the Rancher local cluster,** record whether `kubectl -n cattle-system get ingress rancher -o yaml` has the `nginx.ingress.kubernetes.io/proxy-read-timeout` and `proxy-send-timeout` annotations, and their values.
    - Kubeconfigs for the test clusters, downloaded from the Rancher UI (for C10).
  - The test table below.
  - A short report format, "What each field means", "If a test fails", and "Never type". Message text and agent answers are never typed into the report either. Only codes, timings and shapes go in.

  | ID | Do this | Expected | Record |
  | --- | --- | --- | --- |
  | C1 | On one ready cluster per site, ask a short question (e.g. "list namespaces") | Lines appear one by one while the agent works, then an answer and the diagnostics line | Per site: live `y`/`n`, first-event ms, events, total ms |
  | C2 | Ask a follow-up that needs the previous answer | The agent uses the earlier answer. No `SessionNotFoundError` in the agent pod log | Pass/fail |
  | C3 | Ask something that makes the agent run tools for over 2 minutes, ideally one tool call over 60s | Completes | Pass/fail; if cut off, after how long, the error text, and the `result` in `event=chat` (`cut-off` or `idle-timeout`) |
  | C4 | Press Stop during a reply, then send again. Optionally, a minute later, run `kubectl get --raw "/api/v1/namespaces/kagent/services/http:kagent-controller:8083/proxy/api/sessions/<context_id>/tasks?user_id=admin@kagent.dev"` with that cluster's kubeconfig. Take `<context_id>` from the `event=chat` line. | `Stopped`; the next reply works | Pass/fail; optionally the stopped turn's `status.state` (only the state, not the content) |
  | C5 | Send in two tabs at once | The second shows `A reply is already running in another tab` | Pass/fail |
  | C6 | New conversation, then ask about the previous answer | The agent doesn't know it | Pass/fail |
  | C7 | Open a cluster whose check isn't `ready` (if any) | `Chat unavailable: ...`, no chat box | Pass/skip |
  | C8 | Optional: log out of one site in the Rancher UI, then send on a cluster of that site | `Your session on <site> has expired: log in again`; other sites still chat | Pass/skip |
  | C9 | From the `event=chat_event` lines of one C1 reply, list the distinct `shape` values | — | The shapes, one per line |
  | C10 | Per site, with a test cluster's kubeconfig: `time kubectl get --raw "/api/v1/namespaces/default/configmaps?watch=1&fieldSelector=metadata.name=nonexistent&timeoutSeconds=150"` | Closes after about 150s | Per site: seconds until it closed |

  Report example:
  ```
  v0.3.0 report  2026-12-05
  C1 pass  live s1=y s2=y s3=y  first s1=900 s2=850 s3=1200  events s1=31 s2=28 s3=40  total s1=41s s2=38s s3=55s
  C2 pass
  C3 pass  total=3m10s
  C4 pass  kagent state after stop=completed
  C5 pass
  C6 pass
  C7 skip
  C8 skip
  C9 shapes:
    status-update state=submitted final=false parts=text meta=
    status-update state=working final=false parts= meta=kagent_session_id,kagent_user_id
    status-update state=working final=false parts=text,data meta=kagent_adk_partial,kagent_type
    status-update state=working final=false parts=text meta=kagent_adk_partial
    artifact-update parts=text meta=
    status-update state=completed final=true parts= meta=
  C10 s1=150 s2=150 s3=60
  rancher s1=v2.13.3 s2=v2.13.3 s3=v2.13.3  ingress timeouts s1=1800 s2=? s3=none
  warn/error: none
  other: none
  ```
  Include the "If streaming fails on site" table from the design, so the tester knows which C1/C3/C10 observations matter.
- [ ] **Step 2:** Stage the file and ask the user to commit: `docs: live-test kit for plan 3`.
- [ ] **Step 3: Handoff:** the user pushes, CI builds the image, the user tags `v0.3.0`, moves the image in, runs `PLAN3-live-test.md` and brings back the report. C1/C3/C10 decide whether a fallback plan is needed. C4's kagent state shows whether polling the session's tasks can recover a cut stream. C9 confirms or corrects the event parsing. Then Plan 4 (hardening).
