# AGENTS.md — claude-validator

Guidance for coding agents working in this repository. Read this before editing.

## What this is

A Go CLI/TUI that validates Claude.ai session cookies stored as Netscape-format
text files. For each cookie file it calls the claude.ai API and reports whether
the session is valid, showing account details (email/plan/orgs) for valid ones.
Input is a folder scanned recursively (or a single file). Optional proxy support
via `proxies.txt` in `sticky` (one static proxy pinned per file) or `rotating`
(per-request round-robin) mode. UI is a Bubble Tea TUI; piped output is plain
text. Built to check thousands of cookie files fast without retries.

## Commands

```sh
go build ./...                 # compile all packages
go build ./cmd/claude-validator # compile binary to claude-validator(.exe)
go vet ./...                   # vet — must be clean before any commit
go test ./...                  # unit tests — must pass before any commit
gofmt -w .                     # format (goimports-style; run before committing)
go run ./cmd/claude-validator  # validate files in cookies/ (default -dir) against live claude.ai
```

Useful flags: `-dir` (file or folder; default `cookies/`), `-pattern` (default
`*` for all files in folder), `-valid-dir` (default `valid/`, folder for valid
cookies), `-invalid-dir` (default `invalid/`, folder for invalid cookies),
`-workers` (default 5), `-timeout` (default 30s, per request), `-proxies`
(default `proxies.txt`, used only if the file exists), `-proxy-mode`
(`sticky|rotating`), `-endpoint` (default `https://claude.ai`; exists so tests
can point at a local server).

## Layout

| Directory / File | Responsibility |
|---|---|
| `cmd/claude-validator/main.go` | flags, discovery wiring, worker pool, proxy selection closures, bootstrap |
| `pkg/cookie/cookie.go` | Netscape parser + recursive discovery + cookie-header building |
| `pkg/proxy/proxy.go` | proxy line auto-detection (4 formats), fail-fast loader, `NewClient`, pooled `ClientFor` |
| `pkg/validator/validator.go` | status enum, `Result`, `CheckCookieFile`, HTTP classification switch, `APIErrorDetail` |
| `pkg/tui/tui.go` | Bubble Tea model: rows, spinner, channel-driven updates, capped rendering, plain output runner |
| `tests/*_test.go` | unit tests in `/tests` (httptest-based for classification, parser tests, proxy tests) |
## Verified live-API facts (do not "fix" these back to assumptions)

These were confirmed against production claude.ai; tests in `validate_test.go`
pin the exact shapes. If claude.ai changes, re-verify live before editing
classification.

- `GET /api/organizations` — decides the status. 200 JSON array → VALID (org
  `name` fields joined with ", "). Extremely rate-limit tolerant: ~1,200
  concurrent requests produced zero 429s.
- `GET /api/account` — account details. `email_address`, `full_name`,
  `memberships[].organization.{plan_display_name, rate_limit_tier}`.
  **This endpoint rate-limits aggressively** (~50% 429s at 60× concurrency).
  A non-200 here keeps VALID and sets `Detail = "account lookup: HTTP <code>"`.
  Plan resolution order: `plan_display_name` → `rate_limit_tier` → `seat_tier`
  (free accounts have null `plan_display_name`, so `default_claude_ai` shows).
- `GET /api/auth/session` — **dead** (404 JSON `not_found_error`). Do not use.
- An invalid/expired-at-server session returns **403 JSON**
  `{"error":{"type":"permission_error","message":"Invalid authorization",
  "details":{"error_code":"account_session_invalid"}}}` — NOT 401.
- 403 classification order matters: `cf-mitigated: challenge` header wins first
  (BLOCKED), then JSON with a parseable claude.ai error body (INVALID, detail
  from `apiErrorDetail`), then non-JSON 403 (BLOCKED, HTML challenge page),
  otherwise ERROR.
- A live Cloudflare challenge (HTML "Just a moment...") has not been observed
  from Go's HTTP/2 client on a clean IP; `cf_clearance` was not required for
  API access from this environment. It is IP-bound — in sticky mode it relies
  on the pinned proxy being the harvester.
- 401 is kept in the classifier as `INVALID: unauthorized` but has not been
  observed live.
- No retry logic anywhere, by design: one request per endpoint per file,
  deterministic. Every failure becomes a row, never a stall.

## Behavior invariants — preserve when editing

- **Statuses come from HTTP code + JSON-ness only.** Unknown/missing JSON
  fields must never flip a status; decode defensively, leave row fields empty.
- **Failure of the second request (`/api/account`) must never downgrade a
  VALID verdict.** Report it in `Detail` instead.
- Pre-checks (missing `sessionKey`, locally expired) make **zero** HTTP
  requests. Tests assert this via per-path hit counters.
- `checkCookieFile` uses a **named return value** `(res Result)` with a defer
  setting `res.Elapsed`. If you refactor, keep it named — a value return plus
  `defer` mutation silently zeroes `Elapsed` (this bug shipped once already).
- Clients are pooled via `clientFor` keyed by `proxyLabel + timeout`. Sticky
  mode reuses one client per file; rotating re-picks per request via the atomic
  counter in `main.go`. Do not create clients per request outside the pool.
- `sessionKey` duplicate lines: last line wins (pinned by test). Lines with
  wrong field count (≠7 tab-separated) are ignored, never fatal. `#HttpOnly_`
  prefix lines are data lines; other `#` lines are comments.
- `-dir` pointing at a single file bypasses `-pattern` entirely.
- A file that fails to *read* becomes a `PARSE_ERROR` row — never silently
  skipped; it is finalized in the TUI before workers start and is not sent to
  the worker pool.

## TUI / output model (Bubble Tea)

- Rows are pre-created for every discovered file; work runs in background
  goroutines; results arrive as `resultMsg` via a single `waitForResult`
  channel-receive cmd that re-arms itself. Channel closes → `allDoneMsg`.
- **Two rendering regimes**, split at `maxShownRows = 40`:
  - ≤40 rows: full list, as always.
  - >40 rows: progress line + rolling window of the 15 most recent results.
    Rendering thousands of rows at 10 fps is wasted work — keep the cap.
  - Row updates are O(1) via the `byPath` map. Do not reintroduce linear scans.
- **Headless (non-TTY stdout)**: `tea.WithInput(nil)` + `tea.WithoutRenderer()`,
  auto-quit when done, then `plainOutput()` prints the complete per-file list.
  Do not print anything extra to stdout during the run. TTY stays pure TUI;
  `q`/`ctrl+c` quits, exit code 0.
- Status colors: green valid, red invalid/expired/parse_error, yellow
  rate_limited, cyan blocked, gray error.

## Project Go conventions

- Format with `gofmt` before committing; keep `go vet` and `go test` clean.
- Integer loops: prefer `for i := range n` / `for range n` over classic
  three-clause loops (Go 1.22+; module is on go 1.27).
- Boring stdlib where possible: proxy and HTTP come from stdlib; the only
  third-party deps are `bubbletea` + `lipgloss` (+ transitive). Do not add
  dependencies for things stdlib covers.
- Comments state invariants and "why", not "what". Keep them short.
- Error handling: fail fast at startup (bad proxy line, bad flag), never
  mid-run (one bad cookie is one ERROR row, not an aborted scan).

## Testing conventions

- Classification tests use `httptest.NewServer` + the `hitCountingServer`
  helper (per-path atomic counters). Pin **exact** status and `Detail` strings —
  they are user-facing output.
- Deterministic and isolated: no live-network tests in the suite. Live
  verification is a manual step (see below).
- When you change user-visible classification, update the pinned test bodies to
  the verified live shape, not to what the code happens to do.
- Coverage gaps that are known and accepted: `RATE_LIMITED` on the primary
  endpoint is simulated only (live orgs endpoint never 429s); main() flag
  wiring is exercised manually, not unit-tested.

## Live verification recipe (manual, no secrets in output)

1. Throwaway fixture server (`tmp_serve/`, delete after): serves canned
   `/api/organizations` and `/api/account`; run `go run . -dir fixtures
   -endpoint http://127.0.0.1:<port>` with crafted cookie files (valid future
   expiry / past expiry / missing sessionKey). Expect exactly 2 hits per valid
   file, 0 hits for pre-check failures.
2. Proxy path: point `proxies.txt` at a second local server port; sticky shows
   `proxy: <host:port> (sticky)`, rotating round-robins across endpoints.
3. Real cookies: `go run . -dir cookies/<file>.txt`. NEVER print, log, or commit
   cookie values. Never run more than a few hundred live requests per minute;
   `/api/account` 429s are expected under concurrency and are not a bug.

## Secrets policy (critical)

- **All session cookies live in `cookies/` — gitignored wholesale.** Any file
  in there can carry a real `sk-ant-sid02-…` session key under an arbitrary
  name, so nothing inside is ever committed. Stray-cookie patterns
  (`cookie_*.txt`, `message.txt`, `*.cookies`) are also ignored as belt-and-braces.
- Never commit, echo, screenshot, or paste cookie values. When generating test
  fixtures from real files, transform them (e.g. replace the sessionKey value)
  and still treat the result as sensitive — copies of `cf_clearance` and other
  session cookies leak session context too.
- Delete throwaway fixture dirs at the end of any session that created them.

## Commit style

Conventional Commits, strictly: `type: imperative summary` (≤72 chars), types
in use: `feat`, `fix`, `test`, `docs`, `chore`, `refactor`. Scope optional
(`feat(tui): ...`). Body wraps at 80 and explains why; breaking changes via `!`.
Never commit cookie files (see secrets policy) — `git status` must be clean of
ignored-looking `.txt` fixtures before committing.
