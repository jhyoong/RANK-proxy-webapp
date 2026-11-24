# Plan 3.2 Live Test: Cluster List Size Fix

Run this inside the target network with the `v0.3.2` image, then type up the short [report](#report-format). It is designed to be typed by hand, because files can't leave the network. It checks that every site's cluster list now loads (in the v0.2.0 test, site 1 failed with `unexpected EOF`), records how big each site's list reply is, and confirms that site 1's clusters can be checked and chatted with. Nothing here changes Rancher settings, RBAC or kagent.

## Before you start

- [ ] **Image.** Move the image into the internal registry. Deploy `v0.3.2` by digest. Record the digest.
- [ ] **Config.** Keep the v0.3.x ConfigMap. `kagent.agent_name` must be set. If kagent runs in a site's `local` cluster, set `clusters.include_local: true`.
- [ ] **Rancher UI.** Open the Rancher UI for each site in another tab of the same browser, logged in. You need each site's cluster count, and the logged-in tab for S2.
- [ ] **Known cluster.** Note one site-1 cluster that runs kagent.
- [ ] Deploy: `kubectl apply -f deploy/portal.yaml`, then `kubectl -n kagent-portal port-forward svc/portal 8080:80`, then open `http://localhost:8080`.
- [ ] Watch logs in a second terminal: `kubectl -n kagent-portal logs -f deploy/portal`. Look for `event=clusters_list` and, new in v0.3.2, `event=clusters_list_error` (a `WARN` line for each site whose list failed). The logs never contain passwords or token values.

To change config: edit the ConfigMap, then `kubectl -n kagent-portal rollout restart deploy/portal`. Log out before each restart where you can (see the Plan 1 note on tokens left behind).

## Tests

Log in first. Latencies and the per-site list status are on `/diagnostics`.

| ID | Do this | Expected | Record |
| --- | --- | --- | --- |
| S1 | Log in, open `/home` | Clusters from all 3 sites. Each count matches the site's cluster count in the Rancher UI, minus `local` (unless `clusters.include_local: true`, then including it) | Counts per site, plus list ms per site (from `/diagnostics`) |
| S2 | Per site: in the browser tab logged in to that site's Rancher UI, open DevTools (Network), then open `https://<site>/v1/management.cattle.io.clusters` | A JSON page loads | Per site: the response size in KB. In the Size column, use the uncompressed (resource) size, not the transferred size |
| S3 | While doing S1, and after one Refresh, watch the portal log | No `event=clusters_list_error` lines | Pass/fail |
| S4 | Click the site-1 cluster known to run kagent | `kagent is ready` | Pass/fail, SSAR ms, card ms |
| S5 | On that cluster, ask the chat a short question (e.g. "list namespaces"), as in Plan 3 test C1 | Lines appear one by one, then an answer and the diagnostics line | Live `y`/`n`, first-event ms, events, total ms |

S2 shows how close each site is to the portal's 32 MiB limit for the cluster list. The fix assumes no site comes near it.

## Report format

Type the report by hand in this short format. On a normal run most lines are short. The values below are an example only.

```
v0.3.2 report  2026-12-01
S1 pass  count s1=60 s2=41 s3=17  ms s1=900 s2=99 s3=85
S2 size s1=1450KB s2=980KB s3=410KB
S3 pass
S4 pass  ssar=40ms card=35ms
S5 pass  live=y first=900 events=31 total=41s
warn/error: none
other: none
```

**What each field means**

| Field | What to write |
| --- | --- |
| S1, S3 to S5 | `pass`, `fail` or `skip`, plus the extra fields shown |
| S1 `count` | The cluster count per site from the `/home` summary line, in site order. If it differs from the Rancher UI count, write `fail` and both numbers, e.g. `s1 portal=0 ui=60`. |
| S1 `ms` | The list latency per site from `/diagnostics` |
| S2 `size` | The uncompressed response size per site, in KB |
| S4 `ssar`, `card` | The SSAR ms and card ms from the diagnostics table on the cluster page |
| S5 | From the chat's diagnostics line: live `y`/`n`, first-event ms, events, total seconds |
| `warn/error` | One line per `WARN` or `ERROR` log line: its `event` and `site`, e.g. `clusters_list_error site-1` |

**Cluster and site names** may be internal. If a name is sensitive, replace it with `xx` in anything you type.

**If S1 or S3 fails**, add a line under it with the fields of that site's `event=clusters_list_error` line: `site`, `http_code`, `ms`, `error`. For example:

```
S1 fail  count s1=0(err) s2=41 s3=17
  site-1 http_code=200 ms=900 error="unexpected response from Rancher: response from /v1/management.cattle.io.clusters exceeds 33554432 bytes"
```

How to read the `error`:

| `error` contains | Meaning |
| --- | --- |
| `exceeds 33554432 bytes` | The reply is bigger than the portal's 32 MiB limit. Compare with S2. The limit needs raising, or the list needs field filtering (a follow-up plan). |
| `decode ...: unexpected end of JSON input` or `read ...: unexpected EOF` | The reply was cut short below the limit: something in the network (a proxy or load balancer) closed it early. Not a portal limit. |
| `token expired or revoked` | Log out and in again, then repeat S1. |

**Never type** a password or token value. Token **ids** (`token-ab12c`) and cluster IDs are fine. Log lines never contain secrets, so their fields are safe to copy. **The S2 JSON page contains cluster details: record only its size, never its content.**
