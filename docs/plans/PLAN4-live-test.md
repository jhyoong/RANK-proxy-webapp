# Plan 4 Live Test: Security Hardening (v0.4.0)

Run this inside the target network with the `v0.4.0` image, then type up the short [report](#report-format). It is designed to be typed by hand, because files can't leave the network. It checks the login rate limits, the response headers and cookie, the cross-origin block, token revocation when a session ends (expiry, eviction, restart), and the NetworkPolicy. Nothing here changes Rancher settings, RBAC or kagent.

**Use test accounts, not real admins, for S2, S3, S5, S6 and S7.** Wrong passwords count toward the AD lockout threshold.

## Before you start

- [ ] **Image.** Move the image into the internal registry. Deploy `v0.4.0` by digest. Record the digest.
- [ ] **Config.** Keep the v0.3.x ConfigMap. Add the new keys (defaults shown; only `trusted_proxies` has no useful default):
  ```yaml
  login:
    max_attempts_per_user: 3
    max_attempts_per_ip: 20
    attempt_window: 15m        # every counted attempt restarts the window
    trusted_proxies: ["<ingress-controller-pod-cidr>"]
  session:
    max_per_user: 5
  ```
  `cookie_secure` must be true (or unset, with TLS or an ingress in front) so S1 sees HSTS and `Secure`.
- [ ] **AD lockout.** Confirm your AD lockout threshold is at least 5 and note its observation window. The default `max_attempts_per_user: 3` per 15m assumes this. Wrong passwords typed straight into Rancher's UI also count toward AD lockout.
- [ ] **Four test accounts**: `<testuser>` (S2 only, ends up blocked), `<testuser2>` (S5, S6, S7: needs successful logins), `<testuser3>` and `<testuser4>` (S3, each ends up blocked). Also a second machine or network for S3, and `<curl-image>`: any image with `curl` that your cluster can pull (for S8).
- [ ] **Deploy** with `kubectl apply -f deploy/portal.yaml`. Fill in the NetworkPolicy placeholders (ingress namespace, Rancher site CIDRs) first. To keep S1 to S7 clean, you can apply the NetworkPolicy only at S8.
- [ ] **Logs** in a second terminal: `kubectl -n kagent-portal logs -f deploy/portal`. New events: `login_rate_limited`, `cross_origin_blocked`, `session_ended`. The logs never contain passwords, bearer tokens or message text.
- [ ] **Rancher audit log or a way to count login attempts** per site (the Rancher audit log, or the AD log). Needed for S2.

To change config: edit the ConfigMap, then `kubectl -n kagent-portal rollout restart deploy/portal`. A restart ends every session (S7).

## Tests

| ID | Do this | Expected | Record |
| --- | --- | --- | --- |
| S1 | `curl -sI https://<portal>/`. Then log in in a browser and open DevTools, Application (or Storage), Cookies | Headers: `Content-Security-Policy`, `Strict-Transport-Security: max-age=31536000`, `X-Frame-Options: DENY`, `Cross-Origin-Opener-Policy: same-origin`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`. Cookie `portal_session`: `HttpOnly`, `Secure`, `SameSite=Strict` | Pass/fail, and any missing header |
| S2 | As `<testuser>`: wrong password 3 times (each shows `Login failed`), then a 4th time (wrong or right) | The 4th shows `Too many login attempts; try again later` (URL `/?e=limited`). Rancher or AD audit shows 3 site-1 attempts, not 4 (a blocked login makes no Rancher call). Portal log: `event=login_rate_limited scope=user username=<testuser>`. Then try `CORP\<testuser>` (your domain prefix) and `<testuser>@<upn-suffix>`: expect `Too many login attempts` immediately, and no new site-1 attempt | Pass/fail, attempts seen at site 1 |
| S3 | **(a) Real IP in the log.** From a second machine, as `<testuser3>`, enter a wrong password 4 times in a browser (the 4th is blocked). **(b) Forged header.** From a machine outside the ingress, run `for i in 1 2 3 4; do curl -s -o /dev/null -w '%{redirect_url}\n' -X POST -H 'X-Forwarded-For: 203.0.113.9' https://<portal>/login -d username=<testuser4> -d password=bad; done` (`<testuser4>` is fresh, so it is not blocked beforehand; the 4th line ends in `?e=limited`) | The pass condition is the `ip=` field in the two `event=login_rate_limited scope=user` log lines. (a) `ip=` is the second machine's real IP. (b) `ip=` is the curl machine's real IP. It is **not** the ingress pod IP (that would mean `trusted_proxies` is wrong and every user shares one bucket) and **not** `203.0.113.9` (that would mean a forged header is trusted) | Pass/fail, both `ip=` values |
| S4 | `curl -si -X POST -H 'Sec-Fetch-Site: cross-site' https://<portal>/login -d username=x -d password=y`. Then a normal browser login through the ingress | `403` with body `cross-origin request blocked`. Log: `WARN event=cross_origin_blocked method=POST path=/login ip=<ip>`. The browser login works (so `Origin` and `Host` pass through the ingress) | Pass/fail, browser login ok y/n |
| S5 | Set `session.idle_timeout: 2m` in a test ConfigMap and restart. Log in as `<testuser2>`, open `/diagnostics` and copy a token ID **before** the wait starts (loading `/diagnostics` resets the idle timer), then don't touch the session again. Wait about 3 minutes (idle timeout plus up to 1 minute for the sweep, which runs every minute) | Log: `event=session_ended reason=expired username=<testuser2>`, then `event=logout` with `"Revoked":true` for each site. The token is gone in Rancher: `kubectl get tokens.ext.cattle.io <id>` (or `kubectl get tokens.management.cattle.io`, whichever your Rancher uses) is NotFound, or it is missing under the Rancher UI, API Keys. Reloading `/home` goes back to login | Pass/fail, minutes waited |
| S6 | Log in 6 times as `<testuser2>` (6 private windows; a successful login resets that user's count, so it is not blocked). Then reload `/home` in the first window | The first window is sent back to login. Log: `event=session_ended reason=evicted` followed by `event=logout`. Allow up to 1 minute for the log line (see note). The other 5 windows still work | Pass/fail, seconds until the log line |
| S7 | Log in as `<testuser2>`, then `kubectl -n kagent-portal rollout restart deploy/portal` | The old pod's log (`kubectl logs <old-pod>`) has `event=session_ended reason=shutdown`, then `event=logout` with `"Revoked":true`. The token is gone in Rancher | Pass/fail |
| S8 | Apply the NetworkPolicy. In the portal namespace run `kubectl -n kagent-portal run dbg --image=<curl-image> --labels=app=portal --rm -it --restart=Never -- sh`, then `curl -m 5 https://example.com` and `curl -m 5 https://<site-1-rancher>` | `example.com` times out. The Rancher site answers. After this, the portal still logs in, lists clusters and chats, and the portal pod stays Ready after the policy is applied (kubelet probes) | Pass/fail per piece |
| S9 | Chat to a reply that takes longer than 10s (the body read deadline). Also a `<site>-stg-20`-style silent wait if your agent has one | The reply completes. The chat is not cut at 10s | Pass/fail, total s |
| S10 | Repeat Plan 3 C1, C2 and C4 quickly | Same results as before | Pass/fail each |

**S1 note.** `HSTS` and the `Secure` cookie flag only show when `cookie_secure` is on. A local fakerancher run sets `cookie_secure: false`, so they are absent there; that is expected and only applies to the local dry run.

**S2 note.** The limit counts the attempt before calling Rancher. A login that fails because every site is unavailable gives the attempt back, unless a site timed out or the browser disconnected mid-login (AD may already have seen the bind). A successful login resets the user count and gives back its IP attempt, so successes do not count toward the IP limit (20 per 15m per client IP, or per /64 for IPv6, shared by all users behind the same IP). Only failed logins add up; if S2 and S3 run from one machine, you may still reach the IP limit; wait out `attempt_window` and note it.

**S6 note.** Evicted (and relogin) sessions are revoked by the next sweep, so the `session_ended` line can lag the 6th login by up to a minute. The first window is rejected immediately.

**S8 note.** The NetworkPolicy allows DNS only to `kube-dns` pods. If the cluster uses NodeLocal DNSCache (`169.254.20.10`), that address is not covered by the kube-dns rule; add an egress rule for it or logins fail on name lookup.

## Report format

Type the report by hand in this short format. The values below are an example only.

```
v0.4.0 report  2026-12-01
S1 pass
S2 pass  site1 attempts=3
S3 pass  ip=10.1.2.3,10.1.2.9
S4 pass  browser login=y
S5 pass  waited=3m
S6 pass  log lag=40s
S7 pass
S8 pass
S9 pass  total=41s
S10 C1 pass  C2 pass  C4 pass
warn/error: login_rate_limited user (expected), cross_origin_blocked (expected)
other: none
```

| Field | What to write |
| --- | --- |
| S1 to S10 | `pass`, `fail` or `skip`, plus the extra fields shown |
| S1 on fail | The missing header, or the cookie flag that is absent |
| S2 `site1 attempts` | Login attempts the Rancher or AD log shows for the test user at site 1 (expect 3) |
| S3 `ip` | The `ip=` values in the two `login_rate_limited` lines, (a) then (b). Ingress pod IP means `trusted_proxies` is wrong; `203.0.113.9` means a forged header was trusted |
| S4 `browser login` | `y` if a normal browser login works through the ingress, `n` if it is blocked |
| S5 `waited` | Minutes until `session_ended reason=expired` appeared |
| S6 `log lag` | Seconds from the 6th login until `reason=evicted` appeared |
| `warn/error` | One entry per `WARN` or `ERROR` log line: its `event`. Mark the ones you caused as `(expected)` |

**If S3 fails**, write the `ip=` values from the log and your ingress pod CIDR.

**Names** may be internal. Replace any sensitive name with `xx`.

## Before the live test: open items

1. **AD lockout threshold and window.** See "Before you start".
2. **Ingress controller pod CIDR** (for `login.trusted_proxies`) and its namespace (for the NetworkPolicy ingress rule).
3. **The Rancher sites' IPs or CIDRs** for the NetworkPolicy egress rules. If they sit behind a load balancer with changing IPs, a CIDR-based policy may not fit, and egress may need to stay open on 443.
