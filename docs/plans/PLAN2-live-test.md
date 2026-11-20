# Plan 2 Live Test: Cluster List and kagent Access Check

Run this inside the target network with the Plan 2 image (`v0.2.0`), then type up the short [report](#report-format). It is designed to be typed by hand, because files can't leave the network. It tests the cluster list, the per-cluster kagent access check and the diagnostics page against the three real Rancher sites. Nothing here changes Rancher settings, RBAC or kagent. The checks are read-only: a permission check (SSAR) and a fetch of the agent card.

## Before you start

- [ ] **Image.** Move the image in using the README's `skopeo` steps. Record the digest. Deploy by digest.
- [ ] **Config.** In the ConfigMap, set `kagent.agent_name` to the deployed kagent agent's name (the portal will not start without it). Defaults are fine for `clusters.cache_ttl` (2m) and `api.timeout` (15s).
- [ ] **Rancher UI.** Open the Rancher UI for each site in another tab. You need each site's cluster count and which clusters show as disconnected.
- [ ] **Known clusters.** Note one cluster that runs kagent, and (if any) one without kagent or with kagent down.
- [ ] Deploy: `kubectl apply -f deploy/portal.yaml`, then `kubectl -n kagent-portal port-forward svc/portal 8080:80`, then open `http://localhost:8080`.
- [ ] Watch logs in a second terminal: `kubectl -n kagent-portal logs -f deploy/portal`. Look for `event=clusters_list` and `event=cluster_check`. The logs never contain passwords or token values.

To change config: edit the ConfigMap, then `kubectl -n kagent-portal rollout restart deploy/portal`. Log out before each restart where you can (see the Plan 1 note on tokens left behind).

## Tests

Log in first. Latencies and the per-site list status are on `/diagnostics`.

| ID | Do this | Expected | Record |
| --- | --- | --- | --- |
| L1 | Log in, open `/home` | Clusters from all 3 sites. The summary counts match each site's cluster count in the Rancher UI, minus `local` | Counts per site, plus list ms per site (from `/diagnostics`) |
| L2 | Find a cluster the Rancher UI shows as disconnected (if any) | Greyed out, no link | Pass/skip. If wrong, that row's raw state and conditions from `/diagnostics` |
| L3 | Click a cluster known to run kagent | `kagent is ready`, the agent name, streaming yes/no | Card name, streaming, SSAR ms, card ms |
| L4 | Click a cluster without kagent, or with kagent down (if any) | `kagent or the agent isn't installed on this cluster` (`not-installed`) or `kagent is installed but not running` (`not-running`) | The outcome text and card code |
| L5 | Click Refresh | The list reloads (`event=clusters_list` in the logs) | Pass/fail |
| L6 | Type part of a cluster name in the filter | Only matching rows | Pass/fail |
| L7 | Optional: a non-admin colleague opens a cluster they can see but have no kagent role on | `You don't have kagent access on this cluster` | Pass/skip |
| L8 | Search the list for `local` | No cluster is named exactly `local` (a name that merely contains "local" is fine) | Pass/fail |
| L9 | Edit the URL to `/clusters/<site>/local` | 404 | Pass/fail |

L7 is optional because you have access to everything, so it needs a colleague with a narrower role.

## Report format

Type the report by hand in this short format. On a normal run most lines are one word. The values below are an example only.

```
v0.2.0 report  2026-12-01
L1 pass  count s1=42 s2=37 s3=51  ms s1=310 s2=280 s3=400
L2 pass
L3 pass  agent=<name> streaming=y  ssar=120ms card=180ms
L4 skip
L5 pass
L6 pass
L7 skip
L8 pass
L9 pass
warn/error: none
other: none
```

**What each field means**

| Field | What to write |
| --- | --- |
| L1 to L9 | `pass`, `fail` or `skip`, plus the extra fields shown |
| L1 `count` | The cluster count per site from the `/home` summary line, in site order. If it differs from the Rancher UI count minus `local`, write `fail` and both numbers, e.g. `s2 portal=37 ui=39`. |
| L1 `ms` | The list latency per site from `/diagnostics` |
| L3 `agent` | The agent name shown on the result. Write `xx` if it is sensitive. |
| L3 `streaming` | `y` or `n`. The page shows `true` or `false`; record it as `y` or `n`. |
| L3 `ssar`, `card` | The SSAR ms and card ms from the diagnostics table on the cluster page |
| L4 | The outcome code (`not-installed` or `not-running`) and the card code, e.g. `L4 pass  not-installed card=404` |
| `warn/error` | One line per `WARN` or `ERROR` log line: its `event` and `site`, e.g. `cluster_check site-2` |

**Cluster names** may be internal. If a name is sensitive, replace it with `xx` in anything you type.

**If a test fails**, add a line under it with the key fields from the log line for that test. For an `event=cluster_check` line, copy `site`, `cluster`, `principal`, `ssar_allowed`, `ssar_ms`, `card_http_code`, `card_ms`, `result`, and `rancher_body` if present (it appears on unexpected codes). For an `event=clusters_list` line, copy the per-site entries of its `sites` field (`site`, `status`, `http_code`, `count`, `ms`). For example:

```
L3 fail  result=unexpected
  site-2 ssar_allowed=true card_http_code=502 rancher_body="..."
```

For an L2 failure, also type that cluster's raw state and its Connected and Ready conditions from `/diagnostics`.

**Never type** a password or token value. Token **ids** (`token-ab12c`) and cluster IDs are fine. Log lines never contain secrets, so their fields are safe to copy.
