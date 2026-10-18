// Package errortracking sends the server's and the worker's warnings and
// errors to Sentry (docs/25-production-deployment.md#monitoring-with-sentry).
// Every error-level log record becomes a Sentry issue, so a new kind of
// failure reaches the operator by email; warnings and errors are also kept as
// searchable Sentry logs. Records arrive already redacted by the logging layer,
// and a preview app's own output never passes through here. Reporting is off
// unless SENTRY_DSN is set.
package errortracking
