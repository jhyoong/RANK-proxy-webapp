# Plan 5 Live Test: End-User UI

Run this inside the target network with the Plan 5 image, then type up the short [report](#report-format). It checks the conversation sidebar against real kagent, the unavailable screen, the `ui:` block and the CSP. Nothing here changes Rancher settings, RBAC or kagent.

## Before you start

- [ ] **Image.** Deploy the Plan 5 image by digest. Record the digest.
- [ ] **kagent auth mode.** kagent must run in its default `unsecure` auth mode. The portal sends no user id, so history is shared per cluster.
- [ ] **Config.** Add a `ui:` block to the ConfigMap, then `kubectl -n kagent-portal rollout restart deploy/portal`:
  ```yaml
  ui:
    banner: "Live test banner"
    banner_level: warning
    login_help: "Need access? Email <support-address> or see https://<internal-wiki>/help"
    support_email: "<support-address>"
  ```
- [ ] **Clusters.** `<ready-cluster>` (kagent running), `<no-kagent-cluster>` (reachable, no kagent installed).
- [ ] **Logs**: `kubectl -n kagent-portal logs -f deploy/portal`.
- [ ] Browser DevTools open on the Console.

## Tests

| ID | Do this | Expected | Record |
| --- | --- | --- | --- |
| U1 | On `<ready-cluster>` start two conversations from the portal. Reload the page. Then ask kagent for the sessions directly through Rancher: `GET .../api/sessions/agent/<ns>/<name>` | The sidebar lists both conversations, newest first. The agent-filtered list contains the sessions the portal created | Pass/fail, entries seen |
| U2 | Look at the sidebar titles and the session `name` in the U1 response | The `name` is filled and the sidebar shows it (not a bare UUID) | Pass/fail |
| U3 | Open a conversation that used many tool calls (`?c=<uuid>`). Also `GET /clusters/<site>/<id>/conversations/<uuid>` | The transcript renders: user and agent turns, tool calls with args and results, Markdown in agent text. Long results end with `… (truncated)` | Pass/fail |
| U4 | Send a long-running prompt, close the tab mid-reply, reopen `?c=<uuid>` | The conversation shows as "working" and its transcript loads. Once the agent finishes, a reload shows it completed | Pass/fail |
| U5 | Open `<no-kagent-cluster>` | The unavailable screen with a `Reference: <outcome> · <cluster name>` line and no other debugging detail. Log: `event=cluster_check` with `ssar_reason`, `ssar_http_code`, `card_*` | Pass/fail, outcome |
| U6 | Log out and view the login page | The banner (amber), login help (URL and email as links) and support email show as configured | Pass/fail |
| U7 | Using a second user, open `<ready-cluster>` | The same conversations show (shared history). Let the session expire: the whole portal session ends and `/home` goes to login | Pass/fail |
| U8 | Browse login, `/home`, cluster page, a chat and a transcript with the Console open | No CSP violations or other console errors | Pass/fail, messages |
| U9 | Repeat Plan 3 C1, C2 and C4 quickly | Same results as before | Pass/fail each |

## Report format

Type the report by hand. Values below are an example only.

```
Plan 5 report  2026-12-01
U1 pass  entries=2
U2 pass
U3 pass
U4 pass
U5 pass  outcome=not-installed
U6 pass
U7 pass
U8 pass
U9 C1 pass  C2 pass  C4 pass
warn/error: none
other: none
```

Record results inline in the Record column or this block.
