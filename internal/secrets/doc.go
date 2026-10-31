// Package secrets stores repository secrets for previewed apps and prepares them
// for exactly one container start.
//
// Owners and Admins keep one .env file per repository on its Secrets tab. The
// file and each value it sets are encrypted with AES-256-GCM under versioned keys
// (Keyring), bound to repository, name and key version; values are masked until
// the file is opened to edit. At container creation the worker asks Provider
// for the environment: fork or unverified sources receive no secrets, and a stored
// secret overrides a same-named .previewrc.yaml entry. Redactor removes known
// values from streamed logs. Reencrypt moves values and files onto a new active
// key.
package secrets
