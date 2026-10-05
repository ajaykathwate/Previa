# 06 · Source and configuration

Everything that happens between "this PR may be built" and "here is a directory to
build": confirm the PR with GitHub right now, download exactly that commit, unpack
it without trusting it, and read `.previewrc.yaml` from the same tree. None of this
builds or runs code; image builds start in the next phase ([21-roadmap.md](21-roadmap.md)).

The PR author controls everything in the archive, including the config file, so
all of it is treated as hostile input: identity is verified, paths are confined,
sizes are capped and config is parsed strictly.

## Fresh upstream verification

Saved PR state can be minutes old, and mergeability changes when the base branch
moves even without a push. So immediately before manual authorization and before
executing eligible work, `pullrequests.SyncService.VerifyUpstream` re-reads the PR
from GitHub. It implements the consumer-owned `environments.UpstreamVerifier`
interface; the real Build action and worker connect to it in Prompt 10.

1. A short transaction captures the installation revision, the repository
   observation version and the PR generation. A locally closed PR fails closed
   without any GitHub call.
2. With no transaction open, one bounded (20 s) installation-authenticated read
   resolves the base repository by numeric ID and fetches the PR: lifecycle, head
   SHA, head repository (fork identity) and mergeability.
3. A second transaction re-locks installation → repository → PR. If any captured
   revision or the PR generation moved, the result is discarded (`ErrUpstreamChanged`).
   Otherwise the observation is recorded through the normal source-ordered path and
   committed even when the outcome is a refusal, so a closure or conflict seen here
   is never lost.

| Upstream result | Outcome | Retry? |
| --- | --- | --- |
| Open, mergeable, same head | Snapshot returned (generation, SHA, base/head repo IDs, fork flag, current names) | — |
| Open, new head | New head recorded as a newer generation; the snapshot no longer `Matches` an older request | — (old request superseded) |
| Closed or merged | Recorded; `ErrUpstreamClosed` | No |
| Conflicts / refresh required | Recorded; `ErrPRConflicts` / `ErrPRRefresh` | No |
| Mergeability still computing | Recorded; `ErrPRChecking`; a bounded recheck job is scheduled | Yes |
| 404, 403 or 410, or local access revoked | `ErrUpstreamInaccessible`; nothing recorded | No |
| Network error, 5xx, rate limit | `ErrUpstreamUnavailable` (rate limits carry `UpstreamRetryAfter`) | Yes |
| Installation, repository or PR changed during the read | `ErrUpstreamChanged`; nothing recorded | Yes |

Verification is observation, not authorization: it never creates intent in Manual
mode. If the PR had event-authorized Automatic intent and the recorded status
changed, that intent is carried to the new generation (with a fresh quiet period),
exactly as a resolving mergeability check does. Discovery never authorizes anything.

## Source preparation

`source.Preparer.Prepare` turns one verified commit into a workspace:

1. **Exact commit, through the base repository.** The archive source resolves the
   base repository by its numeric ID, then asks the API for the tarball of the full
   40-character SHA. Branch names and abbreviated SHAs are rejected up front. Fork
   PRs are fetched the same way: GitHub serves PR head commits through the base
   repository, so Previa never needs access to the fork, private or not.
2. **Credentials stay with the API.** The installation token is sent only to the
   API call that returns the archive location; the redirect is not followed by that
   client. The archive is then downloaded from `codeload.github.com` (HTTPS only,
   allowlisted host, at most 3 redirects, each re-checked) by a separate client with
   no credentials. The archive URL carries its own short-lived token, so errors never
   include it.
3. **Identity check.** `git archive` records the commit in the tarball's first
   (global) header. Preparation fails unless that header names exactly the requested
   SHA, so a different or substituted commit cannot be built. (The real-source check
   confirmed GitHub's tarballs carry this header.)
4. **Defensive extraction** into a fresh worker-owned workspace:
   - one top-level directory; canonical, local paths only (no `..`, absolute paths,
     `a/./b` or `a//b`), names of at most 255 bytes;
   - only directories, regular files and relative symlinks that stay inside the tree;
     hard links, devices, FIFOs and sparse entries are rejected;
   - duplicate or overlapping entries (a file beneath a symlink or file) are rejected;
   - symlinks are created only after every file exists, so no write passes through
     one, and every write goes through Go's `os.Root`, which refuses to escape;
   - file modes keep only the executable bit, never setuid/setgid/sticky.
5. **Config from the same tree.** `.previewrc.yaml` must be a regular file at the
   root (not a link), opened with `O_NOFOLLOW`, and is parsed by `repoconfig.Parse`
   with the same defaults, clamping and hash as below. The configured Dockerfile
   must resolve, through `os.Root`, to a regular file inside the tree. The tree
   itself is the build context.

| Limit (`source.DefaultLimits`) | Default |
| --- | --- |
| Compressed archive | 256 MiB (declared length checked before reading) |
| Extracted file bytes | 1 GiB |
| Entries (files, directories, links) | 100,000 |
| Path length below the top directory | 2,048 bytes |
| Archive redirects | 3 |
| Whole preparation | 5 minutes, and the caller's cancellation |

### Workspaces and leftovers

Each preparation gets `<root>/previa-src-<pr>-g<generation>-r<revision>-<random>/`
with mode `0700`: a `workspace.json` marker (PR, generation, revision, commit, time,
PID) written **before** extraction, and the tree in `source/`. Any failure or
cancellation removes the workspace; `Preparer.Release` removes it after the build.
If the process crashes, `Workspaces.List` still finds every leftover (including
unmarked ones, by modification time) and `Workspaces.Remove` deletes them safely
(root-confined, never follows links), ready for housekeeping in Prompt 9.

### Failure codes

`source.Error` carries a safe message, a code and whether retrying can help:
`invalid_request`, `authentication_unavailable`, `access_denied`,
`resource_unavailable`, `rate_limited` (with `RetryAfter`), `api_unavailable`,
`api_error`, `redirect_rejected`, `request_cancelled`, `too_large`,
`too_many_files`, `unsafe_archive`, `invalid_archive`, `identity_mismatch`,
`missing_file`, `invalid_config` (wrapping `repoconfig.ValidationError`),
`missing_dockerfile` and `workspace_unavailable`.

### Real-source check

```sh
TEST_GITHUB_INSTALLATION_ID=<id> TEST_GITHUB_REPO_ID=<id> TEST_GITHUB_PR_NUMBER=<n> make test-source
```

It reads the PR, prepares its head commit with the production archive source and
logs what it verified; it never prints tokens or URLs and writes nothing to GitHub.
Find the IDs with the query in [02](02-development-setup.md). Recorded run
(2026-10-04): a same-repository PR's archive was fetched through the base
repository, its header declared exactly the head SHA, the tree extracted safely,
and the outcome was `missing_file` because that repository has no
`.previewrc.yaml` yet; the workspace was removed.

## The `.previewrc.yaml` file

Place `.previewrc.yaml` at the repository root. It is read from the exact commit
being built (above), not the default branch. `internal/repoconfig` also keeps a
standalone API fetcher for reading the file without downloading the archive.

### YAML contract

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
is owner-controlled platform data; see [15](15-build-policy-and-intents.md).

### Resource tiers and ceilings

| Tier | CPU limit | Memory limit |
| --- | --- | --- |
| `small` | 500 millicpu / 0.5 vCPU | 512 MiB |
| `standard` | 1000 millicpu / 1 vCPU | 1 GiB |
| `large` | 2000 millicpu / 2 vCPU | 2 GiB |

These are per-preview container limits, not reservations, subscription plans or
Docker build-process budgets. Default platform ceilings are **2000 millicpu and
2147483648 bytes (2 GiB)**. `DefaultLimits()` supplies these values.

Platform callers can pass positive `Resources{CPUMillis, MemoryBytes}` ceilings to
`NewService` or `Parse`. There is no `.env` setting for ceilings yet.
Lower ceilings clamp the selected tier and return `cpu_clamped`/`memory_clamped`
notices alongside the effective values. For example, large under 750 millicpu /
768 MiB ceilings becomes 0.75 vCPU / 768 MiB; the requested tier stays visible.
Unknown tiers and malformed declarations are rejected rather than clamped.
Higher ceilings alone do not enlarge the fixed tier mappings.

Clamping protects host policy but does not guarantee the app fits. Runtime adapters
must enforce the returned limits; this parser does not apply Docker restrictions.
Concurrency, active-preview caps, expiry and cleanup remain separate requirements.

### Validation and errors

- Dockerfile paths cannot be absolute, contain backslashes/drive notation, controls,
  traversal segments or name only a directory. Valid leading `./` is normalized.
- This is lexical validation. Source preparation then checks the real tree: the
  Dockerfile must resolve through `os.Root` to a regular file inside the extracted
  source (see Source preparation above).
- Env names follow `[A-Za-z_][A-Za-z0-9_]*`; duplicates are rejected. Empty values
  and additional `=` characters in values are allowed. NUL values are rejected.
  Use this YAML for non-secret values only; store sensitive values as repository secrets ([07](07-repository-secrets.md)). A secret overrides a YAML entry with the same name.
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

### Feature API and effective hash

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
- Persisting the hash on the environment and posting config errors to the PR arrive with the build pipeline and PR feedback (roadmap Prompts 8 and 10).

## Tests

```sh
go test -count=1 -race ./internal/source ./internal/repoconfig
TEST_DATABASE_URL=... go test -count=1 -run 'TestVerifyUpstream|TestInspect' ./internal/pullrequests
```

- **Verification** (PostgreSQL): fresh state recorded without authorizing; a moved
  head; closed, conflicting, still-checking (recheck scheduled), inaccessible,
  unavailable and rate-limited upstreams; installation, webhook and access changes
  during the read; Automatic intent carried across a status change; GitHub error
  classification without response details.
- **Source**: a verified tree with modes and an in-tree link; missing or wrong commit
  header; traversal, absolute, non-canonical and multi-root paths; absolute and
  escaping links; writes through a link; hard links, devices, FIFOs; duplicates;
  entry, extracted-size, compression-bomb and compressed-size caps; long paths;
  malformed and truncated archives; config and Dockerfile rules; cancellation; crash
  leftovers. Archive tests use a fake API plus a TLS archive host to prove the token
  reaches only the API, redirects to other or plain-HTTP hosts are never contacted,
  redirects are bounded and errors never contain the archive URL.
- **Config**: YAML fixtures under `internal/repoconfig/testdata` (valid/minimal,
  unknown/duplicate keys, traversal, invalid port/env/health path, tiers, clamping)
  and the standalone API fetcher's error classification.
