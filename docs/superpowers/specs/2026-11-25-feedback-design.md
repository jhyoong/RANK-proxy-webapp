# Feedback collection

Date: 2026-11-25. Builds on v0.5.1.

## 1. Intent

Collect feedback from signed-in portal users: a "Rate your experience" score and free-text comments, tied to the user who sent them. Users reach it from a link at the bottom of every inner page, from the user menu (next to Sign out), and from a small "How was this session?" prompt in the chat after a reply finishes.

## 2. Goals

- A `/feedback` page with a 1–5 rating and an optional comment.
- A "Give feedback" link in a footer on every inner page, and a "Give feedback" item in the user menu.
- An inline "How was this session?" strip in the chat, shown after a reply completes.
- Every submission is written as one structured log event, `event=feedback`, carrying the user's details.

## 3. Non-goals

- A database, a file, a webhook or email delivery. Logs only (user decision): it adds no infrastructure, no secrets and no NetworkPolicy changes, and matches how the portal already reports diagnostics.
- A page for viewing or exporting feedback. People read it from the log stack.
- Feedback from signed-out users. The login and signed-out pages (the `bare` layout) get no link.
- New config keys. Limits are constants.

## 4. Anti-goals

- A modal or popup that interrupts someone mid-task. The chat prompt is inline, appears only after a finished reply, and is easy to dismiss.
- Loosening the CSP or putting user text into the DOM as HTML.
- A feedback endpoint anyone can use to flood the logs.

## 5. Approaches considered

- **A. A separate `/feedback` page (chosen).** It's server-rendered like every other page, uses a plain HTML form with the CSRF field, and needs no JavaScript. The downside is that it leaves the current page; the links open in a new tab, so a running chat reply isn't interrupted.
- **B. A popup dialog on every page.** You never leave the page, but every page would need JavaScript (today only the cluster page loads any). That's more code and more testing for little gain.
- Rejected earlier: webhook and email delivery (they need Secrets, new egress rules, and either lose feedback when the endpoint is down or need retries) and a chat popup (too intrusive for a troubleshooting tool).

## 6. Design

### Routes

- `GET /feedback?from=<path>`: shows the form. Requires a session; without one it redirects to login like other inner pages. `from` is kept only if it is a local path (starts with `/`, not `//`, at most 512 bytes). Otherwise it is dropped.
- `POST /feedback`: one handler for both the page form and the chat strip.
  - Fields: `rating` (1–5, optional), `comment` (optional, at most 2000 characters after trimming), `source` (`footer` | `menu` | `chat`; anything else becomes `footer`), `from`, `conversation` (chat only; must be a UUID, otherwise dropped).
  - At least one of rating or comment is required.
  - CSRF: the `csrf` form field (page) or the `X-CSRF-Token` header (chat strip), checked against the session in constant time, the same way chat does.
  - Page form: on success, render a "Thanks for your feedback" page with a "Back to clusters" link. On a validation error, re-render the form with the message and the user's input kept.
  - Chat strip: returns `204` on success, `400` / `403` / `429` otherwise, with no body that the UI depends on.
- Rate limit: 10 submissions per user per hour, keyed by the session's cap key (principal, else normalised username), using the existing `ratelimit.Limiter`. Over the limit → `429` with the message "You've sent a lot of feedback recently. Please try again later."

### Log event

`s.log.Info("feedback received", "event", "feedback", ...)` with:
`feedback_id` (random UUID per submission, so an external collector can deduplicate; see `docs/feedback-collection-options.md`), `username`, `principal`, `rating` (0 when not given), `comment`, `source`, `page` (the `from` path), `site` and `cluster` (parsed from `page` when it is `/clusters/{site}/{id}`), `conversation`, `version`.
Comment text is a slog attribute value, so the handler quotes and escapes it. This is the one log event that contains user-written text; the README must say so.

### UI

- **Footer** (in the `shell` layout): a muted, centred "Give feedback" link to `/feedback?from=<current path and query>`, opening in a new tab (`target="_blank" rel="noopener"`). `pageData` gains the current request path so the template can build the link.
- **User menu**: a "Give feedback" link above Sign out, same URL with `source=menu`, also opening in a new tab.
- **Feedback page**: a card titled "Rate your experience" with five radio buttons styled as a 1–5 scale (labels "1 – Poor" to "5 – Excellent"), an "Anything else you'd like to tell us?" textarea (`maxlength` 2000), and Submit. The thanks page uses the same card.
- **Chat strip** (`chat.js`): after a `done` event with no error, append below the reply a strip reading "How was this session?" with buttons 1–5 and a × dismiss button.
  - Picking a rating reveals an optional comment box plus Send and Skip. Send posts the rating and comment; Skip posts the rating alone. Either way the strip is replaced by "Thanks for your feedback".
  - It appears at most once per conversation: the context ID is recorded in `sessionStorage` when the strip is shown, so it doesn't come back on later replies or after a reload. Dismissing posts nothing.
  - It never appears while streaming, after an error, or when replaying a transcript.
  - On a failed post, show "Couldn't send feedback" in the strip and leave it in place.
  - All text goes in with `textContent`; styles live in `style.css`.

### Tests and docs

- `server_test.go`: GET without a session redirects; POST with and without CSRF (field and header); rating bounds; empty submission rejected; comment length cap; `from` validation (`//evil`, absolute URLs); rate limit → 429; the log event's fields (including site/cluster parsed from `page`); the chat-style request gets 204.
- README: a short "Feedback" section that names the log event and its fields, and notes that `comment` holds free user text.
- Version: v0.6.0.

## 7. Left to the builder

Exact CSS for the footer, scale and strip, within the existing palette. How `pageData` carries the current path. Whether to share the rating-scale markup between the page and the strip (the strip is built in JS, so probably not).
