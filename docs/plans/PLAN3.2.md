# Plan 3.2: Cluster List Size Fix (v0.3.2) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Site 1's cluster list loads, a reply that is still too large fails with a clear "exceeds N bytes" error instead of `unexpected EOF`, and a failed site list shows up as a `WARN` log line with its real HTTP code. A short v0.3.2 live-test runbook goes with it.

**Architecture:** `rancher.Client.do` gains a per-call body limit. The cluster list uses a 32 MiB limit; every other call keeps 1 MiB. A reply over the limit returns a `*BadResponseError` that also matches the new sentinel `ErrBodyTooLarge`. `clusters.Service.List` records the HTTP code from `BadResponseError` and logs one `event=clusters_list_error` WARN line per failed site. No config changes.

**Tech Stack:** Go 1.27.4 standard library. CI is the same as before (`go vet ./...`, `go test -race ./...`, Trivy).

**Spec:** No separate design doc. This plan comes from the v0.2.0 live-test result in [PLAN2-report.md](PLAN2-report.md) (L1: `s1 status=error http_code=0 count=0 ms=520 (unexpected EOF decoding /v1/management.cattle.io.clusters)`) and these findings:

- `internal/rancher/client.go:22` caps every reply at `maxBody = 1 << 20` through `io.LimitReader`. When a JSON reply is cut there, decoding fails with exactly `unexpected EOF`. s2 (41 clusters) and s3 (17) fit under 1 MiB; s1 (~60) does not.
- **Smaller pages won't help.** Rancher 2.13 pins Steve v0.7.48, and its SQL cache (`ui-sql-cache`) is on by default from 2.12. In SQL mode Steve ignores `limit` and `continue` (`pkg/stores/sqlpartition/listprocessor/processor.go` only reads `pagesize`/`page`), so every cluster comes back in one reply. The existing `limit=500` + continue loop stays: it still pages correctly against non-SQL Rancher.
- `internal/clusters/list.go:80-91` records `HTTPCode` only for `*rancher.StatusError`, so a 2xx reply that fails to decode logs `http_code=0`.
- `clusters_list` always logs at Info, so the report's `warn/error:` line missed s1's failure.

Decided with the user: raise the limit for the list only, with clear too-large detection. No `include=` field filtering and no `pagesize`/`page` paging. Add the WARN line. Write a short v0.3.2 runbook. The PLAN2 report's `other:` line is out of scope.

## Global Constraints

- The cluster list body limit is **32 MiB** (`32 << 20`). Every other Rancher call keeps **1 MiB** (`maxBody`). A2A streaming (`stream.go`, `a2a/events.go`) is unchanged.
- Too-large error text (from `BadResponseError.Error()`): `unexpected response from Rancher: response from <path> exceeds <limit> bytes`, where `<path>` has no query string.
- New log line, one per site whose `Status == "error"`, at WARN: `msg="clusters" event=clusters_list_error site=<site> http_code=<n> ms=<n> error=<SiteList.Error>`. The existing Info `event=clusters_list` line is unchanged.
- Never log or render a token. Never log a reply body.
- Repo rule: the implementer stages changes. **The user makes every commit, push and tag.**
- Version: no source change. `cmd/portal/main.go:31` `version` is set by `-ldflags` from the git tag. v0.3.2 is cut by the user pushing tag `v0.3.2` on top of v0.3.1 (`4622e92`).

## Review Focus

1. **A reply exactly at the limit** is accepted, and one byte over is rejected as too large, not as `unexpected EOF`. Test: `TestDo_BodyLimitBoundary` (Task 1).
2. **A short body under the limit** (a proxy cutting the reply) still reports a decode error, **not** too-large, so the two causes can be told apart on site. Test: `TestListClusters_BadJSON` stays green and asserts `!errors.Is(err, rancher.ErrBodyTooLarge)` (Task 1).
3. **One site's list fails while the others succeed.** Only that site gets a WARN line, the other sites' rows still appear, and skipped (not logged in) sites get no WARN. Tests: `TestList_BadResponseWarnsWithCode`, `TestList_SkipsSitesNotLoggedIn` extended (Task 2).
4. **Non-list calls are not loosened.** The agent card, SSAR, login and `/v3/users` still reject replies over 1 MiB. Test: `TestAgentCard_OverOneMiB` (Task 1).
5. **The WARN line contains no token**, like the Info line. Test: `TestList_LogHasNoToken` stays green with a failing site added (Task 2).

## File Structure

- Modify `internal/rancher/client.go`: per-call limit, `ErrBodyTooLarge`, `BadResponseError.tooLarge`.
- Modify `internal/rancher/clusters.go`: `maxListBody`, and `ListClusters` uses it.
- Create `internal/rancher/export_test.go`: test hook for `maxListBody`.
- Modify `internal/rancher/clusters_test.go`, `internal/rancher/client_test.go`: tests.
- Modify `internal/clusters/list.go`, `internal/clusters/clusters_test.go`: HTTP code and WARN line.
- Modify `docs/plans/PLAN2-live-test.md`; create `docs/plans/PLAN3.2-live-test.md`.

---

### Task 1: Per-call body limit with too-large detection

**Files:**
- Modify: `internal/rancher/client.go:22,62-70,99-140`
- Modify: `internal/rancher/clusters.go:10-14,63`
- Create: `internal/rancher/export_test.go`
- Test: `internal/rancher/clusters_test.go`, `internal/rancher/client_test.go`

**Interfaces:**
- Produces: `var ErrBodyTooLarge = errors.New("response too large")`. `*BadResponseError` gains an unexported `tooLarge bool`; `Is(target)` returns true for `ErrBadResponse` always, and for `ErrBodyTooLarge` when `tooLarge`. `Code` keeps the HTTP status. Task 2 relies on `errors.As(err, &be)` giving `be.Code`.
- Produces (test-only): `func SetMaxListBody(t testing.TB, n int64)` in `export_test.go`. It sets `maxListBody` and restores it via `t.Cleanup`.

- [ ] **Step 1: Write the failing tests**

In `clusters_test.go`:
```go
func TestListClusters_LargePage(t *testing.T) {
	f := newFake()
	long := strings.Repeat("x", 1024)
	for i := range 1500 { // ~1.6 MiB in one page (PageSize 0)
		f.Clusters = append(f.Clusters, ranchertest.FakeCluster{ID: fmt.Sprintf("c-%04d", i), DisplayName: long, State: "active"})
	}
	c, bearer := listSetup(t, f)
	got, err := c.ListClusters(context.Background(), bearer)
	if err != nil { t.Fatal(err) }
	if len(got) != 1500 { t.Fatalf("got %d clusters", len(got)) }
}

func TestListClusters_BodyTooLarge(t *testing.T) {
	rancher.SetMaxListBody(t, 4096)
	f := newFake()
	long := strings.Repeat("x", 1024)
	for i := range 20 {
		f.Clusters = append(f.Clusters, ranchertest.FakeCluster{ID: fmt.Sprintf("c-%d", i), DisplayName: long, State: "active"})
	}
	c, bearer := listSetup(t, f)
	_, err := c.ListClusters(context.Background(), bearer)
	if !errors.Is(err, rancher.ErrBodyTooLarge) || !errors.Is(err, rancher.ErrBadResponse) { t.Fatalf("got %v", err) }
	var be *rancher.BadResponseError
	if !errors.As(err, &be) || be.Code != 200 { t.Fatalf("code: %v", err) }
	if want := "response from /v1/management.cattle.io.clusters exceeds 4096 bytes"; !strings.Contains(err.Error(), want) { t.Fatalf("msg %q", err) }
}
```
Extend `TestListClusters_BadJSON`: also assert `!errors.Is(err, rancher.ErrBodyTooLarge)`.

`TestDo_BodyLimitBoundary` (in `clusters_test.go`, using `ListStatus=200` and `ListBody`): with `SetMaxListBody(t, n)` where `n = len(body)` for a valid body `{"data":[{"id":"c-1"}]}` padded with trailing spaces to an exact length, the call succeeds. With `n = len(body)-1`, it fails with `ErrBodyTooLarge`.

`TestAgentCard_OverOneMiB` (in `client_test.go` or `clusters_test.go`, wherever the agent card tests live): a `FakeCluster` with `CardBody` = a valid JSON card padded with whitespace to `1<<20 + 1` bytes. `AgentCard` returns an error matching `ErrBodyTooLarge`.

- [ ] **Step 2: Run them to make sure they fail**

Run: `go test ./internal/rancher/ -run 'LargePage|BodyTooLarge|BodyLimitBoundary|OverOneMiB|BadJSON'`
Expected: FAIL. `LargePage` fails with `unexpected EOF`; the others fail to compile (`SetMaxListBody`, `ErrBodyTooLarge` undefined).

- [ ] **Step 3: Implement**

- `client.go`: rename the body of `do` to `func (c *Client) doLimit(ctx context.Context, method, path string, q url.Values, bearer string, body any, out any, limit int64) error`. `do` becomes a one-line call with `maxBody`. On a 2xx with `out != nil`, read with `io.ReadAll(io.LimitReader(resp.Body, limit+1))`. If `len > limit`, return `&BadResponseError{Code: resp.StatusCode, tooLarge: true, msg: fmt.Sprintf("response from %s exceeds %d bytes", path, limit)}`. Otherwise `json.Unmarshal`, keeping the existing `decode %s: %v` message. Non-2xx and `out == nil` paths are unchanged.
- `clusters.go`: `var maxListBody int64 = 32 << 20` (a var, so the test can override it). `ListClusters` calls `c.doLimit(..., maxListBody)`.
- `export_test.go` (`package rancher`): `SetMaxListBody` as in Interfaces.

- [ ] **Step 4: Run the tests to make sure they pass**

Run: `go vet ./... && go test -race ./...`
Expected: PASS (all packages, including the existing pagination, cap and card tests).

- [ ] **Step 5:** Stage the files and ask the user to commit: `fix(rancher): allow 32 MiB cluster list replies and report too-large replies clearly`

---

### Task 2: Real HTTP code and a WARN line for failed site lists

**Files:**
- Modify: `internal/clusters/list.go:78-91,116-129`
- Test: `internal/clusters/clusters_test.go`

**Interfaces:**
- Consumes: `*rancher.BadResponseError` with `.Code`, and `rancher.ErrBadResponse` (Task 1).
- Produces: no API change. `SiteList.HTTPCode` is now set for bad-response errors too.

- [ ] **Step 1: Write the failing tests**

```go
func TestList_BadResponseWarnsWithCode(t *testing.T) {
	fs := newFakes(
		[]ranchertest.FakeCluster{cl("c-1", "one")},
		[]ranchertest.FakeCluster{cl("c-2", "two")})
	fs[0].ListStatus = 200
	fs[0].ListBody = "<html>not json</html>"
	e := setup(t, fs)
	l := e.svc.List(context.Background(), e.sites)
	if s := l.Sites[0]; s.Status != "error" || s.HTTPCode != 200 { t.Fatalf("site-1 = %+v", s) }
	if got, want := ids(l), []string{"site-2/c-2"}; !eq(got, want) { t.Fatalf("rows = %v", got) }
	warns := 0
	for _, line := range strings.Split(strings.TrimSpace(e.logs.String()), "\n") {
		if strings.Contains(line, `"event":"clusters_list_error"`) {
			warns++
			for _, want := range []string{`"level":"WARN"`, `"site":"site-1"`, `"http_code":200`, `"error":"unexpected response from Rancher`} {
				if !strings.Contains(line, want) { t.Fatalf("missing %s in %s", want, line) }
			}
		}
	}
	if warns != 1 { t.Fatalf("warn lines = %d", warns) }
}
```
Extend `TestList_SiteUnauthorizedOthersStillListed`: exactly one `clusters_list_error` line, with `"site":"site-2"` and `"http_code":401`. Extend `TestList_SkipsSitesNotLoggedIn`: no `clusters_list_error` line. Extend `TestList_LogHasNoToken`: give one fake `ListStatus = 500` so a WARN line is present when the token check runs.

- [ ] **Step 2: Run them to make sure they fail**

Run: `go test ./internal/clusters/ -run 'TestList_'`
Expected: FAIL (`HTTPCode` 0, no `clusters_list_error` lines).

- [ ] **Step 3: Implement in `list.go`**

- In the error branch, add a `case` after the `StatusError` check, following `check.go:145-151`: `errors.As(err, &be)` → `sl.HTTPCode = be.Code`, then `sl.Error = auth.ShortErr(err)`.
- After `wg.Wait()` and the Info line, for each `l.Sites[i]` with `Status == "error"`, call `s.log.Warn("clusters", "event", "clusters_list_error", "site", sl.Site, "http_code", sl.HTTPCode, "ms", sl.Latency.Milliseconds(), "error", sl.Error)`. Logging after `Wait` keeps the order fixed (config order) and keeps logging out of the goroutines.

- [ ] **Step 4: Run the tests to make sure they pass**

Run: `go vet ./... && go test -race ./...`
Expected: PASS

- [ ] **Step 5:** Stage the files and ask the user to commit: `fix(clusters): log failed site lists at WARN with the real HTTP code`

---

### Task 3: Live-test docs for v0.3.2

**Files:**
- Modify: `docs/plans/PLAN2-live-test.md:22,29,30,58`
- Create: `docs/plans/PLAN3.2-live-test.md`

- [ ] **Step 1: Update `PLAN2-live-test.md` for `include_local`**

- `:22` L1 Expected: `... minus local` → `... minus local (unless clusters.include_local: true, then including it)`.
- `:29` L8 and `:30` L9: add `Skip if clusters.include_local: true` to the Do column.
- `:58`: same wording as `:22`.

- [ ] **Step 2: Write `PLAN3.2-live-test.md`** in the same structure and tone as `PLAN2-live-test.md` (intro, Before you start, Tests table, Report format, field table, failure lines, Never type). Image `v0.3.2` by digest. Read-only; nothing changes Rancher, RBAC or kagent. Tests:

| ID | Do this | Expected | Record |
| --- | --- | --- | --- |
| S1 | Log in, open `/home` | All 3 sites list. Each count matches the Rancher UI count, minus `local` unless `include_local` is on | Counts and list ms per site (from `/diagnostics`) |
| S2 | Per site, while logged in to that site's Rancher UI, open `https://<site>/v1/management.cattle.io.clusters` in the same browser with DevTools Network open | A JSON page loads | Per site: the response size (DevTools "Size", uncompressed/resource size) in KB. Shows how close each site is to the 32 MiB limit |
| S3 | Watch the portal log while doing S1 and a Refresh | No `event=clusters_list_error` lines | Pass/fail; on fail, the line's `site`, `http_code`, `ms`, `error` |
| S4 | Open a site-1 cluster known to run kagent | `kagent is ready` | Pass/fail, SSAR ms, card ms |
| S5 | On that cluster, run Plan 3 test C1 (short question) | Lines stream live, then an answer | Live `y`/`n`, first-event ms, events, total ms |

Report example:
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
`warn/error`: one line per `WARN` or `ERROR` log line, with its `event` and `site` (now including `clusters_list_error`). If S1 still fails for a site, copy that site's `clusters_list_error` fields. If its `error` says `exceeds 33554432 bytes`, the limit is still too small; if it says `decode ...: unexpected end of JSON input` or `read ...: unexpected EOF`, something in the network cut the reply short (a different cause).

- [ ] **Step 3:** Stage both files and ask the user to commit: `docs: live-test kit for v0.3.2`
- [ ] **Step 4: Handoff:** the user pushes, CI passes, the user tags `v0.3.2` and pushes the tag (CI builds the image), moves the image in by digest, runs `PLAN3.2-live-test.md` and brings back the report. If S1 passes, the Plan 3 live test (`PLAN3-live-test.md`, with `v0.3.2` in place of `v0.3.0`) can go ahead on all three sites.
