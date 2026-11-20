# Plan 1 Live Test: Login Flow

Run this inside the target network with the Plan 1 image, then bring back the filled [report](#report-template). It tests only login, per-site diagnostics and logout against the three real Rancher sites. Nothing here changes Rancher settings, RBAC or kagent.

## Before you start

- [ ] **AD lockout.** Find the lockout threshold and window (verification E11). A wrong password normally costs **one** AD attempt: site 1 answers and the portal stops. The exception is when site 1 times out or returns 502/503/504; then the portal tries site 2, which is a second attempt with the same password. Use a test admin account if one exists. Do each wrong-password test **once**. Click "Log in" once only: a double-click sends two logins. (A refresh after a failed login is safe; the portal redirects, so the password isn't re-sent.)
- [ ] **Image.** Move the image in using the README's `skopeo` steps. Record the digest. Deploy by digest.
- [ ] **CA files.** Get the full CA chain for each Rancher site's hostname into `deploy/portal.yaml` (`site-N.pem`).
- [ ] **Site order.** The first site in `sites:` is "site 1". Put the site that every user can authenticate on first (verification E20).
- [ ] **Manifest check.** Optional: run `kubeconform -strict deploy/portal.yaml` if you have it. It was not run during development.
- [ ] Deploy: `kubectl apply -f deploy/portal.yaml`, then `kubectl -n kagent-portal port-forward svc/portal 8080:80`, then open `http://localhost:8080`.
- [ ] Watch logs in a second terminal: `kubectl -n kagent-portal logs -f deploy/portal`. The logs never contain passwords or token values. Each site's token appears only as its id (`token-xxxxx`).

To change config: edit the ConfigMap, then `kubectl -n kagent-portal rollout restart deploy/portal`.

**Tokens left behind.** Only the Log out button revokes Rancher tokens. A pod restart, session expiry (idle or absolute), or logging in again without logging out drops the portal session, but those Rancher tokens stay valid until their `ttl` (up to `absolute_timeout`, 8h). Log out before each restart where you can.

## Tests

| ID | Do this | Expected | Record |
| --- | --- | --- | --- |
| T1 | Start the pod | Log line `portal listening` lists the 3 sites with the right URLs. Pod Ready. | Startup log line (redact nothing; it has no secrets) |
| T2 | Log in with a valid admin account | If you see "Unexpected response from Rancher", stop and copy the `event=login` log line: the login response shape differs from what the portal expects. Otherwise the home page shows 3 rows `ok`, HTTP 200, a token id, `expiresAt`, Rancher user id, principal IDs. No warnings. | `expiresAt` minus login time for each site. It should be about 8h, which shows Rancher honours `ttl`; with the TTL settings unset, check whether it's capped. Is the `activedirectory_user://…` value identical on all 3 rows? Latency per site. |
| T3 | Log out | Logged-out page: 3 rows, Logged out = yes, Revoked = yes. "Checks" shows how many tries it took to see the token rejected; 1 is normal, and more than 1 means Rancher's cache lagged the delete. | Any row with no, plus its error text. The Checks value per site. Optional: `kubectl get tokens.management.cattle.io <token-id>` on that site's local cluster → NotFound. |
| T4 | **Once:** log in with the right username and a wrong password | "Login failed". Log line `event=login` shows site 1 `rejected` and sites 2 and 3 `skipped`. If Rancher returned anything other than 401, a `login_http_error` line shows Rancher's error body. | The `http_code` Rancher returned for a bad password. **Important:** the portal stops only on 4xx/500; if this is 502/503/504 the lockout logic is wrong. Confirm site 2/3 saw no attempt (Rancher logs or audit, if available). |
| T5 | Log in as a user that exists in AD but has no Rancher access on some site (if such a user exists) | Status for that site, plus a drift warning if it rejects | Status and `http_code` per site, and the warning text |
| T6 | Set `idle_timeout: 2m`, restart, log in, wait 3 minutes, reload `/home` | Redirected to the login page | Pass/fail. Then set it back to 30m. |
| T7 | Set site 1's `url` to an unreachable address (e.g. `https://10.255.255.1`), restart, then log in with a valid account | Site 1 `unavailable` after about `login.timeout` (15s). Sites 2 and 3 `ok`. Login succeeds. | Time to log in, plus site 1's error text. Restore the URL afterwards. |
| T8 | Point one site's `ca_file` at another site's CA, restart, then log in | That site `unavailable` with a certificate error. Others `ok`. | The error text. Restore afterwards. |
| T9 | Optional: log in twice in two browsers (or one private window) | Two independent sessions. Logging out of one leaves the other working. | Pass/fail |

## Redaction rules

- Never write down a password or token value. Token **ids** (`token-xxxxx`) are fine.
- In principal IDs, replace personal names with `<name>` but keep everything else exactly: spacing, case, commas, parentheses. Shape (placeholders here; in your report keep the real OU and DC values): `activedirectory_user://CN=<name> (Admin), OU=<ou>,DC=<dc>,DC=<dc>`.
- Copy log lines as JSON. They are already token-free. Redact names as above.

## Report template

```markdown
# Plan 1 live test report

Date:
Image digest:
Portal cluster:
Rancher version per site: site-1 = , site-2 = , site-3 =

| ID | Result (pass/fail) | HTTP codes | Timings | Notes |
| --- | --- | --- | --- | --- |
| T1 | | | | |
| T2 | | | expiresAt-login: s1= s2= s3= | principal identical across sites? y/n |
| T3 | | | | |
| T4 | | bad-password code: | | sites 2/3 untouched? y/n |
| T5 | | | | |
| T6 | | | | |
| T7 | | | login took: | |
| T8 | | | | error text: |
| T9 | | | | |

## Principal IDs as shown (redacted)
- site-1:
- site-2:
- site-3:

## WARN / ERROR log lines (redacted)

## Anything unexpected
```
