# AIValidator

High-throughput, concurrent session and cookie validator for Claude.ai and ChatGPT web interfaces.

AIValidator scans directories of cookie dumps (Netscape or JSON formats), validates authentication state against upstream endpoints, extracts account details (email, subscription plan, organization, token expiration), sorts valid and invalid files on disk, and logs results in real time through an interactive terminal interface (TUI) or structured plain text.

---

## Architecture and Subprojects

The repository is structured into two independent Go modules, each targeting a specific provider's authentication pipeline:

```
AIValidator/
├── claude/
│   ├── cmd/claude-validator/   # Entry point and CLI flag parsing
│   ├── pkg/
│   │   ├── cookie/             # Netscape format parser and recursive discovery
│   │   ├── proxy/              # Proxy loader, line parser, and HTTP client pool
│   │   ├── validator/          # Claude API validation and HTTP classification logic
│   │   └── tui/                # Bubble Tea interactive UI and plain output renderer
│   ├── tests/                  # Unit tests with mock HTTP test servers
│   ├── AGENTS.md               # Internal developer notes and live-API invariants
│   ├── go.mod
│   └── go.sum
└── chatgpt/
    ├── cmd/chatgpt-validator/  # Entry point and CLI flag parsing
    ├── pkg/
    │   ├── app/                # Application lifecycle, workers, and file pipeline
    │   ├── cookie/             # Netscape/JSON parser, streaming walker, fast counter
    │   ├── proxy/              # Proxy parser, multi-threaded health checker, client pool
    │   ├── validator/          # NextAuth session and backend API verification logic
    │   └── tui/                # Dual-waterfall Bubble Tea interface
    ├── tests/                  # Unit tests for parser, proxy, and classification
    ├── go.mod
    └── go.sum
```

---

## How It Works

### Validation Flow: Claude.ai (`claude/`)

1. **Local Pre-Check:**
   - Parses the Netscape cookie file.
   - Extracts the `sessionKey` cookie value and its UNIX expiration timestamp.
   - If `sessionKey` is missing or its timestamp is in the past, marks the file `INVALID` or `EXPIRED` immediately without initiating an HTTP request.
2. **Session Verification (`GET /api/organizations`):**
   - Sends requests with realistic desktop Chrome headers (`Sec-Ch-Ua`, `Sec-Fetch-*`, `User-Agent`).
   - A `200 OK` JSON array response confirms a valid session and extracts associated organization names.
   - This endpoint exhibits high rate-limit tolerance under heavy concurrency.
3. **Account Metadata Extraction (`GET /api/account`):**
   - Fetches the account email, full name, and subscription tier (`plan_display_name` -> `rate_limit_tier` -> `seat_tier`).
   - The account endpoint rate-limits aggressively (~50% HTTP 429 under high concurrency). If this secondary request returns 429 or fails, the verdict remains `VALID`, with the error logged in the detail column rather than downgrading the result.
4. **WAF & Error Classification:**
   - Detects Cloudflare mitigation headers (`cf-mitigated: challenge`) and HTML challenge pages, classifying them as `BLOCKED`.
   - Distinguishes expired sessions returning `403 Forbidden` with JSON error payload (`account_session_invalid`) as `INVALID`.

### Validation Flow: ChatGPT (`chatgpt/`)

1. **Local Pre-Check & Token Ingestion:**
   - Reads files up to 512 KB to prevent memory exhaustion on corrupted files.
   - Supports 7-column Netscape format (tab or whitespace separated, with `#HttpOnly_` prefix handling) and JSON exports (`[{"name": "...", "value": "..."}]` or `{"name": "value"}`).
   - Extracts or generates a UUIDv4 `oai-did` device identifier.
2. **Session Extraction (`GET /api/auth/session`):**
   - Queries the NextAuth session endpoint with browser headers.
   - Validates the presence of an active user email and JWT `accessToken`.
   - Decodes JWT claims to retrieve `chatgpt_account_id`, `plan_type` (`Free`, `Plus`, `Team`, `Pro`, `Enterprise`), and expiration timestamp (`exp`).
3. **Backend API Probe (`GET /backend-api/me` or `/models`):**
   - Dispatches a secondary probe with `Authorization: Bearer <accessToken>` and the optional `chatgpt-account-id` header to verify backend permissions.
   - If the token is rejected by the backend API, the file is classified as `INVALID`.
4. **Network & DNS Fault Isolation:**
   - Differentiates between dead sessions and transient network/DNS drops (socket timeouts, host resolution failures, proxy dropouts).
   - Transient failures are classified as `RETRY` and kept in place, preventing temporary connection issues from corrupting valid session dumps.

---

## Core Features

- **High-Throughput Streaming Engine:**
  - Fast directory pre-counter scans file metadata without loading file bodies into memory.
  - Producer-consumer channel pipeline reads and parses files on demand, maintaining flat memory usage even on 100,000+ file batches.
- **Connection and Client Pooling:**
  - HTTP clients are pooled and reused based on proxy endpoint and configured timeout.
  - Supports both `sticky` mode (pins a single proxy to a file across all its verification probes) and `rotating` mode (round-robin proxy selection per request).
- **Proxy Format Auto-Detection:**
  - Directly parses four common proxy representations without requiring manual schema definitions:
    - `protocol://user:pass@host:port` (HTTP, HTTPS, SOCKS5)
    - `host:port:user:pass`
    - `user:pass@host:port`
    - `host:port`
  - Optional pre-flight proxy health checking verifies candidate proxy responsiveness before processing begins.
- **Automated Disk Organization:**
  - Automatically moves files into target output directories (`valid/` and `invalid/`) as checks finish.
  - Collision-safe file relocation appends numeric counters if matching filenames already exist in the destination.
- **Real-Time Result Logging:**
  - Valid sessions are streamed to `valid_sessions.txt` with full metadata (file path, email, plan tier, expiration date, and proxy address).
- **Interactive Terminal UI and Headless Detection:**
  - Dual-waterfall Bubble Tea interface in the ChatGPT validator separates all incoming events from confirmed valid sessions.
  - Claude validator provides an interactive folder selector (`-tui`) and a rolling window when result counts exceed 40 rows.
  - Automatically detects non-TTY environments (CI/CD pipelines, piped stdout, log redirects) and switches to clean, line-delimited plain text.

---

## Installation

Ensure Go 1.22 or newer is installed on your system.

### Build Claude Validator

```sh
cd claude
go build -o claude-validator ./cmd/claude-validator
```

On Windows:

```powershell
cd claude
go build -o claude-validator.exe ./cmd/claude-validator
```

### Build ChatGPT Validator

```sh
cd chatgpt
go build -o chatgpt-validator ./cmd/chatgpt-validator
```

On Windows:

```powershell
cd chatgpt
go build -o chatgpt-validator.exe ./cmd/chatgpt-validator
```

---

## Usage

### Claude Validator

```sh
# Basic scan against a folder of cookies
./claude-validator -dir ./cookies -workers 8

# Scan with proxy pool in sticky mode
./claude-validator -dir ./cookies -proxies proxies.txt -proxy-mode sticky -workers 16

# Launch interactive folder selection TUI
./claude-validator -tui

# Validate a single file directly
./claude-validator -dir ./cookies/account_01.txt
```

#### CLI Options: Claude

| Flag | Aliases | Default | Description |
|---|---|---|---|
| `-dir` | `-folder`, `-f`, `-d` | `cookies` | Directory or single cookie file to inspect |
| `-pattern` | | `*` | Glob pattern for file names when `-dir` is a directory |
| `-tui` | | `false` | Launch interactive directory picker TUI (requires TTY) |
| `-workers` | `-w` | `5` | Number of concurrent validation workers |
| `-timeout` | `-t` | `30s` | Per-request network timeout |
| `-endpoint` | `-e` | `https://claude.ai` | Base API endpoint URL |
| `-proxies` | `-proxy`, `-p` | `proxies.txt` | Path to proxy list (optional; skipped if not found) |
| `-proxy-mode` | `-mode`, `-m` | `sticky` | Proxy assignment: `sticky` or `rotating` |
| `-valid-dir` | `-valid`, `-v` | `valid` | Destination directory for valid cookie files |
| `-invalid-dir` | `-invalid`, `-i` | `invalid` | Destination directory for invalid cookie files |

---

### ChatGPT Validator

```sh
# Run validator across target directory
./chatgpt-validator -dir ./cookies -workers 12

# Run with proxy verification and round-robin rotation
./chatgpt-validator -dir ./cookies -proxies proxies.txt -proxy-mode rotating -workers 16

# Skip proxy health pre-check for faster startup
./chatgpt-validator -dir ./cookies -proxies proxies.txt -no-check-proxies

# Disable automatic file relocation by setting target folders to empty strings
./chatgpt-validator -dir ./cookies -valid-dir "" -invalid-dir ""
```

#### CLI Options: ChatGPT

| Flag | Aliases | Default | Description |
|---|---|---|---|
| `-dir` | `-folder`, `-f`, `-d` | `cookies` | Directory or single cookie file to inspect |
| `-pattern` | | `*` | Glob pattern for file names when `-dir` is a directory |
| `-workers` | `-w` | `8` | Number of concurrent validation workers |
| `-timeout` | `-t` | `15s` | Per-request network timeout |
| `-endpoint` | `-e` | `https://chatgpt.com` | Base API endpoint URL |
| `-proxies` | `-proxy`, `-p` | `proxies.txt` | Path to proxy list or direct proxy string |
| `-proxy-mode` | `-mode`, `-m` | `sticky` | Proxy assignment: `sticky` or `rotating` |
| `-no-check-proxies` | `-ncp` | `false` | Skip pre-flight proxy connectivity check |
| `-valid-dir` | `-valid`, `-v` | `valid` | Destination directory for valid cookie files |
| `-invalid-dir` | `-invalid`, `-i` | `invalid` | Destination directory for dead or expired cookie files |

---

## Status Classification Reference

Every inspected file produces a status classification:

| Status | Meaning | Action Taken |
|---|---|---|
| `VALID` | Session authenticated successfully; account metadata retrieved | Moved to `valid/`; recorded in `valid_sessions.txt` |
| `INVALID` | Session token rejected by upstream API or missing required fields | Moved to `invalid/` |
| `EXPIRED` | Cookie expiration timestamp has passed | Moved to `invalid/` |
| `RATE_LIMITED`| Upstream returned HTTP 429 on primary verification endpoint | Retained in place |
| `BLOCKED` | Cloudflare challenge or WAF obstruction detected | Retained in place |
| `RETRY` | Transient network failure, DNS error, or proxy connection drop | Retained in place |
| `PARSE_ERROR` | File could not be opened or parsed | Retained in place |
| `EMPTY` | File contained no parseable cookies | Retained in place |
| `ERROR` | Unexpected HTTP status code or client-side failure | Retained in place |

---

## Input Formats

### Cookie Files

1. **Netscape / curl format (standard text file):**
   ```tsv
   # Netscape HTTP Cookie File
   .claude.ai	TRUE	/	TRUE	1772457800	sessionKey	sk-ant-sid02-...
   #HttpOnly_.chatgpt.com	TRUE	/	TRUE	1772457800	__Secure-next-auth.session-token	eyJhbGci...
   ```
2. **JSON format (ChatGPT validator):**
   ```json
   [
     {
       "name": "__Secure-next-auth.session-token",
       "value": "eyJhbGci..."
     }
   ]
   ```
   Or key-value pairs:
   ```json
   {
     "__Secure-next-auth.session-token": "eyJhbGci..."
   }
   ```

### Proxy Lists (`proxies.txt`)

Lines beginning with `#` and empty lines are ignored. Lines are evaluated automatically:

```text
# Protocol URI
socks5://user:pass@192.168.1.50:1080
http://10.0.0.1:8080

# Delimited formats
192.168.1.50:8080:myuser:mypass
myuser:mypass@192.168.1.50:8080
192.168.1.50:8080
```

---

## Security and Secrets Policy

- Never commit real session cookies, tokens, or dumps into source control.
- In both submodules, default cookie directories (`cookies/`, `valid/`, `invalid/`) and candidate files (`proxies.txt`, `*.cookies`, `valid_sessions.txt`) are excluded via `.gitignore`.
- Test suites rely exclusively on local fixtures and standard library HTTP test servers (`httptest.NewServer`). No outbound network requests are made during test execution.
