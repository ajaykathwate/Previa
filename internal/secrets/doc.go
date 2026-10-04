// Package secrets stores repository secrets for previewed apps and prepares them
// for exactly one container start.
//
// Owners manage values on a repository's Secrets tab; values are encrypted with
// AES-256-GCM under versioned keys (Keyring), bound to repository, name and key
// version, and never shown again. At container creation the worker asks Provider
// for the environment: fork or unverified sources receive no secrets, and a stored
// secret overrides a same-named .previewrc.yaml entry. Redactor removes known
// values from streamed logs. Reencrypt moves values onto a new active key.
package secrets
