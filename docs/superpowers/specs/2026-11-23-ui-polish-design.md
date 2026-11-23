# PLAN5: End-user UI, login-page config, and resumable conversations

Date: 2026-11-23. Builds on v0.4.0 (PLAN4). Source request: `docs/plans/PLAN5.md`.

## 1. Intent

The portal began as a test harness, so its pages show debugging detail: SSAR and HTTP tables, per-site token tables, a "chat test" tag, and timing lines under each reply. It is now moving to a wider group of end users. They need only four things:

1. Sign in.
2. See the clusters they have access to.
3. Open a cluster.
4. Chat with kagent there, either starting a new conversation or resuming an earlier one from any browser.

Everything should look clean, polished and professional. Every debugging detail goes; the logs keep it.

## 2. Goals

- A consistent visual design across every page, using the "split screen" login look the user chose: a navy panel (`#0f172a`) with a white work area, slate greys, and a navy top bar on the inner pages.
- Three new config items, shown on the login page: an announcement banner, a help/description box, and a support email address.
- A friendly "kagent isn't available on this cluster" screen that points users to the support email.
- A conversation sidebar on the cluster page. It lists past conversations with the configured agent on that cluster, read from kagent's own database, and lets the user resume one or start a new one. Conversations are shared by everyone who can use kagent on that cluster (see 7.4).
- Rendered Markdown in agent replies, and tool calls shown as collapsible lines with their arguments and output, both live and on replay.
- Support for any number of sites (one or more), driven entirely by `sites:` in the config. This already works today; the UI must not assume three.

## 3. Non-goals

- Filtering the cluster list by kagent availability. Rancher already limits the list to clusters the user can see; unusable clusters stay listed and show the unavailable screen when opened.
- Per-user conversation history. kagent is a debugging tool for now, so shared history is acceptable (user decision). See 7.4 for the later upgrade path.
- Renaming, deleting or searching conversations.
- Human-approval (HITL) flows. A turn ending in `input-required` is shown as "the agent asked for approval", as it is today.
- Showing sub-agent child sessions or model thinking.
- A dark theme, user preferences, or internationalisation.
- Reloading the config without a restart. Adding or removing a site is still a ConfigMap edit plus a pod restart.

## 4. Anti-goals (what would make this a failure even if it works)

- Agent or tool text reaching the DOM as HTML. Today agent text only ever goes through `textContent`, and that must stay true.
- Loosening the CSP (`default-src 'none'; script-src 'self'; style-src 'self'; ...`). That means no inline styles or scripts, no CDNs and no web fonts from elsewhere.
- The browser calling kagent REST endpoints directly. All kagent reads go through portal handlers that build the upstream URL themselves.
- Debugging jargon on screen: SSAR, HTTP codes, token IDs, latencies.

## 5. Constraints

- Go server-rendered templates (`internal/web/templates`), with static assets embedded via `embed.FS` (`internal/web/static`). No build step, no Node.
- CSP as above. Inline SVG icons inside templates are fine (they are markup, not style or script). Avoid `style=""` attributes; use classes.
- kagent v0.9.6 REST API through the Rancher service proxy, as documented in `docs/kagent-chat-via-rancher.md`:
  - `GET .../api/sessions` is verified live.
  - `GET .../api/sessions/agent/<ns>/<name>` is verified from source (v0.9.6 `server.go:241`).
  - `GET .../api/sessions/<id>/tasks` is verified live.
  - No `user_id` is sent anywhere, so kagent files and reads everything under its default user `admin@kagent.dev` (`authn.go:25-31`).
  - Responses are wrapped as `{error, data, message}`.
- kagent must run in its default `unsecure` auth mode (`go/core/pkg/app/app.go:215`). In `trusted-proxy` mode it expects a JWT, and calls through Rancher would fail. Record this as a deployment requirement in the README.
- RBAC: the existing `services/proxy` grant on the kagent controller covers GET (`get`) for the REST reads as well as POST for A2A. The builder should confirm that the access check's SSAR still reflects what's needed.
- Existing tests in `internal/web/server_test.go` check page text (about 43 `strings.Contains` checks). Update them to the new markup and wording; don't delete them.

## 6. Approaches considered

1. **Keep the current stack (chosen).** Go templates, one hand-written stylesheet built on CSS custom properties (colour, spacing and radius tokens), and plain JavaScript. The server reads kagent history and returns it to the browser as normalised JSON. The same JavaScript renderer draws live and replayed turns. Agent Markdown is rendered by a small hand-written renderer that builds DOM nodes.
2. **Same, plus a vendored CSS framework file** (for example Pico) in `/static/`. Rejected: overriding its defaults to get the chosen navy look costs about as much as writing the CSS, and it adds a file to keep patched.
3. **A single-page app (React or similar).** Rejected: it adds Node tooling and a build step to a Go-only repo and CI, and the pages don't need it.

For Markdown, the alternative was a vendored Markdown library plus an HTML sanitizer, which gives full Markdown coverage. It was rejected to keep the "no HTML from agent text" rule and to avoid adding dependencies.

## 7. Design

### 7.1 Config additions

A new top-level `ui:` block. All keys are optional; empty means the element isn't shown.

```yaml
ui:
  # Announcement shown at the top of the login page. Plain text, one line.
  banner: ""
  # Banner style: info (blue) or warning (amber). Default info.
  banner_level: info
  # Help/description box on the login page. Plain text; blank lines separate
  # paragraphs; http(s) URLs and email addresses become links.
  login_help: ""
  # Support contact shown on the login page, the unavailable screen, error
  # messages, and the user menu. Rendered as a mailto: link.
  support_email: ""
```

- Validation: `banner_level` must be `info` or `warning`. `support_email` must parse with `net/mail.ParseAddress` and be a bare address. Lengths are capped (banner 300 chars, help 2000) so a typo can't wreck the layout.
- All three are rendered with Go's `html/template` escaping. Links in `login_help` are made on the server by splitting the text into text and link tokens, never by inserting raw HTML.
- Document the block in `config.example.yaml` and in `deploy/portal.yaml`'s ConfigMap.
- If `support_email` is empty, text that would name it falls back to "contact your platform team" with no link.

### 7.2 Visual system

- `style.css` is rewritten around tokens: `--navy #0f172a`, the slate scale (`#f8fafc`, `#f1f5f9`, `#e2e8f0`, `#cbd5e1`, `#94a3b8`, `#64748b`, `#475569`, `#334155`), accent blue `#2563eb` for links and focus rings, green `#15803d` for active, amber for warning, and red `#b91c1c` for errors. A 4px spacing scale, 6–10px radii, and the `system-ui` font stack.
- Visible focus states, contrast meeting WCAG AA, and `prefers-reduced-motion` respected.
- Responsive: the login split stacks below about 720px; the conversation sidebar becomes a toggle drawer below about 900px.
- Inner-page shell (`layout.html`):
  - A navy top bar with "kagent portal" and a breadcrumb (Clusters / <cluster name>).
  - On the right, a user menu: the username, the support email link, and "Sign out", which posts to `/logout` with the CSRF token. A small version string sits in the menu.
  - The login and signed-out pages use their own shell without the top bar.
- Icons are inline SVG in templates.

### 7.3 Pages

**Login (`/`)**
- The split screen layout.
- Left navy panel: the product name, a one-line tagline, `ui.login_help`, and at the bottom "Need help? <support email>".
- Right side: `ui.banner` across the top if set, then the "Sign in" form (username, password, button) and any error message.
- The login error wording stays as today, including rate-limit messages, but is styled.

**Cluster list (`/home`)**
- A heading "Clusters" with a count, plus "Updated N min ago · Refresh". Refresh is the existing POST with CSRF.
- A search box (the existing `?q=` filter) and site filter chips: "All sites" plus one per site in the session, from config order. Chips use a `?site=` query parameter, which is new; the filter happens on the server and is combined with `q`.
- The table shows only Name, Site and Status. Status is a green dot plus "Active", or greyed "Disconnected", which isn't clickable. The cluster ID column is removed.
- Site problems from login appear as one plain banner per site:
  - A site that couldn't be reached: "Site 2 couldn't be reached when you signed in. Its clusters aren't shown. Sign in again to retry."
  - A site that rejected the credentials others accepted: "Site 2 didn't accept your sign-in. Its clusters aren't shown."
  - A site whose cluster list errored: "Couldn't load clusters from Site 2." with a Refresh hint.
- The principal mismatch and no-principal warnings are no longer shown on this page; they stay in the logs.
- Empty states: "No clusters match "<q>"" and "No clusters available".

**Cluster page (`/clusters/{site}/{id}`)**
- `ready`: the chat layout in 7.4.
- `expired`: end the portal session the same way `/logout` does (revoke every remaining site token, delete the session, clear the cookie). Then redirect to `/?expired=1`, and the login page shows "Your session has expired. Please sign in again." *(Builder's call; see 7.4 "Expiry".)*
- Every other outcome (`no-access`, `not-installed`, `not-running`, `unreachable`, `denied`, `bad-card`, `unexpected`, `check-failed`, and `disconnected` if reached directly):
  - A centred card: "kagent isn't available on this cluster", one line explaining that it may not be installed, may not be running, or your account may not have access, then "Contact the platform team at <support email>".
  - A small grey "Reference: <outcome> · <cluster name>" line, the only diagnostic detail kept.
  - "Back to clusters".
- The diagnostics and agent-card tables are removed.

**Signed out (`/logout` result)**
- A simple centred card: "You've signed out" and "Sign in again".
- If any site's token revoke failed, add one neutral line: "Some sessions couldn't be closed and will expire on their own." No table.

**Removed**
- `GET /diagnostics` and `diagnostics.html`. The route now returns 404.
- The "chat test" tag and the chat timing (`diag`) line.

### 7.4 Chat and conversation history

**Shared history (user decision)**
- The portal sends no `X-User-Id` header and no `user_id` parameter, as today. Every conversation on a cluster is stored under kagent's default user (`admin@kagent.dev`), and the sidebar lists all of them for anyone who can use kagent on that cluster.
- *(Builder's call)* Under the sidebar heading, a small muted line says: "Conversations on this cluster are shared with everyone who can use kagent here."
- Conversations from the live tests already sit under the default user, so they will appear.
- **Upgrade path, if per-user history is wanted later** (verified against kagent v0.9.6 source):
  - Send an opaque ASCII ID, such as a hash of the AD principal, as `X-User-Id` on chat and as `user_id` on reads.
  - Don't send the raw DN: kagent's Python runtime puts `user_id` into URLs without escaping it (`kagent-adk/_session_service.py:81,136,156,172`), and non-ASCII values likely fail.
  - Also limit `context_id` to IDs the user owns, because kagent's `/tasks` lists by session ID only (`tasks.sql:11-14`).

**Layout**
- The left sidebar (about 240px) has a "+ New conversation" button, then all conversations with the configured agent on this cluster, newest first, grouped as Today / Yesterday / Last 7 days / Older.
- Each entry shows a title and a relative time; the open conversation is highlighted.
- The right side is the chat: messages, then a composer with a textarea (Enter sends, Shift+Enter adds a newline), a Send button that turns into Stop while a reply runs, and a character counter near the limit.
- An empty new conversation shows a short prompt ("Ask kagent about <cluster name>").

**URL and state**
- `/clusters/{site}/{id}` starts a new conversation with a fresh contextId, as today.
- `/clusters/{site}/{id}?c=<uuid>` opens that conversation. The portal validates the UUID and loads its history; new messages continue it by sending the same `contextId`.
- After the first message of a new conversation, the browser updates the URL with `history.replaceState` to `?c=<contextId>`, so a reload resumes it. It also adds the conversation to the top of the sidebar on the client.
- Clicking a sidebar entry or "New conversation" is a normal link; full page navigation is fine.
- Leaving while a reply is running needs no confirmation, because kagent finishes the reply anyway. See "Resuming a turn that is still running".

**New server endpoints** (GET; session required; same cluster lookup and `kagent ready` assumptions as chat)
- `GET /clusters/{site}/{id}/conversations` returns `[{id, title, updated_at}]`.
  - The source is `.../api/sessions/agent/<agent_ns>/<agent_name>`. It exists in v0.9.6 (`server.go:241`). It hides sub-agent child sessions, has no paging, is ordered by `updated_at` descending, and returns 404 if the agent isn't in kagent's database. Treat that 404 as an empty list.
  - Session fields are `id`, `name` (omitted if null), `user_id`, `created_at`, `updated_at`, `agent_id` (in the form `<ns>__NS__<name>`, not `ns/name`) and `source`.
  - The title is `name`, which kagent fills on A2A with the first 20 characters of the first message plus "…". If `name` is missing, use "Conversation · <date>". Showing a longer title would cost one tasks call per entry, so it's out of scope.
  - The list is capped at the 50 most recent.
- `GET /clusters/{site}/{id}/conversations/{cid}` returns the normalised transcript: `{turns: [{user: text, items: [...], state}]}`. Each item is one of `{kind:"text", text}`, `{kind:"tool", id, name, args, result, is_error}`, or `{kind:"notice", text}`.
  - It is built from `.../api/sessions/<cid>/tasks` using the rules in `docs/kagent-chat-via-rancher.md` §4: dedupe by `messageId`; render every part in order; pair `function_response` to `function_call` by `data.id`; use artifacts only as a fallback; read both `kagent_` and `adk_` metadata prefixes.
  - The response also carries `running: true` when the last task is still `submitted` or `working`. Earlier turns stuck in `working` (which can't still be running, because one turn runs at a time) get a "This reply didn't finish" notice.
  - A 404 from kagent (an unknown conversation) returns JSON 404. *(Builder's call)* `chat.js` then shows a "Conversation not found" message in the chat area with a "Start a new conversation" link, and disables the composer. The cluster page handler doesn't pre-check the conversation, which avoids an extra Rancher round trip.
  - *(Builder's call)* Agent match: before returning the transcript, check that the conversation belongs to the configured agent, the ID must appear in the agent-filtered list for that cluster. A conversation with a different agent gets the same 404. The chat endpoint keeps accepting any valid UUID, as today.

**Resuming a turn that is still running** *(builder's call)*
- kagent keeps working after the browser goes away, and Rancher cuts silent streams, so a turn left `working` is usually still running rather than failed. Sending a new message then would run two turns at once and interleave their stored events (doc §3).
- So when the transcript says `running: true`, `chat.js` shows the turn so far with a "kagent is still working on this reply…" indicator and keeps the composer disabled. It re-fetches the transcript every 5 seconds until `running` is false, then renders the final turn and enables the composer.
- Polling stops after `chat.max_duration`, measured from the task's timestamp. After that, show "This reply didn't finish" and enable the composer.
- The Stop button closes the browser's stream, as today. Its label stays "Stop", but afterwards the UI shows "Stopped watching. kagent may still finish this reply; reopen the conversation to see it." The portal doesn't send a cancel to kagent (out of scope).
- Leaving the page while a reply runs needs no confirmation dialog, because the reply isn't lost.

**Expiry** *(builder's call)*
- Any Rancher 401 for the user's token, from the cluster page check, a chat, or the conversation endpoints, ends the whole portal session as `/logout` does: revoke the other sites' tokens and delete the session.
- Pages redirect to `/?expired=1`. JSON and SSE endpoints return 401, and `chat.js` shows "Your session has expired" with a "Sign in again" link to `/`.
- Tokens from one login share one TTL, so in practice the sites expire together anyway.

**History permission** *(builder's call)*
- No extra permission check. If a history read returns 403, the sidebar shows "Conversation history isn't available for your account" and chat stays usable. If it returns a 5xx or times out, the sidebar shows "Couldn't load conversation history" with a retry link (reload).
- Should the sidebar list be fetched by the browser or rendered on the server? Render it on the server into the cluster page, so it appears without a flash. Load the transcript from JSON by `chat.js` when `?c=` is present, so live and replayed turns share one renderer. If the history call fails, the chat still loads, with an inline "Couldn't load conversation history" notice.
- Tool `args` and `result` are capped on the server: each result's text is truncated to 64 KB with a "(truncated)" marker, and args to 8 KB. Never pass tokens or headers through.

**Live stream changes (`internal/chat`, `internal/web/chat.go`)**
- The `tool` SSE event gains `id` and `args`.
- A new `tool_result` event carries `{id, name, result, is_error}` from `function_response` parts, with the same caps.
- The `done` event keeps its fields for logging, but the UI no longer shows them.
- The other events (`progress`, `answer`, `error`) are unchanged.

**Rendering (`chat.js`)**
- One renderer handles both live events and replayed transcripts.
- User messages are right-aligned navy bubbles.
- Agent text is a left-aligned light bubble rendered as Markdown (below).
- Each tool call is one collapsed line: a wrench icon, the tool name, and a short summary of the args. Expanding it shows the args as pretty JSON and the result as preformatted text, with "error" styling if `is_error`.
- Live progress text, the in-between steps, renders as normal agent text. The existing partial-replace and answer-dedupe logic stays.
- Error lines use the friendly wording from the existing `httpError` map, plus a support email mention where useful. The 401 case links to sign in.

**Markdown subset (hand-written, DOM-only)**
- Supported: paragraphs, line breaks, headings (`#`–`###`), bold and italic, inline code, fenced code blocks (with a copy button), ordered and unordered lists (one nesting level), links (only `http:`/`https:`, opening with `rel="noopener noreferrer" target="_blank"`), blockquotes, and simple GitHub-style pipe tables. Kubernetes output is often tabular.
- Every node is created with `createElement`/`textContent`; there is no `innerHTML` anywhere.
- Anything unrecognised renders as plain text.
- While streaming, re-render the current bubble from its full text on each chunk. That is acceptable at these sizes.

### 7.5 Fake Rancher

`cmd/fakerancher` and `internal/rancher/ranchertest` gain the kagent session endpoints for ready clusters:
- They record conversations per contextId as chats happen.
- They serve the list, the agent-filtered list and the tasks endpoints in the shapes from the doc, including one tool call and result per scripted reply.
- They seed two older conversations on each site's first ready cluster, so the sidebar and its date groups can be clicked through without chatting first.

### 7.6 Deploy and docs

- `deploy/portal.yaml`: add the `ui:` block to the ConfigMap. Add a comment by the NetworkPolicy egress list saying it needs one entry per configured site.
- `README.md`:
  - Update the Pages section (diagnostics removed; conversations added).
  - Add the `ui:` block to the config notes.
  - Note that adding or removing a site needs a config edit, a NetworkPolicy edit and a restart.

## 8. What the user decided

- Audience: end users, with all debugging detail removed.
- Login look: split screen (option B). Chat layout: conversation sidebar (option A). The cluster list and unavailable screen as mocked up.
- Unusable clusters stay in the list; opening one shows a "contact the platform team" message.
- New config: login banner, login help box, support email.
- Conversations are resumed from kagent's database on each cluster and must work from any browser.
- History is shared per cluster for now, and no user ID is sent to kagent. kagent is a debugging tool at this stage, so keep it simple.
- Sites stay configurable (one or more).
- Build approach: keep the current stack, with a hand-written stylesheet and JS and a hand-written Markdown renderer.

## 9. Left to the builder

- The exact copy for empty states and errors, within the tone above.
- Checking live which session-list endpoint works and what the session object contains (title source), and recording the findings in a PLAN5 live-test doc.
- The tool-call summary format (which args to show on the collapsed line).
- Breakpoints, exact spacing, and the icon set.
- Test structure. Required coverage:
  - config validation of `ui:`
  - each page's key text
  - the 404 on `/diagnostics`
  - conversations endpoints: the 404 mapping, the agent-match check, and 403/5xx sidebar states
  - the transcript normaliser against the doc's sample shapes
  - output caps
  - Markdown renderer cases, run in Go against fixtures or with a small headless check. The builder chooses which.
