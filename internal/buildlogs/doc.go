// Package buildlogs stores and streams the output of each pull request's current
// build attempt.
//
// Build admission (internal/environments) opens an attempt's stream by bumping
// its epoch and clearing earlier lines in one transaction. A Writer bound to
// that epoch strips control sequences, redacts known values, bounds lines and the
// attempt's total size, and appends in background batches that a newer attempt
// fences off. Owners read the log on a page with paging and a Server-Sent Events
// stream whose "<epoch>.<next>" cursors resume without loss and reset when a new
// attempt replaces the logs. A periodic job deletes logs after Retention.
package buildlogs
