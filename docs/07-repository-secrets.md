# 07 · Repository secrets

Owners give previews the configuration they need at runtime (API keys, database
URLs) without putting it in the repository, without exposing it to fork authors and
without leaking it into logs. This document covers how values are stored, who
receives them, how keys rotate and what redaction can and cannot do. The preview
runtime ([10](10-preview-runtime.md)) calls the worker contract below after a fresh
access and fork recheck. The worker needs the same keyring as the server; without
it, a repository with secrets fails closed (`secrets_unavailable`) and repositories
without secrets build normally.

## Decisions

| Decision | Choice | Why |
| --- | --- | --- |
| Where owners manage secrets | A **Secrets** tab on each repository (`/repositories/{id}/secrets`) | Keeps the PR list focused; both tabs are ordinary pages, so they work without JavaScript. |
| How they are managed | **One `.env` file per repository** (owner decision, 2026-10-23), kept with its comments and order, shown with values masked and opened in full by **Show values and edit**; each save replaces the repository's secrets with the names the file sets, in **one transaction** | Teams keep a whole `.env`; one file is easier to manage than a growing list of names, and a half-saved file would be harder to reason about than a refused one. This replaces the earlier write-only, per-name editor. |
| Encryption | Standard-library AES-256-GCM, random 96-bit nonce per value | Authenticated encryption with no extra dependency. |
| Key provisioning | `SECRETS_ENCRYPTION_KEYS` + `SECRETS_ACTIVE_KEY_VERSION` environment variables | Matches how `SESSION_SECRET` is provided; versioned for rotation. |
| Name collision with `.previewrc.yaml` env | **The stored secret wins**, and the override is reported by name | The owner's value must beat contributor-editable YAML. |
| Fork PRs | **Never** receive secrets: any mode, any retry, any approval | The fork's author controls the code that would receive them. |
| When values are decrypted | In worker memory, before an allowed build or container creation | Decrypted only for verified same-repository heads; build credentials use ephemeral mounts or registry authentication. |
| Maintainer approval for forks | **Not** part of the MVP | Optional approval would not change the rule above, so it adds risk without benefit now. |

## Storage format

`repository_env_files` (migration 47) stores the file Owners and Admins edit, one
row per repository: `encrypted_text` sealed like a value with the name `.env` as
associated data (no secret can have that name, so neither can stand in for the
other), its `key_version`, a `revision` that increases with each save, and who
saved it when. It is removed with its repository.

`env_secrets` (migration 12) stores one row per repository and name, and is what
previews, builds and redaction read. Saving the file keeps it equal to the names
the file sets:

- `encrypted_value` = 12-byte random nonce ‖ AES-256-GCM ciphertext ‖ 16-byte tag.
- The repository ID, the name and the key version are **authenticated associated
  data**. A value copied to another repository or name, or relabelled with another
  version, fails to decrypt.
- `key_version` records which key sealed it; `updated_at` is shown to owners.
- Migration 28 adds `build` (false by default); only the owner may allow build use. Replacing or rotating a value preserves that permission; deletion removes it.
- Database checks enforce the name format and a ciphertext size consistent with
  1 byte – 32 KiB of plaintext, even if application code regressed.

Rules enforced by `secrets.Service`:

| Rule | Limit |
| --- | --- |
| Name | `[A-Za-z_][A-Za-z0-9_]*`, up to 128 characters, case-sensitive |
| Reserved | names starting with `PREVIA_` (kept for platform-injected variables) |
| Value | 1 byte to 32 KiB, no NUL bytes; textarea line breaks are stored as `\n` |
| File | up to 64 KiB, shown as "1.2 KiB of 64 KiB" in the header and, live, under the editor |
| Count | 500 names with values per file |
| Who | repository Owners and Admins (the `manage` ability); new or changed values also need current GitHub access, removing names and editing comments do not |

Values are hidden by default: the page shows the file's comments, blank lines and
names with every value masked (`NAME=••••••••`, or *not set* for `NAME=`).
**Show values and edit** (`?edit=1`) opens the whole file to Owners and Admins,
sent with `Cache-Control: no-store`. A refused save shows the submitted text back
to the person who typed it. Values never appear in jobs, logs, metrics or errors.

### The .env file

`POST /repositories/{id}/secrets` takes the file (`env`) and the `revision` the
editor opened. `secrets.ParseDotenv` reads it: one `NAME=value` per line, blank
lines and `#` comments kept in the file, an optional `export ` prefix, single-quoted
literals, double-quoted values with `\n`, `\t`, `\"` and `\\` escapes that may span
lines, and unquoted values ending at ` #`. `Service.SaveFile` checks every line
(format, reserved prefix, a name set twice, value size, size and count limits)
before sealing anything; errors name the line, never its value, and nothing is
saved. A name left empty (`NAME=`) stays in the file but is not set, as in any
`.env` loader. Comparing with the stored values gives the names added, changed and
removed; a save that removes names returns them for confirmation (a modal over the
editor, or an inline confirmation without JavaScript) and changes nothing until
confirmed. `Repository.SaveEnvFile` then, under the installation and repository
locks and only while the file is still at the opened revision (otherwise the
editor reopens with the typed text and says someone else saved first), stores the
file, deletes the names the file no longer sets and writes new and changed values
in one transaction. Build access stays with a name through value changes and is
removed with it. A repository whose secrets predate the file opens with one
`NAME=value` line per secret (revision 0) until its first save. `Service.Put`
sets one name by rewriting its line in place or appending one.

## Keys: provisioning and rotation

```dotenv
SECRETS_ENCRYPTION_KEYS=1:<base64 of 32 random bytes>,2:<…>
SECRETS_ACTIVE_KEY_VERSION=2
```

- Generate each key with `openssl rand -base64 32`. Versions are positive integers.
- **Server**: needs the keyring to encrypt new values. Without it, the Secrets tab
  explains that secrets are not configured and existing names stay visible.
- **Worker**: reads the same keyring to decrypt at container start. It is optional
  there: without it the worker logs that repositories with secrets cannot start
  previews, builds repositories without secrets normally, and fails a repository
  with secrets closed (`secrets_unavailable`) instead of starting it without them.
- Startup fails with a message that never echoes key material if the keyring is
  malformed: bad base64, wrong length, duplicate or missing versions.

Rotation, with no downtime:

1. Add a new version to `SECRETS_ENCRYPTION_KEYS` everywhere (keep the old one).
2. Set `SECRETS_ACTIVE_KEY_VERSION` to the new version; restart server and worker.
   New and replaced values now use the new key; old values still decrypt.
3. Run `make secrets-reencrypt`. It moves every value and every repository's `.env`
   file onto the active key in batches, then does the same for the review page's
   sealed test-access passwords
   ([31](31-test-data.md#test-access)), is safe while the server takes edits (a
   concurrent edit wins) and is safe to repeat. It prints counts only, one set of
   lines for `Secrets:` and one for `Test access:`.
4. When it reports that every value uses the active key, remove the old version and
   restart. If an old key is removed too early, decryption reports an unknown key
   version and re-encryption stops without changing data.

## Who receives secrets

The worker calls `secrets.Provider.ForContainer(repoID, source, yamlEnv)` right
before creating the container. `source` comes from fresh upstream verification
([06](06-source-and-configuration.md)): base and head repository IDs.

- **Trusted** only when the head repository is provably the base repository.
  Forks, deleted head repositories and missing or ambiguous identity are
  untrusted. For untrusted sources nothing is even decrypted; the container gets
  the YAML env and a notice that secrets were withheld.
- For trusted sources, every runtime value except the pull-only `REGISTRY_AUTH` is decrypted; any value that fails (wrong key,
  tampering, unknown version) fails the whole call, so a container never starts with
  a partial environment. Secrets override YAML entries of the same name.
- The result is a `ContainerEnv`. Its `Environ()` goes only to the runtime's
  container configuration. It must never become Docker build arguments, part of the
  build context or an image layer. Explicit build opt-in uses the separate `ForBuild` contract below. `Wipe()` clears the
  decrypted bytes afterwards (Go string copies handed to the runtime cannot be wiped,
  so keep their lifetime short).

## Opt-in build credentials

On the Secrets tab, **Allow** under Builds opens a confirmation modal. Its
CSRF-protected `POST …/secrets/build` toggles the selected secret's build permission;
allowing requires current repository access and the installation owner. **Stop**
withdraws future build access, even if GitHub access was removed. Already-running
builds keep credentials already handed to them.

The runtime asks `Provider.ForBuild` for the authorized source and rechecks the
current request after source preparation, immediately before BuildKit execution.
Forks, deleted heads and unverified identities get no decrypted values; the
builder independently refuses secret/auth session providers for different or
missing head repository IDs. Only `build=true` values are available to Dockerfile
steps, through BuildKit's in-memory secret provider:

```dockerfile
RUN --mount=type=secret,id=NPM_TOKEN,env=NPM_TOKEN,required=true npm ci
```

Configure npm to read that variable, for example with a committed `.npmrc`
containing `//registry.npmjs.org/:_authToken=${NPM_TOKEN}`. For pip or Go, mount the
credential/configuration file their client reads or use its supported environment
variable. Secret mounts make the value available; they do not configure the client.

No values are passed as build arguments or written into the build context.
`REGISTRY_AUTH` is special: one `host username token` per line (blank lines and
comments allowed), for example `ghcr.io preview-bot <test-token>`. Registry names
must be unique, ports valid, and tokens at least four characters. Invalid entries
are refused without echoing them. When allowed, it supplies credentials only for
matching registry pulls; Dockerfile steps and the application never receive it.

`BuildSecrets.Redactor` masks values and each registry token in the build output;
returned build failures are redacted too. `Wipe` clears held decrypted bytes after
the attempt. This cannot stop trusted branch code from deliberately copying a
mounted secret into layers or caches, transforming it, or sending it elsewhere.
Review the Dockerfile and use narrowly scoped test credentials. Changing a secret
does not itself invalidate BuildKit's layer cache; change a non-secret cache input
when a step must rerun.

## Redaction

`ContainerEnv.Redactor(extra...)` builds a `secrets.Redactor` from the injected
values plus platform credentials (for example the installation token used to
fetch source). Build and runtime log pipelines must pass output through its
`Writer` before storing or streaming it (Prompt 3).

- Catches exact values even when split across writes: it holds back at most one
  value's length and never emits part of a possible match; output is identical
  however the stream is chunked.
- Each line of a multi-line value (for example a PEM key) is also redacted on its own.
- Bounds: values shorter than 4 bytes are not redacted, at most 512 patterns.

Limits, stated plainly: redaction matches exact bytes. It cannot catch a value the
running code transforms (base64, hex, URL-encoding, splitting), values shorter than
4 bytes, or anything sent somewhere other than the log. Code that legitimately
receives a credential can always exfiltrate it. Redaction protects logs; it does not
protect the credential.

## Guidance for owners

- Use **preview-only credentials**: sandbox API keys, test databases, scoped tokens.
  Previews run pull-request code; never give them production credentials.
- Prefer credentials that can be revoked quickly and that limit what a preview can
  reach.
- Do not put sensitive values in `.previewrc.yaml`: anyone who can read the
  repository can read it.

## Code and tests

| Path | Role |
| --- | --- |
| `internal/secrets/crypto.go` | Keyring parsing, sealing and opening. |
| `internal/secrets/service.go`, `repository.go`, `queries.sql` | The `.env` file and its secrets: whole-file saves, removal confirmation, revisions, limits, owner-scoped SQL. |
| `internal/secrets/dotenv.go` | Reading `.env` text, the masked view and writing one line. |
| `internal/secrets/handler.go`, `web/pages/secrets` | Secrets tab: masked file, editor, size, build access, removal confirmation. |
| `internal/secrets/runtime.go` | Trust rule, collision rule, `ContainerEnv` for the worker. |
| `internal/secrets/redact.go` | Streaming redaction. |
| `internal/secrets/rotation.go`, `cmd/secrets-reencrypt` | Re-encryption. |

```sh
go test -count=1 -race ./internal/secrets            # crypto, redaction, provider
TEST_DATABASE_URL=... go test -count=1 ./internal/secrets   # plus HTTP and database
```

Covered: round trips; every tampered byte; values moved across repository, name or
version; unknown versions; keyring errors without key material; rotation (including
a concurrent edit and a removed old key); the owner HTTP flow with real session and
CSRF middleware; CSRF rejection; cross-owner isolation; revoked repositories
(delete-only); the 100-secret limit; database constraints; an unconfigured server;
fork, deleted-head and unverified withholding with no decryption; collision
precedence; fail-closed decryption; redaction at every split point, random chunking,
overlaps and multi-line values; `.env` parsing, masking and line writing, with
errors that never repeat a value; whole-file saves that are all or nothing, the
masked view never containing a value, CRLF normalization, stale-revision refusal,
confirmed removal, blank names, build access kept through changes, files made from
older secrets, the size and count limits, revoked and other owners' repositories,
and rotation of files beside values. The browser suite covers the masked view,
Show values and edit, the live size, the removal modal, loading a file, a refused
line and the whole flow without JavaScript.
