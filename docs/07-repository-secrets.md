# 07 · Repository secrets

Owners give previews the configuration they need at runtime (API keys, database
URLs) without putting it in the repository, without exposing it to fork authors and
without leaking it into logs. This document covers how values are stored, who
receives them, how keys rotate and what redaction can and cannot do. Containers
start in the next phases ([14-roadmap.md](14-roadmap.md)); the worker contract
below is what they will call.

## Decisions

| Decision | Choice | Why |
| --- | --- | --- |
| Where owners manage secrets | A **Secrets** tab on each repository (`/repositories/{id}/secrets`) | Keeps the PR list focused; both tabs are ordinary pages, so they work without JavaScript. |
| Encryption | Standard-library AES-256-GCM, random 96-bit nonce per value | Authenticated encryption with no extra dependency. |
| Key provisioning | `SECRETS_ENCRYPTION_KEYS` + `SECRETS_ACTIVE_KEY_VERSION` environment variables | Matches how `SESSION_SECRET` is provided; versioned for rotation. |
| Name collision with `.previewrc.yaml` env | **The stored secret wins**, and the override is reported by name | The owner's value must beat contributor-editable YAML. |
| Fork PRs | **Never** receive secrets: any mode, any retry, any approval | The fork's author controls the code that would receive them. |
| When values are decrypted | In worker memory, immediately before container creation | Shortest possible plaintext lifetime; never in images or build inputs. |
| Maintainer approval for forks | **Not** part of the MVP | Optional approval would not change the rule above, so it adds risk without benefit now. |

## Storage format

`env_secrets` (migration 12) stores one row per repository and name:

- `encrypted_value` = 12-byte random nonce ‖ AES-256-GCM ciphertext ‖ 16-byte tag.
- The repository ID, the name and the key version are **authenticated associated
  data**. A value copied to another repository or name, or relabelled with another
  version, fails to decrypt.
- `key_version` records which key sealed it; `updated_at` is shown to owners.
- Database checks enforce the name format and a ciphertext size consistent with
  1 byte – 32 KiB of plaintext, even if application code regressed.

Rules enforced by `secrets.Service`:

| Rule | Limit |
| --- | --- |
| Name | `[A-Za-z_][A-Za-z0-9_]*`, up to 128 characters, case-sensitive |
| Reserved | names starting with `PREVIA_` (kept for platform-injected variables) |
| Value | 1 byte to 32 KiB, no NUL bytes; textarea line breaks are stored as `\n` |
| Count | 100 secrets per repository (enforced under a repository row lock) |
| Who | only the installation owner; adding and replacing also need current GitHub access, deleting does not |

Values are write-only: after saving, pages list names and update times only.
Values never appear in pages (not even in a rejected form), jobs, logs, metrics or
errors.

## Keys: provisioning and rotation

```dotenv
SECRETS_ENCRYPTION_KEYS=1:<base64 of 32 random bytes>,2:<…>
SECRETS_ACTIVE_KEY_VERSION=2
```

- Generate each key with `openssl rand -base64 32`. Versions are positive integers.
- **Server**: needs the keyring to encrypt new values. Without it, the Secrets tab
  explains that secrets are not configured and existing names stay visible.
- **Worker**: will need the same keyring to decrypt at container start once the
  runtime exists (Prompt 5). Today it does not read these variables.
- Startup fails with a message that never echoes key material if the keyring is
  malformed: bad base64, wrong length, duplicate or missing versions.

Rotation, with no downtime:

1. Add a new version to `SECRETS_ENCRYPTION_KEYS` everywhere (keep the old one).
2. Set `SECRETS_ACTIVE_KEY_VERSION` to the new version; restart server and worker.
   New and replaced values now use the new key; old values still decrypt.
3. Run `make secrets-reencrypt`. It moves every value onto the active key in batches,
   is safe while the server takes edits (a concurrent edit wins) and is safe to
   repeat. It prints counts only.
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
- For trusted sources, every value is decrypted; any value that fails (wrong key,
  tampering, unknown version) fails the whole call, so a container never starts with
  a partial environment. Secrets override YAML entries of the same name.
- The result is a `ContainerEnv`. Its `Environ()` goes only to the runtime's
  container configuration. It must never become Docker build arguments, part of the
  build context or an image layer: secrets are runtime-only. `Wipe()` clears the
  decrypted bytes afterwards (Go string copies handed to the runtime cannot be wiped,
  so keep their lifetime short).

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
| `internal/secrets/service.go`, `repository.go`, `queries.sql` | Owner management, limits, owner-scoped SQL. |
| `internal/secrets/handler.go`, `web/pages/secrets` | Secrets tab, CSRF-protected forms, delete confirmation modal. |
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
overlaps and multi-line values. The browser suite covers the tab, saving, replacing
and modal deletion.
