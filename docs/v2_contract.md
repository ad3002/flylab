# FlyLab v2 contract: accounts, history, Claude planner, landing + app

This document is the single source of truth shared by the backend (`internal/**`, `cmd/**`,
`scripts/**`) and the frontend (`web/**`). Both sides implement exactly this; if something is
missing, extend this file instead of inventing private conventions.

## 1. Pages and static routes (served by Go)

| Route | File | Auth |
| --- | --- | --- |
| `GET /` | `web/templates/index.html` — public landing page | public |
| `GET /app`, `GET /app/{path...}` | `web/templates/app.html` — the application (hash routing inside: `#/new`, `#/history`, `#/job/<job_id>`, `#/account`) | public HTML; data needs auth |
| `GET /static/...` | `web/static/...` (css, js, img, fonts) | public |
| `GET /favicon.ico` | serves `web/static/img/favicon.svg` with `Content-Type: image/svg+xml` | public |

Images for the landing live in `web/static/img/`: `hero-far.png` (opaque background plate),
`hero-mid.png`, `hero-near.png` (transparent neuron layers for parallax), `fly-brain.png`
(whole-brain visualization), `spike-texture.png` (raster-like banner).

## 2. Accounts and sessions

Storage (SQLite, same DB, created/migrated idempotently at startup):

- `users(id INTEGER PRIMARY KEY, username TEXT UNIQUE NOT NULL, display_name TEXT NOT NULL,
  password_hash TEXT NOT NULL, created_at TIMESTAMP NOT NULL)`
- `sessions(token_hash TEXT PRIMARY KEY, user_id INTEGER NOT NULL, created_at TIMESTAMP NOT NULL,
  expires_at TIMESTAMP NOT NULL)` — only the SHA-256 of the token is stored.
- `jobs` gets new nullable columns `user_id INTEGER`, `prompt TEXT`, `title TEXT`
  (added with `ALTER TABLE` only when `PRAGMA table_info(jobs)` shows they are missing; a failed
  migration is a startup error, never a log line).

Rules:

- Username: 3–32 chars, `[a-z0-9_.-]`, stored lowercase. Password: 8–128 chars.
- Password hash: stdlib `crypto/pbkdf2` with SHA-256, 210 000 iterations, 16-byte random salt,
  stored as `pbkdf2_sha256$<iter>$<salt_b64>$<hash_b64>`; compare in constant time.
- Session token: 32 random bytes, base64url. Lifetime 30 days.
- Cookie `flylab_session`: `HttpOnly`, `SameSite=Lax`, `Path=/`, `Max-Age=2592000`,
  `Secure` when the request came over https (`r.TLS != nil` or `X-Forwarded-Proto: https`).
- The same token is accepted as `Authorization: Bearer <token>` (for scripts and tests).
- Registration is controlled by env `REGISTRATION_OPEN` (default `true`). When false,
  `POST /api/v1/auth/register` returns 403 `REGISTRATION_CLOSED`; accounts are created with the
  CLI (`flylab user create`).

Endpoints (all JSON; errors use the existing `{"error":{"code","message","request_id"}}` envelope):

| Method + path | Body | Success | Errors |
| --- | --- | --- | --- |
| `POST /api/v1/auth/register` | `{username, password, display_name?}` | 201 `{user, token}` + cookie | 403 `REGISTRATION_CLOSED`, 409 `USERNAME_TAKEN`, 422 `INVALID_USERNAME` / `WEAK_PASSWORD` |
| `POST /api/v1/auth/login` | `{username, password}` | 200 `{user, token}` + cookie | 401 `INVALID_CREDENTIALS` |
| `POST /api/v1/auth/logout` | — | 200 `{"ok":true}`, cookie cleared, session row deleted | — |
| `GET /api/v1/me` | — | 200 `{user, stats:{total_jobs, succeeded, failed, running, last_job_at}}` | 401 `UNAUTHENTICATED` |

`user` object: `{id, username, display_name, created_at}`. Never return `password_hash`.

## 3. Authorization of existing endpoints

- Public: `/health`, `/capabilities`, `/datasets`, `/groups`, `/neurons`,
  `POST /api/v1/plans/validate`.
- Auth required (401 `UNAUTHENTICATED` otherwise): `POST /api/v1/plans/parse`,
  `POST /api/v1/jobs`, `GET /api/v1/jobs`, and every `/api/v1/jobs/{job_id}/...` endpoint.
- Job endpoints are owner-only: a job that belongs to another user, or has no owner, returns
  404 `JOB_NOT_FOUND` (do not leak existence).
- The legacy un-prefixed routes (`/plans/...`, `/jobs/...`) follow the same rules.

## 4. Jobs and history

`POST /api/v1/jobs` body: `{plan_id, prompt?, title?}` (prompt ≤ 4000 chars, title ≤ 120).
The job is stored with the caller's `user_id`. Idempotency keys are scoped per user
(the same key from two users never collides).

`GET /api/v1/jobs?limit=<1..100, default 24>&offset=<n>&status=<optional>` returns only the
caller's jobs, newest first:

```json
{
  "jobs": [ HistoryJob, ... ],
  "total": 20, "limit": 24, "offset": 0
}
```

`HistoryJob` = all existing `Job` fields plus:

- `prompt`, `title` (nullable strings)
- `plan`: the stored `ExperimentPlan` (group ids, rates, duration, repeats, seed, type)
- `summary`: `{total_spikes_A, total_spikes_B, active_neurons_count_A, active_neurons_count_B,
  readout_summary}` for succeeded jobs, else `null`
- `summary_error`: string when `summary.json` exists but cannot be read or parsed, else `null`
  (a corrupt summary must be visible in the UI, not silently dropped)

`GET /api/v1/jobs/{job_id}` returns the same `HistoryJob` shape for one job.
Results, spikes, export and artifact endpoints keep their current response shapes.

## 5. Planner: natural language → plan via `claude -p`

Ollama is removed completely (config, compose, Dockerfile, README, `.env.example`, Makefile).

Config (env):

| Var | Default | Meaning |
| --- | --- | --- |
| `CLAUDE_BIN` | `claude` | path or name of the Claude Code CLI |
| `CLAUDE_MODEL` | `claude-sonnet-5-5` | model passed with `--model` |
| `CLAUDE_TIMEOUT_SECONDS` | `90` | hard timeout per parse |
| `LLM_MAX_CONCURRENCY` | `2` | global concurrent `claude -p` processes |
| `PARSE_RATE_LIMIT_PER_HOUR` | `60` | per-user parse limit; over it → 429 `RATE_LIMITED` |

Invocation (exact flags; the user prompt goes through **stdin**, never as an argument):

```
claude -p --model <CLAUDE_MODEL> --tools "" --no-session-persistence --strict-mcp-config
       --setting-sources "" --output-format json
       --system-prompt <generated system prompt> --json-schema <planner schema>
```

- The system prompt is generated from the registry (each group's id, English name, description,
  neuron count) and from the plan JSON Schema limits (duration 10–1000 ms, repeats 1–3,
  ≤ 2 activation entries, rate range from the schema, `single` vs `compare_silencing`), and states
  that whole-animal behaviour (walking, flight, grooming…) is out of scope.
- Planner schema returns `{status: "ready"|"needs_input"|"unsupported", message,
  unresolved_fields: [string], plan: {experiment_type, activation:[{group_id?, neuron_ids?, rate_hz}],
  silencing:[{group_id?, neuron_ids?}], readout:[{group_id?, neuron_ids?}], duration_ms, repeats,
  base_seed}}`.
- The CLI's JSON envelope is parsed: `is_error`, `subtype`, `structured_output`,
  `total_cost_usd`, `duration_ms`. The plan is then validated with the existing validator.
- The keyword pre-checks in `ParsePrompt` are removed (they rejected any prompt containing
  "fly", e.g. "FlyWire"); only the 4000-character limit stays. Claude decides `needs_input`
  and `unsupported`.

Response of `POST /api/v1/plans/parse` (200): the existing `ParseResult` shape, with
`llm_metadata = {source:"claude", model, duration_ms, cost_usd}`.

Failure handling (no silent fallback):

| Situation | Behaviour |
| --- | --- |
| `CLAUDE_BIN` not found on PATH | heuristic parser runs; response carries `llm_metadata.source = "heuristic_fallback"` **and** top-level `llm_error: "claude CLI not found: …"`; UI shows a visible warning banner on the plan |
| non-zero exit, `is_error: true`, timeout, missing/malformed `structured_output`, plan fails validation | 502 `LLM_ERROR` with a message that includes the reason (stderr excerpt ≤ 500 chars, or the validator error). No heuristic result. |
| concurrency slot not available within 30 s | 503 `LLM_BUSY` |

`GET /capabilities` adds `llm_provider: "claude-cli"`, `llm_model`, and `llm_ready` = whether
`CLAUDE_BIN` resolves with `exec.LookPath` (no Claude call per request).

## 6. CLI subcommands of `bin/flylab`

- `flylab` or `flylab serve` — run the server (current behaviour).
- `flylab user create --username U --password P [--display-name N]`
- `flylab user passwd --username U --password P`

Both use `DB_PATH` from the environment and print a one-line JSON result; errors exit non-zero.

## 7. Demo data

`scripts/seed_demo.py` (python3 stdlib only) talks to a running server over HTTP:

- args: `--base-url`, `--username` (default `demo`), `--password` (required), `--runs` (default 20)
- registers the user (or logs in if it exists), then submits `--runs` varied, scientifically
  sensible experiments through `POST /api/v1/plans/validate` + `POST /api/v1/jobs` with a
  human-like `title` and `prompt` each (mix of English and Russian prompts; vary sugar_grn /
  bitter_grn / ir94e activation, 20–200 Hz, 100–1000 ms, single vs compare_silencing with
  demo_silencing, readout mn9, different seeds, repeats 1–3),
- waits until every job finishes and exits non-zero unless all succeeded, printing a table of
  job_id, title, status, total_spikes_A, total_spikes_B.

## 8. Tests that must exist and pass (run on the server only)

- Auth: register → me → logout → me=401; duplicate username 409; bad password 401;
  registration closed 403; cookie and Bearer both work; password hash never in any response.
- Ownership: user B gets 404 on user A's job, results, spikes and export; `GET /jobs` lists only
  own jobs; idempotency key reuse across users does not collide.
- History: `GET /api/v1/jobs` returns `plan` and `summary` for a succeeded job; a corrupted
  `summary.json` yields `summary_error` containing a parse message (TDD: write this test first).
- Planner with a fake `CLAUDE_BIN` script (in `internal/llm/testdata/`): success → `source:"claude"`
  and validated plan; `is_error:true` → 502 `LLM_ERROR`; malformed `structured_output` → 502;
  missing binary → heuristic plan **with** `llm_error`; a prompt containing "FlyWire" is not
  rejected as unsupported.
- `scripts/smoke.sh` updated to register a throwaway user and use Bearer auth.

## 9. Backend additions (implemented; extend, do not contradict, sections 1-8)

- `GET /capabilities` also returns `registration_open` (bool) and `limits.max_prompt_chars`
  (4000), `limits.max_title_chars` (120), so the UI can hide the register form and size inputs.
- `POST /api/v1/auth/register` may also return 422 `INVALID_DISPLAY_NAME` (display name > 64
  characters). An empty `display_name` becomes the username.
- `HistoryJob` also has `plan_error`: string when the stored plan of the job cannot be loaded,
  else `null` (same visibility rule as `summary_error`). `summary_error` is also set for a
  `succeeded` job whose `summary.json` is missing or lacks one of the summary fields.
- `summary.total_spikes_B` is `null` for `single` jobs (flysim has no condition B there);
  `active_neurons_count_B` is then `0`. For `compare_silencing` a null `total_spikes_B` is
  reported in `summary_error`.
- `stats.running` in `GET /api/v1/me` counts every non-terminal job (queued, running, cancelling).
- `GET /api/v1/jobs`: a malformed `limit` (not 1..100), `offset` (< 0) or unknown `status` is
  422 `INVALID_LIMIT` / `INVALID_OFFSET` / `INVALID_STATUS` (never silently replaced by a default).
- `POST /api/v1/jobs`: 422 `PROMPT_TOO_LONG` / `TITLE_TOO_LONG` for the length limits of section 4.
- `POST /api/v1/plans/parse`: 422 `INVALID_REPORT_LANGUAGE` (not en/ru), 422 `UNKNOWN_DATASET`
  (not flywire_630); 429 `RATE_LIMITED` carries a `Retry-After` header and
  `error.details = {limit_per_hour, retry_after_seconds}`; `llm_error` is always present
  (`null` when Claude produced the result). `llm_metadata` additionally has `wall_ms`.
  A `claude` binary that exists but is not executable is 502 `LLM_ERROR` (broken install), not
  the heuristic fallback.
- `GET /api/v1/jobs/{id}/results`: 500 `RESULTS_CORRUPT` when `summary.json` cannot be parsed;
  `.../spikes`: 500 `RATES_CORRUPT` for a malformed `rates.csv` row; `.../export`: 404
  `ARTIFACTS_NOT_FOUND` when the job has no artifact directory (checked before streaming).
- Jobs created before v2 (no `user_id`) are visible to nobody (404), per section 3.
- `flylab user passwd` also revokes every session of that user and reports `sessions_revoked`.
- Malformed environment values (e.g. `PORT=abc`, `REGISTRATION_OPEN=maybe`,
  `LLM_MAX_CONCURRENCY=0`) are a startup error.
- `GET /api/v1/jobs/{id}/spikes`: a malformed `limit` (not 1..10000) or `offset` (< 0) is
  422 `INVALID_LIMIT` / `INVALID_OFFSET` (previously silently replaced by the defaults).
- Each activation set is driven at its **own** `rate_hz` (flysim used to apply the first set's rate
  to every stimulated neuron). Two activation sets that share a neuron are rejected by
  `POST /api/v1/plans/validate` (422 `VALIDATION_FAILED`, message names both sets). flysim fails
  the job, with the ids in `error_message`, if a plan's stimulated, silenced or readout neuron is
  not in the connectome graph, instead of dropping it.

## 10. Abuse limits, CSRF guard, worker health (extend sections 2-9)

Abuse limits (in memory, per process; env, malformed or < 1 is a startup error):

| Var | Default | Scope (`error.details.scope`) |
| --- | --- | --- |
| `AUTH_RATE_LIMIT_PER_IP` | `30` | `ip`: login + register attempts per client address per 15 min |
| `LOGIN_FAILURES_PER_USERNAME` | `10` | `username`: failed logins per username per 15 min; further logins of that username (even correct) are refused until the window passes |
| `REGISTER_RATE_LIMIT_PER_IP_PER_HOUR` | `5` | `register_ip`: accounts created per client address per hour (counted only for well-formed requests) |
| `PASSWORD_HASH_CONCURRENCY` | `4` | concurrent PBKDF2 computations; no slot within 2 s → 503 `AUTH_BUSY` (`Retry-After`, `details.concurrency`) |
| `PARSE_RATE_LIMIT_PER_IP_PER_HOUR` | `120` | `ip`: `/plans/parse` per client address across all accounts |
| `PARSE_GLOBAL_LIMIT_PER_HOUR` | `300` | `global`: `/plans/parse` for the whole server |

- Client address = the TCP peer, or `X-Real-IP` when the peer is loopback (the local nginx).
- Auth limits answer 429 `RATE_LIMITED` with `Retry-After` and
  `error.details = {scope, limit, window_seconds, retry_after_seconds}`.
- Parse limits answer 429 `RATE_LIMITED` with `Retry-After` and
  `error.details = {scope, limit_per_hour, retry_after_seconds}` (`scope` = `user`, `ip` or
  `global`). One account may have only one parse in flight: a second concurrent parse is 429
  with `details = {scope: "user_in_flight", limit: 1, retry_after_seconds}`. A refused request
  does not consume any hourly budget.
- `CLAUDE_BIN` given as a bare name (default `claude`): a PATH entry that exists but cannot run
  (no exec bit, dangling symlink, directory) is 502 `LLM_ERROR` "claude CLI is not usable",
  not the heuristic fallback; `capabilities.llm_ready` is then `false`.

CSRF guard, for every request that is not GET/HEAD/OPTIONS (all routes):

- `Sec-Fetch-Site`, when sent, must be `same-origin` or `none`, else 403 `CSRF_REJECTED`.
- `Origin`, when sent, must have this request's host or the configured `DOMAIN` as host, else
  403 `CSRF_REJECTED` (a sibling `*.aglabx.com` page and the opaque `null` origin included).
- `Content-Type` must be `application/json` (parameters allowed), also for bodiless POSTs
  (`/auth/logout`, `/jobs/{id}/cancel`), else 415 `UNSUPPORTED_MEDIA_TYPE`.
- The session cookie stays `SameSite=Lax` (section 2); the guard above is what stops a
  cross-site `<form enctype="text/plain">` login/logout and sibling-subdomain requests.

Worker health:

- The worker fails startup (fatal, like a failed migration) when jobs orphaned by the previous
  process cannot be marked failed.
- A job's final status is saved with brief retries; if it still cannot be saved, or the queue
  cannot be read, the worker is degraded: `GET /health` returns 503
  `{"status":"degraded", "worker_error": "<reason>"}` (`worker_error` is `null` when healthy),
  and `GET /capabilities` reports `worker_error` (string or null) and `worker_ready: false`.
  The UI shows `worker_error` as a page-level banner. An unsaved final status stays reported
  until restart (the restart's orphan recovery marks the job failed `WORKER_INTERRUPTED`).

`GET /api/v1/jobs/{id}/spikes` validates every `rates.csv` row (exact header, exactly 6
columns, non-negative integer trial and spike_count, finite non-negative rate_hz,
is_readout `true`/`false`) before filtering and paging, in CSV and JSON mode: 500
`RATES_CORRUPT` names the line. A missing file is 404 `RATES_NOT_FOUND`; any other read error
is 500 `RATES_UNREADABLE`.

`flysim replay` applies the same rule as `run`: a stimulated, silenced or readout id missing
from the graph, or an input event whose root id is not in the graph, fails the replay with the
ids instead of being dropped.
