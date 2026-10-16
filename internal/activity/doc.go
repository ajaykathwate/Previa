// Package activity keeps the bounded record of what people and the system did
// to pull requests (README-MVP-PRODUCT-EXPANSION.md, delivery step B): events
// written in the same transaction as the change they describe, deduplicated
// so a retry records one event, and pruned after 90 days or beyond 5,000 per
// pull request with a marker that older history was removed.
//
// Events reference objects and carry small, safe metadata, never message
// bodies, secrets or app output. They are history, not state: active review
// state and unresolved findings never depend on them. Features record through
// small interfaces of their own that internal/app connects to the Recorder.
package activity
