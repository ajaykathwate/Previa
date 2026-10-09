# 11 · Secrets and outbound connections

**Module:** SEC · **Code:** `internal/secrets`, `internal/outbound`,
`internal/platform/egress` (forwarder) · **Feature docs:**
[07 · Repository secrets](../docs/07-repository-secrets.md),
[11 · Backend previews](../docs/11-backend-previews.md)

Repository secrets (write-only, encrypted, never shown again) and owner-approved
outbound connections from previews to external databases and brokers.

| Sub-module | Prefix | Cases |
| --- | --- | --- |
| Secrets editor | TC-SEC-UI | 4 |
| Encryption and redaction | TC-SEC-CRY | 3 |
| Outbound approvals | TC-SEC-OUT | 3 |
| Outbound forwarding | TC-SEC-FWD | 2 |

## 1. Secrets editor (TC-SEC-UI)

- [x] **TC-SEC-UI-001 · Owners save secrets that are never shown again**  
  Type: Positive / Security · Priority: P1 · Level: Integration (PostgreSQL) + Browser  
  Steps: Save a secret; reload; replace it; save several at once; delete selected ones in a modal.  
  Expected: The page lists names, never values; the stored value is encrypted under the active key and decrypts back; replacing does not reveal the old value; bulk save and modal deletion work.  
  Evidence: `internal/secrets` › `TestSecretsHTTPOwnerFlowCSRFAndIsolation`; browser › *repository secrets editor saves several at once, never shows values, and deletes selected ones*

- [x] **TC-SEC-UI-002 · Secrets are owner-only and CSRF-protected**  
  Type: Security · Priority: P1 · Level: Integration (PostgreSQL)  
  Steps: Open, write and delete another owner's secrets; post without CSRF; write to a revoked repository.  
  Expected: Each is refused; nothing changes.  
  Evidence: `internal/secrets` › `TestSecretsHTTPOwnerFlowCSRFAndIsolation`

- [x] **TC-SEC-UI-003 · Invalid input is refused all-or-nothing, without echoing values**  
  Type: Negative · Priority: P1 · Level: Integration + Unit  
  Steps: Save invalid names, an empty value, and a batch where one line is bad; paste `.env` text with errors.  
  Expected: Nothing in the batch is saved; errors point at the line or name and never repeat a value.  
  Evidence: `internal/secrets` › `TestSecretsHTTPOwnerFlowCSRFAndIsolation`; `internal/secrets` › `TestSecretsBulkSaveDotenvAndBulkDelete`; `internal/secrets` › `TestParseDotenvErrorsNameTheLineNotTheValue`; `internal/secrets` › `TestParseDotenv`

- [x] **TC-SEC-UI-004 · Limits, constraints and an unconfigured server**  
  Type: Edge · Priority: P2 · Level: Integration + Browser  
  Steps: Save past the per-repository limit; replace at the limit; insert invalid rows directly; run without a keyring; use the editor without JavaScript.  
  Expected: The limit holds and replacing at it works; the database refuses bad names and too-short ciphertext; without a keyring the page explains itself and still lists names; the editor works without JavaScript.  
  Evidence: `internal/secrets` › `TestSecretsLimitConstraintsAndUnconfiguredServer`; browser › *the secrets editor works without JavaScript and pasted .env text is saved*

## 2. Encryption and redaction (TC-SEC-CRY)

- [x] **TC-SEC-CRY-001 · Values are sealed, bound to their row and tamper-evident**  
  Type: Security · Priority: P1 · Level: Unit  
  Steps: Seal and open values; reuse a sealed value for another repository or name; flip any byte; truncate.  
  Expected: Ciphertext never contains the plaintext; nonces differ; a value opened in the wrong place, tampered or truncated fails.  
  Evidence: `internal/secrets` › `TestKeyringSealOpenRoundTripAndBinding`

- [x] **TC-SEC-CRY-002 · Keys rotate and re-encrypt safely; key errors never echo keys**  
  Type: Security · Priority: P1 · Level: Unit + Integration  
  Steps: Add a key and switch the active version; re-encrypt twice and during an edit; remove an old key too early; configure malformed keyrings.  
  Expected: Old values stay readable; new ones use the active key; re-encryption is idempotent and loses no concurrent edit; a missing key stops it unchanged; configuration errors never contain key material.  
  Evidence: `internal/secrets` › `TestKeyringRotationKeepsOldValuesReadable`; `internal/secrets` › `TestReencryptMovesValuesToTheActiveKey`; `internal/secrets` › `TestParseKeyringRejectsBadConfigurationWithoutEchoingKeys`

- [x] **TC-SEC-CRY-003 · Secret values never reach logs, even split across writes**  
  Type: Security · Priority: P1 · Level: Unit  
  Steps: Write output containing secrets split at every byte boundary, in random chunks, overlapping and multi-line.  
  Expected: Every occurrence is redacted; safe output is released promptly; only a possible start of a value is held back; patterns are bounded.  
  Evidence: `internal/secrets` › `TestRedactingWriterCatchesValuesSplitAcrossWrites`; `internal/secrets` › `TestRedactorHandlesOverlapsLinesAndBounds`; `internal/secrets` › `TestRedactingWriterHoldsBackOnlyAPossibleMatch`; `internal/secrets` › `TestRedactingWriterEmitsEverythingThatCannotStartAValue`

## 3. Outbound approvals (TC-SEC-OUT)

- [x] **TC-SEC-OUT-001 · Owners approve public host:port destinations and remove them in a modal**  
  Type: Positive · Priority: P1 · Level: Integration + Browser  
  Steps: Approve `db.example.com:5432`; try an IP address; remove it after confirming.  
  Expected: The IP is refused next to the field; the name is approved and its port no longer offered; removal needs the modal; the worker's refresh wakes on each change.  
  Evidence: `internal/outbound` › `TestApprovalsAreOwnerScopedBoundedAndWakeTheWorker`; browser › *owners approve outbound destinations and remove them only after confirming in a modal*

- [x] **TC-SEC-OUT-002 · Only public names on allowed ports, within limits**  
  Type: Security · Priority: P1 · Level: Unit + Integration  
  Steps: Approve IP literals, `localhost`, internal-looking names, disallowed ports, a second destination on a used port, more than the limit; turn the feature off.  
  Expected: Each is refused; the database itself refuses IP addresses; with the feature off nothing is accepted.  
  Evidence: `internal/outbound` › `TestParseAcceptsOnlyPublicNamesOnAllowedPorts`; `internal/outbound` › `TestApprovalsAreOwnerScopedBoundedAndWakeTheWorker`

- [x] **TC-SEC-OUT-003 · The Outbound tab is owner-only**  
  Type: Security · Priority: P1 · Level: Integration  
  Steps: As a stranger open, approve and remove on another owner's tab; approve after access is removed.  
  Expected: Every request is refused and changes nothing.  
  Evidence: `internal/outbound` › `TestOutboundTabIsOwnerOnly`; `internal/outbound` › `TestApprovalsAreOwnerScopedBoundedAndWakeTheWorker`

## 4. Outbound forwarding (TC-SEC-FWD)

- [x] **TC-SEC-FWD-001 · The forwarder relays only approved, public destinations**  
  Type: Security · Priority: P1 · Level: Unit  
  Steps: Connect from a preview network to unapproved, private, loopback, metadata and unresolvable destinations; rebind a name to a private address between lookups; exceed the connection and byte caps; withdraw an approval mid-connection.  
  Expected: Only approved public destinations are relayed, dialling the exact address it vetted; DNS rebinding fails; caps and withdrawals close connections; counters are written; stale networks lose their approvals.  
  Evidence: `internal/platform/egress` › `TestForwarderRelaysOnlyApprovedPublicDestinations`; `internal/platform/egress` › `TestForwarderDialsTheAddressItVetted`; `internal/platform/egress` › `TestForwarderBoundsConnections`; `internal/platform/egress` › `TestForwarderServesTheRecordedPortsAndWritesCounters`; `internal/platform/egress` › `TestSyncAllowFilesKeepsExactlyTheLiveNetworks`

- [x] **TC-SEC-FWD-002 · Approved connections work end to end; forks get none**  
  Type: Positive / Security · Priority: P1 · Level: End-to-end (real Docker)  
  Steps: From a backend preview connect to the approved destination, an unapproved one, a loopback name and a metadata name; repeat from a fork's preview.  
  Expected: The approved one answers through the egress proxy; the rest are refused and counted; the fork reaches nothing and its approvals are recorded as withheld.  
  Evidence: `internal/app` › `TestRealBackendEndToEnd`
