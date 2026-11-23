# Plan 4: Security Hardening (v0.4.0) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the gaps between the running portal and the "Security controls" table in [design.md](../design.md), so that a reasonably skilled attacker finds no obvious exploits. Ship it as v0.4.0 with a live-test runbook for the security checks.

**Architecture:** Five code changes and one runbook. (1) A new `internal/ratelimit` package limits login attempts per username and per client IP before any call to Rancher. (2) The session store hands back ended sessions (expired, evicted or shut down) so their Rancher tokens get revoked, and it caps sessions per user. (3) The session cookie becomes `SameSite=Strict`, and Go's `http.CrossOriginProtection` wraps the whole mux, which covers the login form too. (4) More security headers, a capped header size, an idle timeout, and per-handler read deadlines. (5) A NetworkPolicy, `govulncheck` in CI, and the base image pinned by digest. Sessions stay in memory on a single replica.

**Tech Stack:** Go 1.27.4 standard library (`net/http`, `net/netip`). No new Go module dependencies. CI adds `golang.org/x/vuln/cmd/govulncheck`, run with `go run`.

**Spec:** No separate design doc. The source is the "Security controls", "Authentication" and "Login protection" sections of [design.md](../design.md), compared with the code on `main` at `987ebe4` (v0.3.2). Decided with the user: **close the gaps only**. No Redis/Valkey, no encryption at rest, no second replica; those stay in design.md's Phase 2.

Gaps found (the code's current state is in brackets):

| # | Design control | Gap |
|---|---|---|
| G1 | Rate limits per username and per source IP | [none] Anyone who can reach `/login` can lock out an admin's AD account. |
| G2 | Logout deletes tokens; timeouts no longer than Rancher's | [`Store.Sweep`/`Get` drop expired sessions without revoking their tokens] The tokens stay valid until their 8h TTL. A pod restart orphans every token. |
| G3 | Cookie `SameSite=Strict`; CSRF on all state-changing requests | [`Lax`; `/login` has no CSRF] A cross-site page can make a victim's browser post logins. |
| G4 | Browser hardening | [no HSTS, COOP or X-Frame-Options] |
| G5 | Server robustness (implied) | [`http.Server` sets only `ReadHeaderTimeout`; no idle timeout or header cap; no limit on sessions per user] |
| G6 | NetworkPolicy; supply chain | [no NetworkPolicy in `deploy/portal.yaml`; no `govulncheck`; distroless base pulled by tag] |

## Prerequisite

- **The v0.3.2 live test ([PLAN3.2-live-test.md](PLAN3.2-live-test.md)) passes first.** If site 1's cluster list still fails, fix that before starting this plan. PLAN4 builds on `main` at v0.3.2.

## Global Constraints

- Login limits (config `login:` block): `max_attempts_per_user: 3`, `max_attempts_per_ip: 20`, `attempt_window: 15m`, `trusted_proxies: []` (a list of CIDRs). These defaults are applied in `config.applyDefaults`. Negative values fail validation. In `web.build`, a value of 0 means no limit; only tests that skip `config.Load` reach that.
- A username's key is `session.UserKey(username)`. It lowercases and trims, removes everything up to the last `\`, and removes everything from the first `@`, so `CORP\jdoe`, `jdoe@corp.example` and `JDoe` share one bucket. The session cap uses the lowercased AD Principal when there is one, and `UserKey` otherwise. (Amended after the final review.)
- A blocked login makes **no call to Rancher**. It redirects to `/?e=limited`, which shows: `Too many login attempts; try again later`.
- Session cap: `session.max_per_user: 5` (the default). When a user logs in a 6th time, their oldest session is ended and its tokens are revoked.
- Cookie: `HttpOnly`, `Secure` per `cookie_secure`, **`SameSite=Strict`**.
- New headers on every response: `X-Frame-Options: DENY` and `Cross-Origin-Opener-Policy: same-origin`. When `cfg.SecureCookies()` is true, also send `Strict-Transport-Security: max-age=31536000`. The existing CSP and other headers don't change.
- `http.Server`: keep `ReadHeaderTimeout: 10s`; add `IdleTimeout: 120s` and `MaxHeaderBytes: 64 << 10`. **Do not set `ReadTimeout` or `WriteTimeout`.** A server-wide read deadline that fires during a 30-minute SSE reply cancels the request context, and a write timeout would cut off the reply.
- New log events (JSON, like the existing ones). Never log a password, bearer token or message text:
  - WARN `event=login_rate_limited username=<u> ip=<ip> scope=user|ip`
  - WARN `event=cross_origin_blocked method=<m> path=<p> ip=<ip>`
  - INFO `event=session_ended reason=expired|evicted|relogin|shutdown username=<u> principal=<p>`, followed by the existing `event=logout` line from `auth.Logout`.
- Repo rule: the implementer stages changes. **The user makes every commit, push and tag.** The commit steps below mean "stage and hand back".
- Version: v0.4.0. The user cuts it by pushing tag `v0.4.0`. There is no source change for the version.

## Review Focus

1. **Logins racing in parallel.** Ten simultaneous wrong-password POSTs for one username must reach Rancher at most 3 times. The attempt is counted *before* the Rancher call, not after it fails. Test: `TestLogin_ParallelAttemptsCapped` (Task 1).
2. **Behind an ingress.** `RemoteAddr` is the ingress pod, so without `trusted_proxies` every user shares one IP bucket. With it set, a spoofed `X-Forwarded-For` from an untrusted peer must be ignored. Tests: `TestClientIP_*` (Task 1).
3. **Rancher down is not a failed password.** A login where every site is `unavailable` gives the username attempt back, so an outage doesn't lock users out of the portal. Test: `TestLogin_UnavailableRefundsUserAttempt` (Task 1).
4. **Same-origin POSTs still work behind the ingress.** `CrossOriginProtection` passes a request when `Sec-Fetch-Site: same-origin` is set or `Origin` matches `Host`. Requests with neither header (curl, the test harness) pass. Tests: `TestCrossOrigin_*` (Task 3). The live test confirms this through the real ingress (Task 6).
5. **An ended session revokes only its own tokens, exactly once.** If a session expires in `Get` and is then swept, it comes back from `Sweep` only once. Evicting one session leaves the user's other sessions alone. Tests: `TestSweep_ReturnsEndedOnce`, `TestCreate_EvictsOldestPerUser` (Task 2).

## File Structure

- Create `internal/ratelimit/ratelimit.go` and `ratelimit_test.go`: a fixed-window attempt counter.
- Create `internal/web/clientip.go` and `clientip_test.go`: client IP behind trusted proxies.
- Modify `internal/config/config.go` and `config_test.go`, `config.example.yaml`, `deploy/portal.yaml` (ConfigMap): new `login.*` and `session.max_per_user` keys.
- Modify `internal/session/store.go` and `store_test.go`: the ended queue, `End`, `Drain`, the per-user cap.
- Create `internal/web/revoke.go`, plus a test in `server_test.go`: revoke tokens of ended sessions.
- Modify `internal/web/server.go`, `internal/web/chat.go`, `internal/web/templates/login.html` (no change expected), `internal/web/server_test.go`.
- Modify `cmd/portal/main.go` and `main_test.go`: wiring, server settings, the revoke loop and the shutdown drain.
- Modify `deploy/portal.yaml`, `.github/workflows/image.yml`, `Dockerfile`, `README.md`.
- Create `docs/plans/PLAN4-live-test.md`.

---

### Task 1: Login rate limiting (G1)

**Files:**
- Create: `internal/ratelimit/ratelimit.go`, `internal/ratelimit/ratelimit_test.go`
- Create: `internal/web/clientip.go`, `internal/web/clientip_test.go`
- Modify: `internal/config/config.go` (`LoginConfig`, `applyDefaults`, `validate`), `internal/config/config_test.go`, `config.example.yaml`, `deploy/portal.yaml`
- Modify: `internal/web/server.go` (`server` struct, `build`, `login`, `loginErrors`), `internal/web/server_test.go`

**Interfaces:**
- Produces: `package ratelimit`
  - `func New(limit int, window time.Duration, now func() time.Time) *Limiter`. If `limit <= 0`, every call to `Reserve` returns true.
  - `func (l *Limiter) Reserve(key string) bool`: counts one attempt. It returns false without counting if `key` already has `limit` attempts in its current window. Each counted attempt restarts the key's window, so a key resets only once `window` has passed since its last counted attempt. This mirrors AD's reset-after-quiet-period. (A refused call doesn't count, so it doesn't restart the window.)
  - `func (l *Limiter) Release(key string)`: gives one attempt back, never going below 0.
  - `func (l *Limiter) Reset(key string)`: forgets the key.
  - No sweep method to wire up. `Reserve` itself drops expired keys when at least `window` has passed since the last sweep, so the map holds only keys that are still in their window.
- Produces: `config.LoginConfig` gains `MaxAttemptsPerUser int \`yaml:"max_attempts_per_user"\``, `MaxAttemptsPerIP int \`yaml:"max_attempts_per_ip"\``, `AttemptWindow time.Duration \`yaml:"attempt_window"\`` and `TrustedProxies []string \`yaml:"trusted_proxies"\``. `validate` parses each entry with `netip.ParsePrefix` and fails on a bad one.
- Produces: `func clientIP(r *http.Request, trusted []netip.Prefix) netip.Addr` in `internal/web/clientip.go`. If the peer address from `RemoteAddr` is not inside `trusted`, the result is the peer address. Otherwise it walks `X-Forwarded-For` from right to left and returns the first address not inside `trusted`. If every entry is trusted, it returns the leftmost one. Entries that don't parse end the walk and give back the last good address.
- Produces: `web.New` and `web.build` keep their signatures. `build` creates `userLim` and `ipLim` (fields on `server`) from `cfg.Login`.

- [ ] **Step 1: Write the failing tests**
  - `ratelimit_test.go`:
    - `TestReserve_LimitAndWindow`: with limit 3 and window 15m, three `Reserve("a")` calls return true and the fourth returns false. After the clock moves 15m+1ns, `Reserve` returns true again.
    - `TestReserve_KeysIndependent`.
    - `TestRelease_GivesBackOne`: Release never drives the count below 0.
    - `TestReset`.
    - `TestReserve_SweepsExpiredKeys`: after the window passes, one `Reserve` on a new key leaves only that key. Check it with a test-only `Len()` in `export_test.go`.
    - `TestZeroLimitAllowsAll`.
    - `TestReserve_Concurrent`: 100 goroutines with limit 3 give exactly 3 trues. Run it under `-race`.
  - `clientip_test.go`:
    - `TestClientIP_NoTrustedUsesRemoteAddr`: the XFF value `6.6.6.6` is ignored.
    - `TestClientIP_TrustedPeerUsesXFF`: peer `10.0.0.5`, trusted `10.0.0.0/8`, XFF `203.0.113.7, 10.0.0.9` → `203.0.113.7`.
    - `TestClientIP_SpoofedLeftmostIgnored`: XFF `6.6.6.6, 203.0.113.7` with trusted peer → `203.0.113.7`.
    - `TestClientIP_UntrustedPeerXFFIgnored`.
  - `config_test.go`:
    - `TestLoginDefaults`: 3, 20 and 15m.
    - `TestLoginNegativeRejected`.
    - `TestTrustedProxiesBadCIDRRejected`.
  - `server_test.go`. Set `Login: {MaxAttemptsPerUser: 3, MaxAttemptsPerIP: 20, AttemptWindow: 15*time.Minute}` in `newHarness`.
    - `TestLogin_UserLimit`: with a failing stub, 3 POSTs give `loginCalls == 3`. The 4th redirects to `/?e=limited` and `loginCalls` stays 3. `GET /?e=limited` shows `Too many login attempts; try again later`.
    - `TestLogin_UsernameCaseFolded`: `JDoe`, `jdoe` and ` jdoe ` share one bucket.
    - `TestLogin_IPLimit`: 20 different usernames from one IP; the 21st is limited, and `loginCalls == 20`.
    - `TestLogin_SuccessResetsUser`: 2 failures, then a success, then 3 more failures, all allowed.
    - `TestLogin_UnavailableRefundsUserAttempt`: the stub returns all sites `StatusUnavailable`. Five POSTs all reach the stub, because the IP limit is 20.
    - `TestLogin_ParallelAttemptsCapped`: 10 goroutines POST at once with a failing stub that sleeps 50ms. `loginCalls <= 3`. Make the stub's counter safe for concurrent use.
    - `TestLogin_EmptyFieldsNotCounted`: empty-field posts don't use up attempts.
    - `TestLogin_RateLimitLogged`: capture the log. One `login_rate_limited` line with `scope=user`, and no `pw` value anywhere in the log.

- [ ] **Step 2: Run the tests and check they fail**

Run: `go test ./internal/ratelimit/ ./internal/config/ ./internal/web/ -run 'Reserve|Release|Reset|ZeroLimit|ClientIP|Login' -race`
Expected: they FAIL to compile (`ratelimit`, `clientIP` and the `LoginConfig` fields are undefined).

- [ ] **Step 3: Implement**
  - `ratelimit`: a `map[string]*entry{count int; start time.Time}` under one mutex.
  - Order inside `server.login`, after the empty-field check:
    1. `ip := clientIP(r, s.trusted)`. If `!ipLim.Reserve(ip.String())`, log with scope ip and redirect to `limited`.
    2. Do the same with `userLim.Reserve(key)` and scope user.
    3. Call `authn.Login`.
    4. On `res.OK`, call `userLim.Reset(key)`. On `failureCode(res) == "unavailable"`, call `userLim.Release(key)`.
  - Add `"limited": "Too many login attempts; try again later"` to `loginErrors`.
  - Parse `cfg.Login.TrustedProxies` once in `build`.
  - Document the four keys in `config.example.yaml`. The comment should say: keep `max_attempts_per_user` below the AD lockout threshold minus the attempts people make in the Rancher UI directly, and set `trusted_proxies` to the ingress controller's pod CIDR. Mirror the keys in the `deploy/portal.yaml` ConfigMap, with `trusted_proxies: [<ingress-pod-cidr>]`.

- [ ] **Step 4: Run the tests and check they pass**

Run: `go test -race ./...`
Expected: PASS.

- [ ] **Step 5: Stage for the user**

```bash
git add internal/ratelimit internal/web/clientip.go internal/web/clientip_test.go internal/config internal/web/server.go internal/web/server_test.go config.example.yaml deploy/portal.yaml
```

---

### Task 2: Revoke tokens of ended sessions; cap sessions per user (G2, G5)

**Files:**
- Modify: `internal/session/store.go`, `internal/session/store_test.go`
- Modify: `internal/config/config.go` (`SessionConfig.MaxPerUser int \`yaml:"max_per_user"\``, default 5, negative rejected), `internal/config/config_test.go`, `config.example.yaml`, `deploy/portal.yaml`
- Create: `internal/web/revoke.go`
- Modify: `internal/web/server.go` (`login`: end any current session on re-login), `internal/web/server_test.go`
- Modify: `cmd/portal/main.go`, `cmd/portal/main_test.go`

**Interfaces:**
- Produces in `session`:
  - `func NewStore(idle, absolute time.Duration, maxPerUser int, now func() time.Time) *Store`. If `maxPerUser <= 0`, there is no cap. Update every caller: `main.go`, `server_test.go` and `store_test.go`.
  - `type Ended struct { Session *Session; Reason string }`. The reasons are `"expired"`, `"evicted"`, `"relogin"` and `"shutdown"`.
  - `Get` and `SetClusters`: when they find an expired session, they move it to an internal ended queue instead of discarding it.
  - `Create`: if the user (case-folded `Username`) already has `maxPerUser` live sessions, it moves the oldest to the queue with reason `"evicted"` before storing the new one.
  - `func (s *Store) End(id, reason string)`: moves a session to the queue. It does nothing if the ID is unknown.
  - `func (s *Store) Sweep() []Ended`: moves expired sessions to the queue, then empties the queue and returns its contents. Each session comes back exactly once.
  - `func (s *Store) Drain() []Ended`: removes every remaining session with reason `"shutdown"`, plus whatever is in the queue.
  - `Delete` is unchanged. Logout already revokes the tokens synchronously.
- Produces in `web`: `func Revoke(ctx context.Context, a Authn, ended []session.Ended, log *slog.Logger)`. For each ended session, it logs `event=session_ended` and calls `a.Logout(ctx, e.Session.Sites)`, with at most 8 sessions in flight. It returns when all are done.
- Consumes: `Authn.Logout` (exists).

- [ ] **Step 1: Write the failing tests**
  - `store_test.go`:
    - `TestSweep_ReturnsExpiredWithReason`.
    - `TestSweep_ReturnsEndedOnce`: an expired session is hit by `Get` (which returns false), then `Sweep` returns it once, and a second `Sweep` returns nothing.
    - `TestCreate_EvictsOldestPerUser`: with max 2, three creates for `JDoe` and `jdoe` leave the 1st evicted and the 2nd and 3rd still working. Another user's session is untouched.
    - `TestEnd_Relogin`.
    - `TestDrain_AllShutdown`.
    - Update the existing tests for the new `NewStore` arity.
  - `server_test.go`:
    - `TestRevoke_CallsLogoutPerSession`: the stub's `Logout` is called once per ended session with that session's sites. The log has `session_ended` with the reason and no bearer value.
    - `TestLogin_ReloginEndsOldSession`: log in, then POST `/login` again with the first cookie. The old cookie no longer opens `/home`, and `hs.store.Sweep()` returns it with reason `relogin`.
  - `main_test.go`:
    - `TestRun_ShutdownRevokesSessions`: start `run` against a `ranchertest` fake and log in through HTTP. Cancel `ctx`, then assert the fake saw a logout (token delete) for the session's token. If the fake does not record deletes, add a counter to `ranchertest.Fake`.

- [ ] **Step 2: Run the tests and check they fail**

Run: `go test -race ./internal/session/ ./internal/web/ ./cmd/portal/`
Expected: compile FAIL (`NewStore` arity, `Ended`, `Revoke`).

- [ ] **Step 3: Implement**
  - In `main.go`, the existing one-minute ticker becomes: `ended := store.Sweep(); go web.Revoke(ctxWithTimeout(30s), authn, ended, log)`. Keep the `*auth.Authenticator` in a variable so both `web.New` and `Revoke` use it.
  - On `ctx.Done()`, after `srv.Shutdown`, call `web.Revoke` on `store.Drain()` with a fresh 10s context, so the total stays inside Kubernetes' default 30s grace period.
  - In `server.login`, when `s.current(r)` finds a session, call `s.store.End(old.ID, "relogin")` before `Create`.
  - Document `session.max_per_user` in `config.example.yaml` and the ConfigMap.

- [ ] **Step 4: Run the tests and check they pass**

Run: `go test -race ./...`
Expected: PASS.

- [ ] **Step 5: Stage for the user**

```bash
git add internal/session internal/config internal/web cmd/portal config.example.yaml deploy/portal.yaml
```

---

### Task 3: Strict cookie and cross-origin protection (G3)

**Files:**
- Modify: `internal/web/server.go` (`setCookie`, `build`)
- Modify: `internal/web/server_test.go`

**Interfaces:**
- Consumes: the client IP from Task 1 (`clientIP`) for the deny log.
- Produces: `build` returns `securityHeaders(cop.Handler(mux))`, where `cop := http.NewCrossOriginProtection()` and `cop.SetDenyHandler(...)` logs `event=cross_origin_blocked` and answers `403 cross-origin request blocked`. The per-form CSRF tokens stay as they are; this is defence in depth, not a replacement.

- [ ] **Step 1: Write the failing tests**
  - `TestCookie_SameSiteStrict`: the login cookie's `SameSite == http.SameSiteStrictMode`. Update `TestCookieSecureFalse` if it asserts `Lax`.
  - `TestCrossOrigin_LoginBlocked`: POST `/login` with `Sec-Fetch-Site: cross-site` gives 403, and `stub.loginCalls == 0`.
  - `TestCrossOrigin_OriginMismatchBlocked`: POST `/logout` with `Origin: https://evil.example`, `Host: portal.example` and no `Sec-Fetch-Site` gives 403.
  - `TestCrossOrigin_SameOriginAllowed`: `Sec-Fetch-Site: same-origin` on POST `/login` gives 303 to `/home`.
  - `TestCrossOrigin_NoHeadersAllowed`: the existing harness requests carry neither header and still pass. The existing suite covers this; add one explicit test.
  - `TestCrossOrigin_GetUnaffected`: a GET `/home` with `Sec-Fetch-Site: cross-site` and a valid cookie gives 200. The browser won't send a Strict cookie cross-site, but the server must not block safe methods.
  - `TestCrossOrigin_Logged`: one `cross_origin_blocked` line.

- [ ] **Step 2: Run the tests and check they fail**

Run: `go test -race ./internal/web/ -run 'Cookie|CrossOrigin'`
Expected: FAIL (SameSite is Lax; cross-site POSTs reach the stub).

- [ ] **Step 3: Implement** as described in Interfaces.

- [ ] **Step 4: Run the tests and check they pass**

Run: `go test -race ./...`
Expected: PASS.

- [ ] **Step 5: Stage for the user**

```bash
git add internal/web/server.go internal/web/server_test.go
```

---

### Task 4: Security headers and HTTP server limits (G4, G5)

**Files:**
- Modify: `internal/web/server.go` (`securityHeaders` takes `secure bool`; `login` sets a read deadline)
- Modify: `internal/web/chat.go` (read deadline around the body decode)
- Modify: `cmd/portal/main.go` (`http.Server` fields)
- Modify: `internal/web/server_test.go`, `cmd/portal/main_test.go`

**Interfaces:**
- Produces: `func securityHeaders(next http.Handler, secure bool) http.Handler`.
- Produces: `const bodyReadTimeout = 10 * time.Second` in `web`. `login`, `refresh`, `logout` and `chat` call `http.NewResponseController(w).SetReadDeadline(s.now().Add(bodyReadTimeout))` before reading the body. `chat` clears it with `SetReadDeadline(time.Time{})` after decoding and before `Send`, so the SSE reply isn't cut. Ignore `http.ErrNotSupported`; `httptest.ResponseRecorder` returns it.

- [ ] **Step 1: Write the failing tests**
  - Extend `TestSecurityHeaders`: `X-Frame-Options: DENY` and `Cross-Origin-Opener-Policy: same-origin` are set, and `Strict-Transport-Security: max-age=31536000` is set when `secure=true`.
  - `TestSecurityHeaders_NoHSTSWhenInsecure`.
  - `TestRun_ServerLimits`: start `run` and send a request with a header over 64 KiB. Expect status 431.
  - `TestChat_LongReplyNotCutByReadDeadline`: through a real `httptest.Server` (reuse `liveChat`), the stub's Send streams for longer than a shortened `bodyReadTimeout`, and the reply completes with `done`. To make this testable, make `bodyReadTimeout` a package var.

- [ ] **Step 2: Run the tests and check they fail**

Run: `go test -race ./internal/web/ ./cmd/portal/ -run 'SecurityHeaders|ServerLimits|ReadDeadline'`
Expected: FAIL.

- [ ] **Step 3: Implement.** In `main.go`: `&http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10}`. Add a comment saying why there is no `ReadTimeout` or `WriteTimeout` (see Global Constraints).

- [ ] **Step 4: Run the tests and check they pass**

Run: `go test -race ./...`
Expected: PASS.

- [ ] **Step 5: Stage for the user**

```bash
git add internal/web cmd/portal
```

---

### Task 5: NetworkPolicy, govulncheck, pinned base image (G6)

**Files:**
- Modify: `deploy/portal.yaml`
- Modify: `.github/workflows/image.yml`
- Modify: `Dockerfile`
- Modify: `README.md`

**Interfaces:** none (deploy, CI and docs only).

- [ ] **Step 1: Add the NetworkPolicy to `deploy/portal.yaml`.** It selects `app: portal` with `policyTypes: [Ingress, Egress]`:
  - Ingress: TCP 8080 only, from `namespaceSelector` `<ingress-controller-namespace>`. Add a comment: during a `kubectl port-forward` live test, port-forward goes through the kubelet and is not subject to NetworkPolicy.
  - Egress:
    - TCP 443 to `ipBlock` `<rancher-site-N-cidr>` for each of the three sites.
    - UDP and TCP 53 to `kube-system` pods labelled `k8s-app: kube-dns`.
  - Nothing else. Next to the Deployment, also add `terminationGracePeriodSeconds: 30` with a comment saying shutdown revokes tokens (Task 2).
- [ ] **Step 2: Check the manifest.** Run `kubectl apply --dry-run=client -f deploy/portal.yaml`, or, with no kubectl, `python3 -c 'import yaml,sys; list(yaml.safe_load_all(open("deploy/portal.yaml")))'`. Expected: no error.
- [ ] **Step 3: Add govulncheck to CI.** In `.github/workflows/image.yml`, `test` job, after `go vet`: `- run: go run golang.org/x/vuln/cmd/govulncheck@<pinned vX.Y.Z> ./...`. Pin the latest release tag on the day you implement this. The job fails if a called function is vulnerable.
- [ ] **Step 4: Run govulncheck locally.** Run: `go run golang.org/x/vuln/cmd/govulncheck@<same version> ./...`. Expected: `No vulnerabilities found.` If it reports any, bump the dependency or Go patch version in this task and note it for the user.
- [ ] **Step 5: Pin the distroless base by digest.** Change the `Dockerfile` line to `FROM gcr.io/distroless/static-debian12:nonroot@sha256:<digest>`, with the current digest from `docker buildx imagetools inspect gcr.io/distroless/static-debian12:nonroot`. Keep the tag for readability. Run `docker build -t portal:local .` and expect it to succeed.
- [ ] **Step 6: Add a "Security" section to `README.md`.** It covers the login limits and `trusted_proxies`, `session.max_per_user`, token revocation on expiry and shutdown, the NetworkPolicy placeholders, and the reminder that the ingress must terminate TLS (HSTS and `Secure` cookies assume HTTPS).
- [ ] **Step 7: Stage for the user**

```bash
git add deploy/portal.yaml .github/workflows/image.yml Dockerfile README.md
```

---

### Task 6: v0.4.0 security live-test runbook

**Files:**
- Create: `docs/plans/PLAN4-live-test.md`

**Interfaces:** none. Follow the shape of [PLAN3.2-live-test.md](PLAN3.2-live-test.md): prerequisites, numbered checks, and a one-line-per-check report template like `docs/plan3-report.txt`.

- [ ] **Step 1: Write the runbook with these checks.** Each one names the command or browser action and the expected result:
  - **S1 Headers.** `curl -sI https://<portal>/` shows the CSP, HSTS, `X-Frame-Options: DENY`, COOP, nosniff and no-referrer headers. In browser devtools, the cookie shows `HttpOnly`, `Secure` and `SameSite=Strict`.
  - **S2 User limit.** Use a **test account, not a real admin**. Three wrong passwords followed by a fourth attempt show `Too many login attempts`. The fakerancher or Rancher audit log shows 3 site-1 attempts, not 4. The portal log shows `login_rate_limited scope=user`.
  - **S3 IP behind ingress.** After S2, a different user on a different machine can still log in. This proves `trusted_proxies` works and users aren't sharing the ingress IP. A `curl` with a forged `X-Forwarded-For` from outside does not bypass the limit.
  - **S4 Cross-origin.** `curl -X POST -H 'Sec-Fetch-Site: cross-site' https://<portal>/login -d username=x -d password=y` gives 403, and the log shows `cross_origin_blocked`. A normal browser login through the real ingress still works; this checks that `Origin`/`Host` pass through the ingress.
  - **S5 Expiry revokes.** With `session.idle_timeout: 2m` in a test ConfigMap: log in, copy the token ID from `/diagnostics`, and wait 3 minutes. The log shows `session_ended reason=expired`, and Rancher shows that token is gone (`kubectl get tokens.ext.cattle.io` or the Rancher UI → API Keys).
  - **S6 Session cap.** Log in 6 times as the same user from private windows. The first window is sent back to login, and the log shows `reason=evicted`.
  - **S7 Restart revokes.** Log in, then `kubectl rollout restart`. The old pod's log shows `session_ended reason=shutdown` and an `event=logout` with `revoked=true`.
  - **S8 NetworkPolicy.** From the portal pod's namespace, a debug pod with the `app: portal` label can't reach anything other than the three Rancher hosts and DNS (for example, `curl https://example.com` times out). The portal still lists clusters and chats.
  - **S9 Chat unaffected.** A chat reply longer than 10s (the body read deadline) completes. A `<site>-stg-20`-style silent wait still works.
  - **S10 Regression.** Repeat PLAN3 C1, C2 and C4 quickly.
- [ ] **Step 2: Do a dry run against the fake.** Run `go run ./cmd/fakerancher` + `go run ./cmd/portal -config dev/config.yaml` and work through S1, S2, S4, S5 and S6 locally. The fake config uses `cookie_secure: false`, so S1's HSTS and `Secure` checks don't apply there; note that in the runbook. Fix anything in the runbook that doesn't match what you see.
- [ ] **Step 3: Stage for the user**

```bash
git add docs/plans/PLAN4-live-test.md
```

---

## Open items for the user (before the live test)

1. **AD lockout threshold and observation window.** The default `max_attempts_per_user: 3` per 15m assumes a threshold of at least 5. Wrong passwords typed straight into Rancher's UI also count toward AD lockout.
2. **The ingress controller's pod CIDR**, for `trusted_proxies`, and its namespace, for the NetworkPolicy ingress rule.
3. **The Rancher sites' IPs or CIDRs**, for the NetworkPolicy egress rules. If they sit behind a load balancer with changing IPs, a CIDR-based policy may not fit, and egress may need to stay open on 443.
