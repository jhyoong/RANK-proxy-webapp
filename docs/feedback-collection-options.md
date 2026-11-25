# Collecting feedback from the portal logs

Date: 2026-11-25. Companion to `docs/superpowers/specs/2026-11-25-feedback-design.md`.

The portal writes each piece of feedback as one JSON log line on stdout (`slog.NewJSONHandler`) and stores nothing itself. This keeps the image simple: no database driver, no credentials, no extra egress. This note covers ways to move those lines into a durable store **outside** the portal. None of them change the portal image.

## What there is to collect

One line per submission, for example:

```json
{"time":"2026-11-25T09:14:03.2Z","level":"INFO","msg":"feedback received","event":"feedback",
 "feedback_id":"3f6c…","username":"jdoe","principal":"activedirectory_user://CN=…","rating":4,
 "comment":"Tool output was hard to read","source":"chat","page":"/clusters/site-1/c-abc",
 "site":"site-1","cluster":"c-abc","conversation":"9b1e…","version":"0.6.0"}
```

Match on `event == "feedback"`. Use `feedback_id` (a random UUID per submission) as the primary key, so a collector that reads the same line twice doesn't store it twice.

## Things that apply to every option

- **Pod logs are short-lived.** The kubelet rotates container logs (about 10 MiB per container by default), and they disappear when the pod is deleted. A collector must pick lines up before then. Anything that reads from a central log store instead is safe for as long as that store keeps logs.
- **Comments are free user text.** They may contain names, hostnames or pasted errors. Whatever store you pick needs access control and retention rules to match.
- **At-least-once delivery is fine** as long as writes are idempotent on `feedback_id`.

## Options

### 1. Use the log stack you already have (no new component)

If the cluster already ships logs to Loki, Elasticsearch/OpenSearch, Splunk or a cloud logging service, feedback is already collected there.

- Save a query (Loki: `{app="portal"} | json | event="feedback"`; Elasticsearch: `event:feedback`) and build a dashboard from it: average rating over time, recent comments.
- Optionally add an alert that fires on new feedback or on low ratings and posts to a team channel.
- **Caveat:** log retention is usually days to weeks. If you want feedback kept longer, combine this with option 2 or 3.

**Effort:** minutes. **Best when:** a log stack exists and reading feedback in it is good enough.

### 2. Route feedback lines in the log shipper

Most clusters run a node-level shipper (Fluent Bit, Vector, Fluentd, the OpenTelemetry Collector). Add a rule that matches portal lines with `event == "feedback"` and sends a copy to a second, long-lived destination:

- **Object storage** (S3 or MinIO): append-only, cheap, easy to query later with DuckDB or Athena. Every shipper supports it.
- **A dedicated index** in Elasticsearch/OpenSearch with long retention.
- **A database**: Fluent Bit has a PostgreSQL output; for others, send to an HTTP endpoint (option 4) that writes to the database.

**Effort:** a config change on a component the platform team owns. No new workload. **Best when:** you want long retention without running anything new. **Watch:** the shipper's config is shared infrastructure, so changes go through whoever owns it.

### 3. Scheduled job that queries the log store

A Kubernetes CronJob (for example every 15 minutes) that:

1. Reads its checkpoint (the last timestamp it processed) from the target store.
2. Queries the log store's API for `event="feedback"` lines since the checkpoint, minus a few minutes of overlap to cover late-arriving lines.
3. Upserts each line into the store on `feedback_id`, then moves the checkpoint forward.

Targets: PostgreSQL (one `feedback` table), SQLite on a PVC, a Google Sheet or SharePoint list (easy for non-engineers to read), or object storage.

This can be a small Python or Go script in its own image, with its own ServiceAccount, Secrets and NetworkPolicy. The portal's policies stay untouched.

**Effort:** a small script plus a CronJob manifest. **Best when:** a log store exists, the shipper config is hard to change, and you want feedback somewhere friendlier than a log UI.

### 4. Small collector service that the shipper pushes to

Like option 3, but push instead of poll. The log shipper forwards matching lines to an HTTP endpoint on a tiny service (`POST /ingest`), which validates them and upserts into the store. This is near real-time, and the same service could later serve a read-only feedback page. It's more to run than a CronJob: an always-on Deployment, a Service and authentication between the shipper and the service.

**Best when:** you want feedback visible within seconds, or plan to build a feedback viewer anyway.

### 5. Read pod logs directly (stopgap only)

A CronJob with RBAC `get` on `pods/log` runs the equivalent of `kubectl logs deploy/portal --since=20m | jq 'select(.event=="feedback")'` and upserts the matches. It needs no log stack. But it loses lines when the pod restarts or the logs rotate between runs, and it only reads the current pod. Use it to get started, not as the final setup.

## Recommendation

1. **Start with option 1** if a log stack exists: it costs nothing and shows whether people use the feature at all.
2. **When you need retention or a friendlier view, add option 2** (shipper → object storage or an index) **or option 3** (CronJob → PostgreSQL or a sheet). Choose between them by who owns the shipper config.
3. Choose option 4 only if near real-time matters. Use option 5 only where there is no log stack at all.

Whichever you choose, the portal stays the same: it writes one JSON line, and everything downstream is a separate deployable with its own credentials.
