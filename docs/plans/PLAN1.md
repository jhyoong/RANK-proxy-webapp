# Plan 1: Login Flow Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A small, deployable portal that tests only the multi-site login flow: log in to the three Rancher sites with an AD admin account, show the result for each site, and log out of all of them. It is configured from one file, built by the existing CI, and runs in the target network to produce a live-test report.

**Architecture:** One Go binary that serves plain HTML forms (no JavaScript). It logs in to each Rancher site with `POST /v1-public/login`, keeps the tokens in an in-memory session, and shows a diagnostics page for each site: status, token id, `expiresAt`, and principal IDs from `GET /v3/users?me=true`. Logout calls `POST /v3/tokens?action=logout` on every site and then checks that each token is revoked. A fake Rancher (`internal/rancher/ranchertest`) backs the unit tests and a local `cmd/fakerancher` for manual click-through testing before the image goes into the network.

**Tech Stack:** Go 1.27.4 standard library (`net/http`, `html/template`, `embed`, `log/slog`) plus `go.yaml.in/yaml/v3` for the config file. Distroless image built by the existing `.github/workflows/image.yml`.

**Spec:** [docs/design.md](../design.md) (Authentication across three sites) with corrections from [docs/verification.md](../verification.md) (C3, C10, C11, D4, D6, D15, D16) and on-site findings (kept out of this public repo).

**Out of scope for Plan 1:** cluster listing, access checks, A2A chat, Redis, encryption at rest, rate limiting, NetworkPolicy, and 2 replicas. Each is a later plan.

## Global Constraints

- Login endpoint: `POST <site>/v1-public/login`, `Content-Type: application/json`, body `{"type":"activeDirectoryProvider","username":"…","password":"…","responseType":"json","ttl":<ms>}`. There is no `/v3-public` fallback: on-site checks showed it is not available.
- Login response: read the bearer only from the `token` field and treat it as opaque. Also read `id`, `userId` and `expiresAt` (verification C3). Never assume the `name:key` shape.
- `ttl` sent at login = `session.absolute_timeout` in milliseconds. The portal must not rely on Rancher's session TTL settings, so it asks for a bounded token.
- Logout: `POST <site>/v3/tokens?action=logout` with `Authorization: Bearer <token>` (verification C10).
- Identity: the principal ID with prefix `activedirectory_user://` from the user's `principalIds`. DNs contain spaces, parentheses and commas, e.g. `CN=Given Surname (Admin), OU=…,DC=…`. Compare principal IDs byte for byte and never normalise them.
- Lockout safety: one AD attempt per portal login. Site 1 (first in config) is tried first. **Any HTTP 4xx or 500** from a site's login ends the login with "login failed" and no other site is tried. Only transport errors (DNS, dial, TLS, timeout) and HTTP 502/503/504 fall through to the next site (verification D4, narrowed for safety).
- After the first success, log in to the remaining sites in parallel. If one of them rejects the credentials (4xx), log a `WARN` with `event=site_credential_drift`.
- TLS: verify every Rancher site against a configured CA file, or the system roots when `ca_file` is empty. No skip-verify option exists.
- Never log or render passwords, request bodies, or the token value. In logs and HTML the token is shown only as its `id` field.
- Session cookie: `portal_session`, `HttpOnly`, `Secure` (config `cookie_secure`, default `true`), `SameSite=Lax` (verification D16), `Path=/`, no `Max-Age`. Session ID is 32 random bytes in base64url. Defaults: idle 30m, absolute 8h.
- CSRF token on `POST /logout`.
- Security headers on every response: `Content-Security-Policy: default-src 'none'; style-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `Cache-Control: no-store`.
- Container: unchanged Dockerfile (distroless nonroot). Config is read from `-config` flag, else `PORTAL_CONFIG`, else `/etc/portal/config.yaml`.
- Repo rule: the implementer stages changes; **the user makes every commit and push** (no commits on their behalf). No secrets in the repo; example config uses placeholder hosts.

## Review Focus

1. **Bad password returns an unexpected status (400, 422, 500) at site 1.** The portal must still stop and not try site 2. Test: `TestLogin_Site1Non401ClientErrorStops` (Task 3).
2. **Usernames with odd characters** (`ORG\jdoe`, `Test User (Admin)`, `jdoe@example.com`, non-ASCII) must reach Rancher byte-identical in the JSON body. Test: `TestLogin_UsernamePassedVerbatim` (Task 2).
3. **The token value must not appear in logs or HTML.** Test: `TestHome_DoesNotLeakToken` and `TestLogin_LogsDoNotContainSecrets` (Tasks 5, 3).
4. **Stale cookies** (unknown ID, expired session, server restarted) must redirect to the login page, not cause an error or panic. Test: `TestHome_StaleCookieRedirects` (Task 5).
5. **A hanging site** must not block login past `login.timeout`. It is marked `unavailable` and the other sites still work. Test: `TestLogin_HangingSiteTimesOut` (Task 3).

---

## File Structure

| Path | Responsibility |
| --- | --- |
| `internal/config/config.go` | Load and validate the YAML config |
| `config.example.yaml` | Documented example config (placeholders only) |
| `internal/rancher/client.go` | One client per site: TLS, `Login`, `Me`, `Logout` |
| `internal/rancher/ranchertest/fake.go` | Fake Rancher used by tests and `cmd/fakerancher` |
| `internal/auth/auth.go` | Multi-site login ordering, drift warnings, principal choice, logout |
| `internal/session/store.go` | In-memory sessions with idle and absolute expiry |
| `internal/web/server.go` | Routes, cookie and CSRF handling, security headers |
| `internal/web/templates/*.html`, `internal/web/static/style.css` | Embedded UI |
| `cmd/portal/main.go` | Wiring, flags, TLS, graceful shutdown (modify existing) |
| `cmd/fakerancher/main.go` | Local fake with 3 sites for manual testing |
| `deploy/portal.yaml` | Example ConfigMap, Deployment and Service for the live test |
| `docs/plans/PLAN1-live-test.md` | On-site runbook and the report template to bring back |

---

### Task 1: Config loading

**Files:**
- Create: `internal/config/config.go`, `internal/config/config_test.go`, `config.example.yaml`
- Modify: `go.mod` / `go.sum` (`go get go.yaml.in/yaml/v3`)

**Interfaces:**
- Produces:
  ```go
  type Config struct {
      Listen       string        `yaml:"listen"`        // default ":8080"
      CookieSecure *bool         `yaml:"cookie_secure"` // nil → true; use (*Config).SecureCookies() bool
      TLS          TLSConfig     `yaml:"tls"`
      Session      SessionConfig `yaml:"session"`
      Login        LoginConfig   `yaml:"login"`
      Sites        []Site        `yaml:"sites"`         // order matters: Sites[0] is "site 1"
  }
  type TLSConfig struct{ CertFile, KeyFile string }     // yaml cert_file / key_file; both empty → plain HTTP
  type SessionConfig struct{ IdleTimeout, AbsoluteTimeout time.Duration } // idle_timeout 30m, absolute_timeout 8h
  type LoginConfig struct{ Timeout time.Duration }      // timeout, default 15s, per site request
  type Site struct{ Name, URL, CAFile string }          // name, url, ca_file
  func Load(path string) (*Config, error)
  func (c *Config) SecureCookies() bool
  ```

- [ ] **Step 1: Write failing tests** in `config_test.go`:
  - `TestLoad_Defaults`: minimal file with one site → `Listen==":8080"`, `SecureCookies()==true`, idle `30*time.Minute`, absolute `8*time.Hour`, login timeout `15*time.Second`.
  - `TestLoad_Durations`: `idle_timeout: 2m` → `2*time.Minute`.
  - `TestLoad_Rejects` (table): no sites; duplicate site names; empty name; `url: http://x` (must be `https`); URL with a path other than empty or `/`; `ca_file` missing; `ca_file` with no PEM certificate; idle > absolute; only one of `cert_file`/`key_file`; unknown top-level key (use `yaml.Decoder.KnownFields(true)`). Each → non-nil error that names the field.
  - `TestLoad_TrimsTrailingSlash`: `url: https://r.example/` → `Site.URL=="https://r.example"`.
- [ ] **Step 2:** Run `go test ./internal/config/` → FAIL (package does not exist).
- [ ] **Step 3: Implement `Load`** with `yaml.v3` decoding, which handles `time.Duration` strings, then defaults, then validation in the order above.
- [ ] **Step 4: Write `config.example.yaml`**: three sites `site-1/2/3` at `https://rancher-1.example.internal` etc. with `ca_file: /etc/portal/ca/site-N.pem`, every key present and commented, `cookie_secure: true`.
- [ ] **Step 5:** Run `go test ./internal/config/` → PASS. Add a test that `Load("../../config.example.yaml")` fails *only* because the CA files are missing. This keeps the example in sync with the schema.
- [ ] **Step 6:** Stage the files and ask the user to commit: `feat(config): load and validate portal config`.

---

### Task 2: Rancher site client and fake

**Files:**
- Create: `internal/rancher/client.go`, `internal/rancher/client_test.go`, `internal/rancher/ranchertest/fake.go`

**Interfaces:**
- Consumes: `config.Site`, `config.LoginConfig.Timeout`.
- Produces:
  ```go
  type Client struct{ Name string /* unexported: base URL, *http.Client */ }
  func NewClient(site config.Site, timeout time.Duration) (*Client, error) // builds tls.Config{RootCAs, MinVersion: TLS1.2}
  type Token struct {
      Bearer    string // opaque, from "token"; never log
      ID        string // from "id"
      UserID    string // from "userId"
      ExpiresAt string // from "expiresAt", kept raw
  }
  type User struct{ ID, Username string; PrincipalIDs []string } // from data[0] of /v3/users?me=true
  type StatusError struct{ Code int; Body string } // Body truncated to 200 bytes, for logs only
  func (e *StatusError) Error() string
  func (c *Client) Login(ctx context.Context, username, password string, ttl time.Duration) (Token, error)
  func (c *Client) Me(ctx context.Context, bearer string) (User, error)
  func (c *Client) Logout(ctx context.Context, bearer string) error
  ```
  Non-2xx → `*StatusError`. Transport failures → the wrapped `net/http` error, unchanged.

  Fake (`ranchertest`):
  ```go
  type Fake struct {
      Users       map[string]FakeUser // keyed by username
      LoginStatus int                 // if non-zero, login always returns this code
      MeStatus    int                 // if non-zero, /v3/users?me=true always returns this code
      Delay       time.Duration       // sleep before every response (respect r.Context())
      // unexported: mutex, issued tokens map, revoked set, call log
  }
  type FakeUser struct{ Password, ID string; PrincipalIDs []string }
  func (f *Fake) Handler() http.Handler
  func (f *Fake) LoginCalls() int
  func (f *Fake) LastLoginBody() map[string]any
  func NewTLS(t testing.TB, f *Fake) (url, caFile string) // httptest.NewTLSServer + write cert PEM to t.TempDir()
  ```
  Fake routes: `POST /v1-public/login` (requires `type=="activeDirectoryProvider"` and `responseType=="json"`, else 400; wrong credentials → 401; success → `{"token":"token-xxxxx:<random>","id":"token-xxxxx","userId":<ID>,"expiresAt":<now+ttl RFC3339>,"type":"token"}`). `GET /v3/users?me=true` (valid, unrevoked bearer → `{"data":[{"id","username","principalIds"}]}`, else 401). `POST /v3/tokens?action=logout` (revokes, 200; unknown → 401).

- [ ] **Step 1: Write failing tests** in `client_test.go` against `ranchertest.NewTLS`:
  - `TestLogin_Success`: returns Token with non-empty Bearer, `ID` matching the bearer prefix, `UserID=="u-abc"`. The fake saw `ttl == 3600000` for `time.Hour` and `Content-Type: application/json`.
  - `TestLogin_Rejected`: wrong password → `errors.As(err, &*StatusError)` with `Code==401`.
  - `TestLogin_UsernamePassedVerbatim`: for each of `ORG\jdoe`, `Test User (Admin)`, `jdoe@example.com`, `Tân`, `LastLoginBody()["username"]` equals the input exactly.
  - `TestMe_ReturnsPrincipals`: principal `activedirectory_user://CN=Test User (Admin), OU=Example Admins,DC=example,DC=com` round-trips byte-identical.
  - `TestLogout_RevokesToken`: after `Logout`, `Me` returns `StatusError{Code:401}`.
  - `TestNewClient_UntrustedCA`: client built with a *different* CA file → `Login` error, and it is **not** a `*StatusError`.
  - `TestLogin_Timeout`: `Fake.Delay = 2s`, client timeout `100ms` → error within 1s, not a `*StatusError`.
- [ ] **Step 2:** Run `go test ./internal/rancher/...` → FAIL.
- [ ] **Step 3: Implement the fake**, then `Client`. Use one `http.Client` per site with `Timeout` = login timeout. Build the URL from the base URL plus a fixed path, setting the query with `url.Values`. Decode the response with `json.Decoder` and cap the body at 1 MiB.
- [ ] **Step 4:** Run `go test -race ./internal/rancher/...` → PASS.
- [ ] **Step 5:** Stage the files and ask the user to commit: `feat(rancher): site client for login, me and logout, with fake`.

---

### Task 3: Multi-site login and logout

**Files:**
- Create: `internal/auth/auth.go`, `internal/auth/auth_test.go`

**Interfaces:**
- Consumes: `rancher.Client`, `rancher.Token`, `rancher.User`, `rancher.StatusError`.
- Produces:
  ```go
  type Status string
  const (
      StatusOK          Status = "ok"
      StatusRejected    Status = "rejected"    // 4xx or 500 from login
      StatusUnavailable Status = "unavailable" // transport error or 502/503/504
      StatusSkipped     Status = "skipped"     // not tried (login stopped earlier)
  )
  type SiteResult struct {
      Site      string
      Status    Status
      HTTPCode  int            // 0 if none
      Error     string         // short, safe for display; never contains token or body
      Token     rancher.Token  // only when StatusOK
      User      *rancher.User  // nil if Me failed
      MeError   string
      Latency   time.Duration
  }
  type Result struct {
      OK          bool           // at least one site StatusOK
      Sites       []SiteResult   // same order as config
      PrincipalID string         // "" if not found
      Warnings    []string       // human-readable
  }
  type LogoutResult struct{ Site string; LoggedOut bool; Revoked bool; Error string }
  type Authenticator struct{ /* clients []*rancher.Client, ttl, log *slog.Logger */ }
  func New(clients []*rancher.Client, ttl time.Duration, log *slog.Logger) *Authenticator
  func (a *Authenticator) Login(ctx context.Context, username, password string) Result
  func (a *Authenticator) Logout(ctx context.Context, sites []SiteResult) []LogoutResult
  ```

**Login algorithm** (the tests pin it):
1. Try sites one by one in config order. For each, call `Login`:
   - success → this is the first site; go to step 2.
   - `*StatusError` with `Code` in 400–499 or 500 → mark `rejected`, mark all others `skipped`, return `OK=false`.
   - transport error or 502/503/504 → mark `unavailable`, continue with the next site.
   - all sites unavailable → `OK=false`.
2. Log in to the remaining untried sites in parallel (`sync.WaitGroup`). Rejection → `rejected` plus a `WARN event=site_credential_drift site=<name>` log line and a warning. Unavailable → `unavailable`.
3. For every `ok` site, call `Me` in parallel. On error, set `MeError` and leave `User` nil.
4. Principal: from each `User.PrincipalIDs`, take the entry with prefix `activedirectory_user://`. If the sites disagree, add the warning `principal ID differs between sites`, log `WARN event=principal_mismatch`, and use the first site's value. If none is found, add the warning `no activedirectory_user principal found`.
5. Log one `INFO event=login` line with `username`, `principal`, and per site `status`, `http_code`, `token_id`, `expires_at`, `latency_ms`. Never log the password or bearer.

**Logout:** for each `ok` site in parallel, call `Logout(Bearer)` and then `Me(Bearer)`. `Revoked` is true iff `Me` returns `StatusError{401}` (or 403). Log one `INFO event=logout` line.

- [ ] **Step 1: Write failing tests** with 3 `ranchertest` fakes:
  - `TestLogin_AllSitesOK`: `OK`, 3× `ok`, `PrincipalID` set, no warnings.
  - `TestLogin_WrongPasswordOnlySite1Tried`: wrong password → `OK==false`, site 1 `rejected`, sites 2 and 3 `skipped`, `fake2.LoginCalls()==0 && fake3.LoginCalls()==0`.
  - `TestLogin_Site1Non401ClientErrorStops` (table: 400, 403, 422, 500): site 1 `LoginStatus=code` → sites 2 and 3 have 0 login calls.
  - `TestLogin_Site1DownFallsBack` (table: closed server URL, 502, 503, 504): site 1 `unavailable`, site 2 is used as the first site, site 3 is called. Result `OK`.
  - `TestLogin_Site2RejectsIsDrift`: site 2 has a different password → `OK`, site 2 `rejected`, a warning mentions site-2, and the log contains `site_credential_drift`.
  - `TestLogin_HangingSiteTimesOut`: site 3 `Delay` > client timeout → whole `Login` returns within timeout + 1s, site 3 `unavailable`, `OK`.
  - `TestLogin_PrincipalMismatchWarns`: site 2 user has a different principal case (`cn=` vs `CN=`) → warning present, `PrincipalID` is site 1's value.
  - `TestLogin_MeFailureStillOK`: site 2 fake `MeStatus=403` → site 2 `ok` with `MeError` set.
  - `TestLogin_LogsDoNotContainSecrets`: capture `slog` into a buffer → the output contains no password and no bearer secret part, but contains `token_id`.
  - `TestLogout_AllRevoked`: after `Logout`, every result is `LoggedOut && Revoked`, and skipped or rejected sites are not called.
- [ ] **Step 2:** Run `go test ./internal/auth/` → FAIL.
- [ ] **Step 3: Implement** per the algorithm above. Classify errors with `errors.As(err, &se)`.
- [ ] **Step 4:** Run `go test -race ./internal/auth/` → PASS.
- [ ] **Step 5:** Stage the files and ask the user to commit: `feat(auth): ordered multi-site login with lockout-safe fallback`.

---

### Task 4: In-memory session store

**Files:**
- Create: `internal/session/store.go`, `internal/session/store_test.go`

**Interfaces:**
- Consumes: `auth.SiteResult`.
- Produces:
  ```go
  type Session struct {
      ID, CSRF            string
      Username, Principal string
      Sites               []auth.SiteResult
      Warnings            []string
      Created, LastSeen   time.Time
  }
  func NewStore(idle, absolute time.Duration, now func() time.Time) *Store
  func (s *Store) Create(sess Session) *Session        // sets ID, CSRF (32 random bytes base64url each), Created, LastSeen
  func (s *Store) Get(id string) (*Session, bool)      // false if unknown or expired (and deletes it); else touches LastSeen
  func (s *Store) Delete(id string) (*Session, bool)   // returns the removed session so logout can use its tokens
  func (s *Store) Sweep()                              // deletes expired; main runs it every minute
  ```
  Expired means `now-LastSeen > idle || now-Created > absolute`.

- [ ] **Step 1: Write failing tests** with a controllable `now`:
  - `TestCreateGet`: round-trip, IDs are 43 chars, two `Create` calls give different IDs and CSRF values.
  - `TestIdleExpiry`: advance `idle+1s` → `Get` false. `TestIdleTouch`: Get at `idle-1s`, again at `idle-1s` later → still true.
  - `TestAbsoluteExpiry`: keep touching every minute until `absolute+1s` → false.
  - `TestDeleteReturnsSession`, `TestSweep`.
  - Run with `-race`, with 50 goroutines doing Create/Get/Delete.
- [ ] **Step 2:** Run `go test ./internal/session/` → FAIL.
- [ ] **Step 3: Implement** with a `sync.Mutex` and a `map[string]*Session`. `Get` returns a copy so handlers can't mutate the stored value.
- [ ] **Step 4:** Run `go test -race ./internal/session/` → PASS.
- [ ] **Step 5:** Stage the files and ask the user to commit: `feat(session): in-memory store with idle and absolute expiry`.

---

### Task 5: Web handlers and UI

**Files:**
- Create: `internal/web/server.go`, `internal/web/server_test.go`, `internal/web/templates/{layout,login,home,loggedout}.html`, `internal/web/static/style.css`

**Interfaces:**
- Consumes: `auth.Authenticator` (through the interface below), `session.Store`, `config.Config`.
- Produces:
  ```go
  type Authn interface {
      Login(ctx context.Context, username, password string) auth.Result
      Logout(ctx context.Context, sites []auth.SiteResult) []auth.LogoutResult
  }
  func New(cfg *config.Config, a Authn, store *session.Store, version string, log *slog.Logger) http.Handler
  ```

**Routes:**

| Route | Behaviour |
| --- | --- |
| `GET /` | Valid session → 303 to `/home`, else render the login form |
| `POST /login` | Form fields `username`, `password`. Empty fields → re-render "Enter username and password" (no Rancher call). `Result.OK` → create session, set cookie, 303 `/home`. All sites `unavailable` → 503 with "Rancher is unavailable, try again later". Otherwise → 401 with "Login failed". Limit the form to 4 KiB with `http.MaxBytesReader`. |
| `GET /home` | No or invalid session → 303 `/`. Renders principal, warnings, and a table per site: Site, Status, HTTP code, Token id, Expires at, Rancher user id, Principal IDs, Latency, Error. Includes a logout form with a hidden `csrf` field. |
| `POST /logout` | Session plus a matching `csrf` field (`subtle.ConstantTimeCompare`) → `store.Delete`, `Authn.Logout`, expire cookie, render the logged-out page with a table per site (Logged out / Revoked / Error). Bad CSRF → 403. No session → 303 `/`. |
| `GET /static/style.css` | Embedded stylesheet |
| `GET /healthz`, `GET /version` | As today |

All responses get the security headers from Global Constraints, through one middleware. Templates use `html/template` with `embed.FS`, parsed once at `New`.

- [ ] **Step 1: Write failing tests** using `httptest` and a stub `Authn`:
  - `TestLogin_SuccessSetsCookie`: POST → 303 `/home`. The `Set-Cookie` header has `portal_session`, `HttpOnly`, `Secure`, `SameSite=Lax`, and no `Max-Age`.
  - `TestLogin_Failed`: stub returns rejected → 401, body has "Login failed", no cookie.
  - `TestLogin_AllUnavailable` → 503 with the "unavailable" text.
  - `TestLogin_EmptyFieldsNoCall`: stub call count 0.
  - `TestHome_ShowsSiteTable`: contains each site name, token id `token-abc12`, and the principal text, HTML-escaped (the `(Admin)` and `,` in the DN render correctly).
  - `TestHome_DoesNotLeakToken`: stub bearer `token-abc12:SECRETVALUE` → body does not contain `SECRETVALUE`.
  - `TestHome_StaleCookieRedirects`: random cookie value → 303 `/`.
  - `TestLogout_RequiresCSRF`: wrong csrf → 403, session still valid.
  - `TestLogout_Success`: stub `Logout` was called with the session's sites, the cookie is expired (`Max-Age=0`), and the session is gone.
  - `TestSecurityHeaders`: every route returns the exact CSP string and `Cache-Control: no-store`.
  - `TestCookieSecureFalse`: `cookie_secure: false` → no `Secure` attribute.
- [ ] **Step 2:** Run `go test ./internal/web/` → FAIL.
- [ ] **Step 3: Implement** the handlers and templates. Keep the CSS minimal: readable table, red for `rejected`/`unavailable`, amber for warnings. Show the version in the footer.
- [ ] **Step 4:** Run `go test -race ./internal/web/` → PASS.
- [ ] **Step 5:** Stage the files and ask the user to commit: `feat(web): login, diagnostics and logout pages`.

---

### Task 6: Wire main, local fake, README

**Files:**
- Modify: `cmd/portal/main.go`, `README.md`, `.gitignore` (add `/fakerancher`, `/dev/`)
- Create: `cmd/fakerancher/main.go`

**Interfaces:**
- Consumes: everything above.
- `cmd/portal`: flag `-config` (default from env `PORTAL_CONFIG`, else `/etc/portal/config.yaml`). Load config, build one `rancher.Client` per site (fail fast on a bad CA), `auth.New(clients, cfg.Session.AbsoluteTimeout, log)`, `session.NewStore`, a ticker goroutine for `Sweep` every minute, and `web.New`. Use `ListenAndServeTLS` when `tls.cert_file` is set. Keep `ReadHeaderTimeout: 10s` and graceful shutdown. Logging uses the JSON handler on stdout. Startup log line: `version`, `listen`, `tls`, and each site's name and URL.
- `cmd/fakerancher`: flags `-dir ./dev` and `-base-port 9441`. Starts 3 fake sites (`site-1..3`) on 127.0.0.1:9441–9443 with `httptest.NewUnstartedServer` + `StartTLS`. Writes `dev/ca-site-N.pem` and `dev/config.yaml` pointing at them (`cookie_secure: false`, idle `2m`). Test users: `admin1`/`pass1` on all three sites; `drift`/`pass1` on sites 1 and 3 but `other` on site 2. Prints the command to run the portal.

- [ ] **Step 1: Write a failing test** `cmd/portal/main_test.go` `TestRun_StartsAndServesHealthz`: refactor `main` into `run(ctx context.Context, args []string, stdout io.Writer) error`. Start it with a temp config pointing at a `ranchertest` fake and `listen: 127.0.0.1:0`, expose the bound address (e.g. a `ready chan string` option or log parse), GET `/healthz` → 200, cancel ctx → `run` returns nil.
- [ ] **Step 2:** Run `go test ./cmd/portal/` → FAIL.
- [ ] **Step 3: Implement** `run`, then `cmd/fakerancher`.
- [ ] **Step 4: Manual smoke test:** run `go run ./cmd/fakerancher` and `go run ./cmd/portal -config dev/config.yaml`, then in a browser: log in as `admin1` → 3 green rows. Log out → 3× revoked. Log in as `drift` → site-2 rejected plus a drift warning. Wrong password → "Login failed", and the fakerancher log shows only site-1 hit. Wait 2m idle → redirected to login.
- [ ] **Step 5:** `go vet ./... && go test -race ./...` → PASS. `docker build -t portal:local .` succeeds.
- [ ] **Step 6: Update the README** with a "Run locally with the fake" section and a "Config file" section that points to `config.example.yaml`.
- [ ] **Step 7:** Stage the files and ask the user to commit: `feat: wire portal main and add local fake Rancher`.

---

### Task 7: Live-test kit

**Files:**
- Create: `deploy/portal.yaml`, `docs/plans/PLAN1-live-test.md`

**`deploy/portal.yaml`** (example, placeholders only):
- Namespace `kagent-portal`.
- ConfigMap `portal-config` with `config.yaml` (3 sites, `ca_file: /etc/portal/ca/site-N.pem`) and the CA PEMs as keys `site-N.pem`, mounted at `/etc/portal/config.yaml` (subPath) and `/etc/portal/ca/`.
- Deployment with 1 replica, `image: <internal-registry>/rank-proxy-webapp@sha256:<digest>`, `runAsNonRoot`, `readOnlyRootFilesystem`, `allowPrivilegeEscalation: false`, `capabilities.drop: [ALL]`, `seccompProfile: RuntimeDefault`, liveness and readiness on `/healthz`, and resources requests 50m/64Mi, limits 256Mi.
- Service on port 80 → 8080.
- No Ingress. The test uses `kubectl port-forward` (localhost counts as a secure context, so `Secure` cookies still work). Add a commented note: set `cookie_secure: false` only if testing over plain HTTP on a non-localhost name.

**`docs/plans/PLAN1-live-test.md`** contains:
1. **Before you start**: get the AD lockout threshold (E11) and use a test admin account where you can. One attempt per wrong-password test, never repeated quickly. Record the image digest in use.
2. **Steps** (each with its expected result):
   - T1 Startup: logs show 3 sites, no TLS errors.
   - T2 Good login: 3× `ok`. Record `expiresAt` minus login time, which should be about the absolute timeout (this checks Rancher honours `ttl`). Record whether the principal IDs are identical across sites.
   - T3 Wrong password, once: "Login failed". Portal log shows site-1 `rejected` and the others `skipped`. Record the HTTP code Rancher returned (confirms the 4xx assumption). Check the Rancher site 2/3 logs or audit for no attempt.
   - T4 Site 1 unreachable: edit the config so site-1 URL is an unroutable IP, restart, and do a good login. Site-1 `unavailable`, sites 2 and 3 `ok`, and the time taken is about the login timeout.
   - T5 Logout: 3× logged out and revoked. Optionally confirm in Rancher that the token is gone (`kubectl get tokens.management.cattle.io <id>` → NotFound).
   - T6 Idle expiry: set `idle_timeout: 2m`, log in, wait 3m, refresh → login page. Note that the Rancher tokens from an expired session remain until their `ttl`. Record this.
   - T7 Wrong CA: point one site's `ca_file` at another site's CA → that site shows a TLS error, and the others still work.
   - T8 Ingress check (optional, if an Ingress is added): same as T2 through the ingress URL.
3. **Report template**: a table with one row per T-step (result, HTTP codes, timings, redacted token ids, notes). Also: Rancher version per site, login response field names exactly as seen, the 2–3 redacted principal IDs as they appear on each site, and any log lines with `WARN`/`ERROR` (redacted).
4. **Redaction rules**: no passwords or token values. Token ids (`token-xxxxx`) are fine. Replace personal names in DNs with `<name>` but keep spacing and punctuation exact.

- [ ] **Step 1:** Write both files. Validate the manifest with `kubectl apply --dry-run=client -f deploy/portal.yaml` (or `kubeconform` if kubectl isn't available).
- [ ] **Step 2:** Stage the files and ask the user to commit: `docs: live-test kit for plan 1`.
- [ ] **Step 3: Handoff:** the user pushes to GitHub (CI builds, scans and pushes the image), tags `v0.1.0`, moves the image into the network with the README's `skopeo` steps, runs `PLAN1-live-test.md`, and brings back the filled report. Results feed Plan 2 (cluster listing + access check).
