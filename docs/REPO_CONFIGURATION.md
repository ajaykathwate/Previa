# Repository preview configuration

Place `.previewrc.yaml` at the repository root. Previa reads it at the exact
requested full commit SHA using the existing GitHub App installation credentials,
not the default branch or a contributor's OAuth token.

The standalone feature is implemented in `internal/repoconfig`. It is not wired
to jobs, webhooks, pages, containers or GitHub comments. This guide describes its
approved parsing contract, not a claim that installing the App builds a preview.

## YAML contract

```yaml
dockerfile: ./Dockerfile
port: 3000
env:
  - NODE_ENV=preview
health_check_path: /health
resource_tier: standard
```

| Key | Type | Default when omitted |
| --- | --- | --- |
| `dockerfile` | Relative file path string | `Dockerfile` at repository root |
| `port` | Integer, 1–65535 | `3000` inside the preview container |
| `env` | List of `NAME=value` strings | Empty list |
| `health_check_path` | Local absolute HTTP path string | No configured HTTP health endpoint |
| `resource_tier` | `small`, `standard` or `large` string | `standard` |

The file itself is required: a missing file is an actionable acquisition error.
`{}` is a valid minimal file. A blank file, explicit nulls or incorrect field types
are invalid. The port does not configure the application; set it to the port the
app actually listens on. Omitted or empty health path does not imply `/health`.
Later runtime integration must define readiness without weakening health-gated
publication when no HTTP endpoint is declared.

Only one YAML mapping/document is supported. Unknown/duplicate keys, tags, anchors,
aliases, merges and complex keys are rejected. Numeric resource objects, build mode
and Automatic debounce settings are not allowed in repository YAML. Build policy
is owner-controlled platform data; see [BUILD_POLICY_AND_USER_FLOW.md](BUILD_POLICY_AND_USER_FLOW.md).

## Resource tiers and ceilings

| Tier | CPU limit | Memory limit |
| --- | --- | --- |
| `small` | 500 millicpu / 0.5 vCPU | 512 MiB |
| `standard` | 1000 millicpu / 1 vCPU | 1 GiB |
| `large` | 2000 millicpu / 2 vCPU | 2 GiB |

These are per-preview container limits, not reservations, subscription plans or
Docker build-process budgets. Default platform ceilings are **2000 millicpu and
2147483648 bytes (2 GiB)**. `DefaultLimits()` supplies these values.

Platform callers can pass positive `Resources{CPUMillis, MemoryBytes}` ceilings to
`NewService` or `Parse`. No new `.env` setting is added in this standalone phase.
Lower ceilings clamp the selected tier and return `cpu_clamped`/`memory_clamped`
notices alongside the effective values. For example, large under 750 millicpu /
768 MiB ceilings becomes 0.75 vCPU / 768 MiB; the requested tier stays visible.
Unknown tiers and malformed declarations are rejected rather than clamped.
Higher ceilings alone do not enlarge the fixed tier mappings.

Clamping protects host policy but does not guarantee the app fits. Runtime adapters
must enforce the returned limits; this parser does not apply Docker restrictions.
Concurrency, active-preview caps, expiry and cleanup remain separate requirements.

## Validation and errors

- Dockerfile paths cannot be absolute, contain backslashes/drive notation, controls,
  traversal segments or name only a directory. Valid leading `./` is normalized.
- This is lexical validation. Source extraction/build integration must check symlinks,
  real filesystem containment and whether the referenced Dockerfile exists.
- Env names follow `[A-Za-z_][A-Za-z0-9_]*`; duplicates are rejected. Empty values
  and additional `=` characters in values are allowed. NUL values are rejected.
  Use this YAML for non-secret values; preview secret storage is a separate feature.
- Health paths start with a single `/` and cannot include a host, query, fragment,
  whitespace or traversal. They are validated, not executed.
- Config input is capped at **64 KiB**. GitHub API envelopes are capped at **128 KiB**
  before decoding. Fetches have a **15-second overall context budget**, respecting
  earlier caller deadlines and the existing installation client's HTTP timeout.

`ValidationError.Issues` contains `field`, `code`, `message` and available line/column
locations. Known fields and env indexes are identified. Unknown/duplicate key
errors use `$` plus source location rather than reflecting arbitrary source text.
Syntax failures are sanitized: no YAML snippets, env values or raw provider errors.
Independent semantic validation problems are returned together where possible.

`FetchError` separates acquisition failures from validation:

| Code | Meaning | Retry automatically? |
| --- | --- | --- |
| `missing_file` | Exact commit is accessible but root config is absent | No |
| `resource_unavailable` | Repository/commit unavailable or access masked as 404 | No |
| `access_denied` | Explicit installation permission/authentication rejection | No |
| `authentication_unavailable` | Installation client cannot be constructed | No |
| `rate_limited` | GitHub API rate limit | Yes, with later orchestration backoff |
| `api_unavailable` | Network/API connection failure | Yes |
| `api_error` | Other API response failure | 5xx/408 retryable |
| `request_cancelled` | Caller cancellation or deadline expired | Deadline retryable; caller cancellation is not |
| `too_large` / `invalid_file` / `invalid_response` | Unsafe size or malformed/unsupported response | No |

GitHub can conceal permission failures with 404. After a content 404, the fetcher
uses the SHA-only commit API to verify the requested commit. It reports a missing
file only after that verification; otherwise it preserves access/rate/transient
classification. It cannot distinguish a masked access failure from a nonexistent
repository/commit, so `resource_unavailable` states both possibilities.

## Feature API and effective hash

- `Parse(data, limits)` returns normalized `Config`, clamp `Notices` and a SHA-256 hash.
- `NewGitHubFetcher(clients)` accepts the small `InstallationClients` interface,
  satisfied by the existing `platform/githubapp.Client`. Token minting/caching stays
  in that platform transport; tokens are neither returned nor stored by this feature.
- `NewService(fetcher, limits).Load(ctx, request)` fetches and parses. Requests carry
  installation ID, owner/repo and a full 40-character commit SHA; branch names and
  abbreviated SHAs are rejected. Caller authorization remains required.
- `FileFetcher` returns an owned byte slice; `Load` clears it after parsing.
- Hashing covers normalized runtime settings and effective limits. Env entries are
  sorted after duplicate-name rejection. Formatting, explicit defaults and tier
  labels resolving to identical effective limits do not change the hash; changed
  env values, port, Dockerfile, health path or effective resources do.
- Config/result values contain env values: never log them. The hash is not encryption
  or a secret-safe fingerprint; do not use it to publish information about secrets.
- Hash persistence and later PR feedback remain future integration work.

## Fixture and test checks

```sh
go test -count=1 -v ./internal/repoconfig
go test -count=1 -race ./internal/repoconfig
```

These tests need no database, `.env`, real GitHub account or webhook. YAML fixtures
under `internal/repoconfig/testdata` cover valid/minimal config, unknown/duplicate
keys, traversal/absolute paths, invalid port/env/health path, malformed resources,
unknown tier and over-ceiling resolution. `missing_file.json` drives a simulated
GitHub 404 with separate exact-commit verification. The over-ceiling fixture selects
large under lower platform test ceilings because large equals the default maximum.

Further tests cover strict types/documents, hashes, aggregated safe errors, exact
API path/ref/auth transport, masked access, rate limits, API failures, oversized
responses/files and deadline cancellation. Fake GitHub APIs prove the adapter
contract; no real GitHub call or preview deployment is claimed.
