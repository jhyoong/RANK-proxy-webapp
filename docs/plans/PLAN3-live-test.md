# Plan 3 Live Test: Streaming Chat with kagent

Run this inside the target network with the Plan 3 image (`v0.3.0`), then type up the short [report](#report-format). It is designed to be typed by hand, because files can't leave the network. It tests the chat panel against the three real Rancher sites: whether replies stream live, whether long turns survive, and which event shapes kagent really sends. The chat relays messages to the kagent agent on a test cluster. Use short, harmless questions. Nothing here changes Rancher settings, RBAC or kagent.

## Before you start

- [ ] **Image.** Move the image in using the README's `skopeo` steps. Deploy `v0.3.0` by digest. Record the digest.
- [ ] **Config.** In the ConfigMap, set `chat.log_events: true` (it logs one `event=chat_event` line per upstream event, for C9). Keep `kagent.agent_name` set to the deployed agent's name.
- [ ] **Test clusters.** Note one ready test cluster per site (the cluster page shows `kagent is ready`). If kagent runs in a site's `local` cluster, set `clusters.include_local: true` so that cluster is listed. With fewer than three sites, list only those sites in the ConfigMap (and its CA `items`) and report only those sites.
- [ ] **Agent pod log.** You need read access to the kagent agent pod log on one test cluster, for C2.
- [ ] **Rancher version, per site.** Open `https://<site>/rancherversion` and record the version. Below v2.13.9 is worth flagging to the site owners.
- [ ] **Ingress timeouts.** If anyone can read the Rancher local cluster, record whether `kubectl -n cattle-system get ingress rancher -o yaml` has the `nginx.ingress.kubernetes.io/proxy-read-timeout` and `proxy-send-timeout` annotations, and their values.
- [ ] **Kubeconfigs.** Download a kubeconfig for each test cluster from the Rancher UI (for C10, and optionally C4).
- [ ] Deploy: `kubectl apply -f deploy/portal.yaml`, then `kubectl -n kagent-portal port-forward svc/portal 8080:80`, then open `http://localhost:8080`.
- [ ] Watch logs in a second terminal: `kubectl -n kagent-portal logs -f deploy/portal`. Look for `event=chat` (one per reply) and `event=chat_event` (one per upstream event). The logs never contain passwords or token values, nor message text or agent answers.
- [ ] **One long test at a time per site.** C3 and C10 hold connections open for minutes. Don't run them in parallel on the same site.

To change config: edit the ConfigMap, then `kubectl -n kagent-portal rollout restart deploy/portal`. Log out before each restart where you can.

## Tests

Log in first. After each reply the chat shows a diagnostics line: state, task id, first-event ms, total ms and events.

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

C8 is optional because it needs you to log out of a site. C4's kubectl check is optional too.

### If streaming fails on site

C1, C3 and C10 decide this:

| Observed | Meaning | Next step (a follow-up plan, not Plan 3) |
| --- | --- | --- |
| Everything arrives at once at the end | Something in the chain buffers (ingress `proxy-buffering`, or a load balancer) | Ask the Rancher site owners to disable buffering for `/k8s/clusters/` (E3). The portal side already sends `X-Accel-Buffering: no` towards the browser. |
| Cut off at ~60s of silence, or at a load balancer idle value (C10 also closes early) | Ingress or load balancer read timeout | Ask the site owners to add the 1800s ingress annotations, or build the fallback: on a cut stream, poll `/api/sessions/<id>/tasks` until the turn ends |
| Cut off at ~10m while data is still flowing | kagent's `controller.streaming.timeout` is still the 600s Helm default on that site | Raise it through Fleet, or accept 10m turns |
| Cut off at 30m | The kagent cap, as expected | None. It matches `chat.max_duration`. |

## Report format

Type the report by hand in this short format. The values below are an example only.

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

**What each field means**

| Field | What to write |
| --- | --- |
| C1 to C8 | `pass`, `fail` or `skip`, plus the extra fields shown |
| C1 `live` | `y` if lines appeared one by one while the agent worked, `n` if everything arrived at once at the end |
| C1 `first`, `events`, `total` | From the diagnostics line (or `first_event_ms`, `events`, `total_ms` in `event=chat`), per site in site order. `total` in seconds. |
| C3 `total` | How long the reply ran. If it was cut off, write `fail`, the time, the error text and the `result` (`cut-off` or `idle-timeout`). |
| C4 `kagent state after stop` | Only the `status.state` of the stopped turn (e.g. `completed`, `working`, `canceled`). Omit if you skipped the kubectl check. |
| C9 `shapes` | The distinct `shape` values from the `event=chat_event` lines, one per line, copied as they appear |
| C10 | Seconds until the watch closed, per site |
| `rancher` | The version per site from `/rancherversion`, and the ingress timeout value per site (`?` if unknown, `none` if the annotation is absent) |
| `warn/error` | One line per `WARN` or `ERROR` log line: its `event` and `site`, e.g. `chat site-2` |

**Cluster and site names** may be internal. If a name is sensitive, replace it with `xx` in anything you type.

**If a test fails**, add a line under it with the key fields from its `event=chat` line: `site`, `cluster`, `principal`, `context_id`, `task_id`, `http_code`, `first_event_ms`, `total_ms`, `events`, `result`, and `rancher_body` if present (it appears on unexpected codes). `result` is one of `completed`, `failed`, `input-required`, `error`, `cut-off`, `timeout`, `idle-timeout`, `cancelled`, `expired`, `no-access`, `not-installed`, `not-running`, `unreachable`, `unexpected`. For example:

```
C3 fail  result=cut-off
  site-2 http_code=200 first_event_ms=900 total_ms=60000 events=12
```

**Never type** a password or token value. Token **ids** (`token-ab12c`) and cluster IDs are fine. Log lines never contain secrets, so their fields are safe to copy. **Message text and agent answers are never typed into the report either.** Only codes, timings and shapes go in.
