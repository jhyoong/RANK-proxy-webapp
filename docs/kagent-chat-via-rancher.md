# kagent Chat Sessions Through the Rancher API

As of 2026-11-20. Covers kagent **v0.9.6** and Rancher **v2.13** and **v2.14**. The MVP targets Rancher v2.13, so every v2.13 requirement is called out. Read alongside [design.md](design.md) and [verification.md](verification.md).

This explains how the portal backend calls kagent through Rancher in order to run a chat live and to rebuild a complete past session: user messages, each tool call, each tool result, thinking, and final answers. It does not cover login, the cluster list or the UI.

**Where each finding comes from**

| Tag | Source |
| --- | --- |
| **[live]** | Tested on 2026-11-20 against Rancher **v2.14.0** (test lab) with kagent controller and agent at **0.9.6**, using agent `kagent/k8s-agent` (Python runtime, OpenAI-compatible model). |
| **[src]** | Read from kagent source at tag `v0.9.6` (commit `e6287be`) and its pinned libraries (a2a-sdk 0.3.23, google-adk 1.31.1, trpc-a2a-go v0.2.5). |
| **[rancher]** | Read from Rancher release notes and source at v2.13.0, v2.13.11, v2.14.0 and v2.14.7. Not tested live on v2.13. |

Nothing was tested on a v2.13 site. The [v2.13 MVP checklist](#v213-mvp-checklist) lists what to confirm there.

## 1. Call path

Every call goes to the Rancher site that manages the cluster, with the user's own Rancher token. Rancher passes it through the cluster agent tunnel to the downstream API server's service proxy, which forwards it to the kagent controller.

```
BASE = https://<rancher>/k8s/clusters/<cluster-id>/api/v1/namespaces/kagent/services/http:kagent-controller:8083/proxy
```

- **Headers on every call:** `Authorization: Bearer <user's Rancher token>`. Legacy `token-xxxxx:secret` tokens work in both v2.13 and v2.14. **[live, rancher]**
- **RBAC:** downstream RBAC sees the call as the Rancher user, because Rancher impersonates them. The user needs `services/proxy` on the kagent controller service: `get` for GETs and `create` for POSTs (R4 in design.md).
- **Service name** is `<helm-release>-controller`, port 8083. It is `kagent-controller` when the Helm release is named `kagent`. **[src]**
- **v2.14 alias:** v2.14 also accepts `/k8s/proxy/<cluster-id>/...` with the same handler. Use `/k8s/clusters/` because it works on both versions. **[rancher]**

Endpoints used, all relative to `BASE`:

| Purpose | Method and path |
| --- | --- |
| Controller version | `GET /version` returns `{"kagent_version":"0.9.6",...}` **[live]** |
| Agent card | `GET /api/a2a/<agent-ns>/<agent-name>/.well-known/agent.json` **[live]** |
| Send a chat turn | `POST /api/a2a/<agent-ns>/<agent-name>/` (A2A JSON-RPC) **[live]** |
| List a user's sessions | `GET /api/sessions?user_id=<U>` **[live]** |
| Sessions for one agent | `GET /api/sessions/agent/<agent-ns>/<agent-name>?user_id=<U>` **[src]** |
| Replay a session (main source) | `GET /api/sessions/<session-id>/tasks?user_id=<U>` **[live]** |
| Raw events (timestamps, thinking) | `GET /api/sessions/<session-id>?user_id=<U>&order=asc&limit=-1` **[live]** |

Every REST (non-A2A) response is wrapped as `{"error": bool, "data": ..., "message": "..."}`.

## 2. User identity

kagent v0.9.6 has no authentication of its own. It runs in `unsecure` auth mode, which works out the user like this: the `user_id` query parameter first, then the `X-User-Id` header, and otherwise `admin@kagent.dev`. **[src]**

- **On A2A calls,** send `X-User-Id: <AD principal ID>`. It passes through Rancher unchanged, and the resulting session is owned by that user. **[live]**
- **On REST reads,** pass `?user_id=<same value>`. Reading the session under a different user returns 404. **[live]**
- **Ownership is not access control.** Anyone with `services/proxy` on the controller can read any user's history by changing `user_id`, and `GET /api/tasks/<id>` has no user check at all. The portal must set `user_id` itself and never relay kagent REST calls from the browser. **[src]**
- **kagent saves the request headers of every turn** in a `system` event (`state_delta.headers`), but only the headers that reach the agent pod are saved. The controller makes its own request to the pod, so the Rancher token is **not** stored. What was stored: `accept`, `content-type`, `host`, `user-agent` and `x-user-id`. Don't put secrets in custom headers. **[live]**
- **Session IDs:** the A2A `contextId` becomes the kagent session ID. One portal conversation is one `contextId`, which is one session. **[live]**
- **Multi-turn works:** issue #1882 (`SessionNotFoundError` on the second turn) is fixed in v0.9.6, and a second turn on the same `contextId` succeeded. **[src, live]**

## 3. Running a chat turn (live streaming)

### Request

```http
POST {BASE}/api/a2a/kagent/k8s-agent/
Authorization: Bearer <token>
Content-Type: application/json
Accept: text/event-stream
X-User-Id: <principal>
```

```json
{
  "jsonrpc": "2.0",
  "id": "<uuid>",
  "method": "message/stream",
  "params": {
    "message": {
      "kind": "message",
      "messageId": "<uuid>",
      "contextId": "<conversation-uuid>",
      "role": "user",
      "parts": [{ "kind": "text", "text": "Which pods are not Ready?" }]
    }
  }
}
```

The response is `200`, `content-type: text/event-stream`, over HTTP/2. Events arrive one at a time: the first came after about 0.14s, and text arrived in small pieces about 100ms apart. Nothing was buffered until the end. **[live, v2.14.0]**

### Event sequence

Each SSE `data:` line is a JSON-RPC response whose `result` is one of the following. **[live]**

| # | `result.kind` | `status.state` | Content |
| --- | --- | --- | --- |
| 1 | `status-update` | `submitted` | Echo of the user message |
| 2 | `status-update` | `working` | No message. `metadata` has `kagent_session_id` and `kagent_user_id` |
| 3…n | `status-update` | `working` | `status.message` holds the agent output (see below) |
| n+1 | `artifact-update` | | Final answer parts (a copy of the last text) **[src]** |
| last | `status-update` | `completed`, `failed` or `input-required`, `final: true` | Usually no message **[src]** |

The `working` messages come in two kinds, told apart by `status.message.metadata.kagent_adk_partial`:

- **`true`: partial text chunk.** Append it to the bubble currently on screen. These chunks are **never stored**, so replay never sees them.
- **`false` or missing: complete step.** It repeats the step's full text, plus any `function_call` parts. When it arrives, **replace** the partial bubble with it, otherwise the text appears twice.

Tool calls and results are A2A data parts:

```json
{"kind":"data","metadata":{"kagent_type":"function_call"},
 "data":{"id":"call_053d09de","name":"k8s_get_resources",
         "args":{"namespace":"kagent","output":"wide","resource_type":"pod"}}}

{"kind":"data","metadata":{"kagent_type":"function_response"},
 "data":{"id":"call_053d09de","name":"k8s_get_resources",
         "response":{"content":[{"type":"text","text":"NAME  READY ..."}],"isError":false}}}
```

- **Pairing:** match each result to its call by `data.id`.
- **Parallel calls:** one message can carry several calls, and the following message carries all their results. **[live]**
- **Result shapes:** MCP tools return `{content, isError}`; other tools return `{"result": ...}`. **[src]**
- **Go runtime agents** use `adk_type`, `adk_partial` and `adk_thought` instead of the `kagent_` keys. Read both prefixes, as the kagent UI does (`ui/src/lib/messageHandlers.ts:456`). **[src]**
- **Sub-agent calls** are tool calls whose name contains `__NS__`. The call part also carries `kagent_subagent_session_id`, which points to a child session (`source: "agent"`) holding the sub-agent's own steps. **[src]**
- **Human approval (HITL):** a turn can end in `input-required` with an `adk_request_confirmation` call. The MVP's agent is read-only, so treat this as "agent asked for approval" and don't build an approval flow yet. **[src]**

### Multiple turns

- **Reuse the same `contextId`** for follow-up messages.
- **Allow only one running turn per conversation.** If a second message is sent while the first is still running, both run at the same time on the same session, and their stored events are interleaved. This happened in the live test. Disable sending until the turn reaches a final state.

## 4. Rebuilding a past session

### Primary source: tasks

`GET {BASE}/api/sessions/<id>/tasks?user_id=<U>` returns one A2A task per user turn, oldest first, with no paging. **[live]** This is what the kagent UI uses (`ui/src/lib/messageHandlers.ts:8`, `extractMessagesFromTasks`). **[src]**

```json
{"id":"4cec67da-...","contextId":"0891727d-...","kind":"task",
 "status":{"state":"completed","timestamp":"2026-11-20T11:34:22.391479+00:00"},
 "metadata":{"kagent_invocation_id":"e-16c16ec7-...","kagent_user_id":"...",
             "kagent_usage_metadata":{"promptTokenCount":68114,"candidatesTokenCount":99,"totalTokenCount":68213}},
 "history":[
   {"role":"user","messageId":"0f95ea83-...","parts":[{"kind":"text","text":"..."}]},
   {"role":"user","messageId":"0f95ea83-...","parts":[{"kind":"text","text":"..."}]},
   {"role":"agent","messageId":"...","metadata":{"kagent_adk_partial":false,...},
    "parts":[{"kind":"text","text":"Let me check..."},{"kind":"data","metadata":{"kagent_type":"function_call"},"data":{...}}]},
   {"role":"agent","parts":[{"kind":"data","metadata":{"kagent_type":"function_response"},"data":{...}}]},
   {"role":"agent","parts":[{"kind":"text","text":"Final answer ..."}]}],
 "artifacts":[{"artifactId":"...","parts":[{"kind":"text","text":"Final answer ..."}]}]}
```

To rebuild a session from its tasks:

1. **Read the tasks in the order returned,** one turn per task.
2. **Remove duplicate messages by `messageId`.** The user message appears twice in each live task. **[live]**
3. **Render every part of every message,** in order:
   - text parts as text;
   - `function_call` as a tool-call card;
   - `function_response` as its result, paired by `data.id`.

   Don't copy the kagent UI here: it drops text that shares a message with a tool call (`messageHandlers.ts:151-158`). **[src]**
4. **Use `artifacts`** only as a fallback for the final answer. It repeats the last text message.
5. **Show turns that aren't finished.** A task left in `working` is a turn still running or cut off (section 5). Mark it as such rather than hiding it.
6. **Show sub-agent steps,** if needed, by fetching the child session the same way.

### Secondary source: raw events

`GET {BASE}/api/sessions/<id>?user_id=<U>&order=asc&limit=-1` returns `{"session": {...}, "events": [...]}`. **[live]**

- **Order:** events are newest first unless you pass `order=asc`.
- **Paging:** `limit=-1` means no limit; `after=<RFC3339>` returns only events after that time. **[src]**
- **Shape:** each event is `{id, session_id, user_id, created_at, updated_at, data}`, where `data` is a **JSON string** holding a raw ADK event. Parse it a second time.
- **Python runtime format:** snake_case keys, with nulls included: `author`, `invocation_id`, `content.parts[].text`, `function_call`, `function_response`, `thought`. **[live]**
- **Go runtime format:** Go runtime agents write PascalCase keys (`Content`, `Author`, …), so the format is different. **[src]**
- **Grouping:** group events by `invocation_id`, which is one turn and matches the task's `kagent_invocation_id`. Skip `author: "system"` events, which hold the saved headers. **[live]**

Use events only for:
- **Per-step timestamps.** Task messages have none; the only time on a task is `status.timestamp`.
- **Thinking,** if a model ever produces it (below).

### What can be recovered

| Element | From tasks | Notes |
| --- | --- | --- |
| User message | Yes, exact | **[live]** |
| Tool call (name, args, id) | Yes, exact | **[live]** |
| Tool result | Yes, the full result | No size cap was found in kagent; the stored field is unlimited text. **[src]** |
| Agent text between steps | Yes | **[live]** |
| Final answer | Yes | **[live]** |
| Order within a turn | Yes | Message order in `history`. **[live]** |
| Order across turns | Yes | Task order; one turn at a time (section 3). |
| Timestamps | Turn end only | Per step: event `created_at`. **[live]** |
| Token usage | Yes | `metadata.kagent_usage_metadata` **[live]** |
| Thinking | **No, with the current setup** | See below. |

**Thinking.** kagent stores thinking when a model returns it: a text part with `thought: true` becomes a text part with metadata `kagent_thought: true` (or `adk_thought`). **[src]** In practice v0.9.6 almost never receives any:

- **Python runtime with OpenAI-compatible providers** (our agent, `ModelConfig local-openai`): only `content` and `tool_calls` are read from the model response, and `reasoning_content` is thrown away (`python/packages/kagent-adk/src/kagent/adk/models/_openai.py:317-345`). No thinking parts appeared in any stored session. **[src, live]**
- **Python runtime with Anthropic, Bedrock or Gemini:** none is captured, either because thinking isn't requested or because reasoning blocks are dropped. **[src]**
- **The one exception:** Go runtime (`spec.declarative.runtime: go`) with Bedrock and `additionalModelRequestFields.thinking` set. **[src]**
- **Models that write `<think>…</think>` into ordinary text** come through as plain text, not flagged as thinking.

For the MVP, plan for no thinking. Getting it would need a kagent change or a different runtime and provider.

## 5. Timeouts, limits and recovery

A long investigation passes through several timeouts. The shortest one wins.

| Layer | Limit | Applies to |
| --- | --- | --- |
| Ingress in front of Rancher | **Rancher v2.13 Helm default: 1800s** read and send (`proxy-read-timeout`, `proxy-send-timeout` annotations). **v2.14 default: none, so ingress-nginx's 60s applies** | Time with **no data** on the stream **[rancher, live]** |
| Load balancer in front of the ingress (per site) | Unknown, check each site | Usually idle time |
| kagent controller to agent pod | **600s total** (`controller.streaming.timeout`, Helm default `600s`; Go `http.Client.Timeout`) | **Whole turn duration**, even while data is flowing **[src]** |
| Rancher request body | `public-api-body-limit`, default **1 MiB**, same in v2.13 and v2.14 | A2A POST body **[rancher, read from code, not tested]** |
| Agent inbound A2A body | 10 MB (`A2A_MAX_CONTENT_LENGTH`) | **[src]** |

Notes:

- **Silent gaps are normal.** No SSE events flow while the model is generating a step that isn't streamed, or while a tool runs. On the test lab's slow local model a gap lasted about 79s. kagent sends no SSE heartbeat during these gaps. **[live]**
- **v2.14 cuts silent streams at 60s.** On the v2.14.0 test lab, both chat streams and an idle watch request were closed after exactly **60.0s** of silence. The `rancher` ingress there has no timeout annotations. **[live]** On v2.13 with the default chart, the same gap is allowed up to 1800s. Confirm the annotations on each real site (checklist below).
- **Turns are capped at 10 minutes.** A turn longer than 600s is cut by the controller whatever the Rancher version. Raise `controller.streaming.timeout` through Fleet if longer investigations are needed (not tested).
- **The agent keeps working after a disconnect.** After the client stream was cut, the turn kept running in the agent pod and kept saving to the task and to events until it finished. **[live]** So a dropped stream is recoverable: poll `GET {BASE}/api/sessions/<id>/tasks?user_id=<U>` every few seconds, render new messages, and stop when the task reaches `completed`, `failed` or `input-required`. A2A `tasks/get` returns the same stored task; `tasks/resubscribe` is not reliable for this. **[src]**
- **Recommended MVP pattern:** stream with `message/stream`. If the stream drops before a final event, switch to polling the session's tasks for that turn.

## 6. Error mapping

| Response | Cause | Source |
| --- | --- | --- |
| `401 {"type":"error","status":"401","message":"Unauthorized 401: must authenticate"}` | Rancher token expired, deleted or wrong | **[live]** |
| `403` from the API server | User lacks `services/proxy` on the controller | design.md |
| Nginx `404` HTML page | Wrong Rancher host name; the ingress only answers on its configured host | **[live]** |
| `404` on `/api/sessions/<id>` | Session doesn't exist **for that `user_id`** | **[live]** |
| `502`/`503`/connection error | Cluster disconnected, or controller down. On v2.13.5+ and v2.14.1+ Rancher returns an error at once when it can't get the downstream impersonation token | **[rancher]** |
| Stream ends without a `final: true` event | A timeout in the chain (section 5); recover by polling | **[live]** |

## 7. Rancher v2.13 vs v2.14

Latest patches as of 2026-10-29: **v2.13.11** and **v2.14.7**. Sources: GitHub release notes, the SUSE notes for v2.13.8, v2.13.11 and v2.14.4–7, and source diffs.

### Differences that matter

| Area | v2.13 | v2.14 | Impact |
| --- | --- | --- | --- |
| Ingress timeouts (Helm `ingress.includeDefaultExtraAnnotations`) | `true` by default: read and send timeouts 1800s, connect 30s | `false` by default (marked deprecated): ingress-nginx's 60s default applies | **Main risk for streaming.** On v2.14, set `ingress.extraAnnotations` (`nginx.ingress.kubernetes.io/proxy-read-timeout: "1800"`, `proxy-send-timeout: "1800"`) or turn the flag back on. v2.14's new Gateway API option has its own timeouts. ([v2.14.0 notes](https://github.com/rancher/rancher/releases/tag/v2.14.0), [#53272](https://github.com/rancher/rancher/issues/53272)) |
| `/v3-public` login | Always on | New `v3-public` feature flag, on by default. If an admin turns it off, `/v3-public/*` returns 404 | Use `/v1-public/login`, which works on both ([#52680](https://github.com/rancher/rancher/issues/52680)) |
| v3 tokens | Supported | Deprecated (no removal release named). Login still returns legacy `token-xxx:secret` tokens on both | No change needed now; keep token handling in one module |
| Downstream Kubernetes | 1.32–1.34 | 1.33–1.35 | Open v2.14 issue [#56209](https://github.com/rancher/rancher/issues/56209): cluster agent hangs with downstream 1.35 (seen on v2.14.3), which stops all `/k8s/clusters` traffic for that cluster. Not confirmed fixed in v2.14.x. |
| `/k8s/proxy/<id>` alias | Not present | Present | Optional; `/k8s/clusters/<id>` works on both |
| OIDC JWT bearer tokens | Not present | Accepted when the `oidc-provider` feature is on | Extra option only; doesn't affect our tokens |

### No material difference

- **Cluster proxy behaviour:** flushing, buffering and streaming are the same in both. Server-sent events flush immediately and Rancher sets no server read or write timeouts.
- **Agent tunnel:** remotedialer goes from v0.6.0 to v0.6.1 with no functional change.
- **Header pass-through:** custom headers such as `X-User-Id` pass through on both. Rancher sets `Impersonate-User` and `Impersonate-Group` downstream.
- **RBAC:** `services/proxy` and SelfSubjectAccessReview checks are done by downstream RBAC as the user on both.
- **Session TTL settings** (`auth-user-session-ttl-minutes`, `auth-user-session-idle-ttl-minutes`, default 960 each), token hashing and logout (`POST /v1/logout`).
- **Request body limit:** 1 MiB on both.
- **Cluster list:** `/v1/management.cattle.io.clusters` has no breaking change.

### Security fixes released in both lines

Run at least **v2.13.9** for the MVP:

| Fix | v2.13 | v2.14 |
| --- | --- | --- |
| Error at once when the downstream impersonation token is missing ([#54554](https://github.com/rancher/rancher/pull/54554)) | v2.13.5 | v2.14.1 |
| CVE-2026-44945 (critical). Caller-supplied `Impersonate-*` headers are now checked against the **local** cluster. Only matters if the portal sends them, which it doesn't | v2.13.8 | v2.14.4 |
| CVE-2026-59675 (login body cap) | v2.13.8 | v2.14.4 |
| CVE-2026-75035 (ext token cross-user listing) | v2.13.9 | v2.14.5 |

## v2.13 MVP checklist

The live tests ran on v2.14.0, so confirm these on a v2.13 site before relying on them:

- [ ] **Rancher version** is v2.13.9 or later on each site (`GET /rancherversion`).
- [ ] **Ingress timeouts.** The `rancher` ingress in `cattle-system` has `nginx.ingress.kubernetes.io/proxy-read-timeout: "1800"` and `proxy-send-timeout: "1800"`. If a site was installed with `includeDefaultExtraAnnotations=false`, add them. Also check any load balancer in front.
- [ ] **Silent-gap test.** Hold a request open with no data through `/k8s/clusters/<id>` for more than 60s. For example, watch a ConfigMap that doesn't exist: `.../api/v1/namespaces/default/configmaps?watch=1&fieldSelector=metadata.name=nonexistent&timeoutSeconds=150`. It should close at about 150s, not at 60s.
- [ ] **Streaming.** A `message/stream` turn delivers events one at a time, with time to first event under 1s plus model latency.
- [ ] **Identity.** `X-User-Id` reaches kagent: the session created is owned by that user (`GET /api/sessions?user_id=<U>`).
- [ ] **Replay.** A two-turn session with tool calls rebuilds fully from `/api/sessions/<id>/tasks`.
- [ ] **Recovery.** Cut a stream on purpose; the turn still completes, and polling the tasks endpoint picks up the rest.
- [ ] **Turns over 10 minutes.** Decide whether `controller.streaming.timeout` needs raising for long investigations.
- [ ] **Body limit (optional).** A POST over 1 MiB through the proxy is rejected. That is fine for chat; it just confirms the limit exists.
