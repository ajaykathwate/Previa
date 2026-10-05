// Package metrics holds Previa's Prometheus registry and its bounded-cardinality
// metrics: ingress, webhooks, jobs, builds, housekeeping, GitHub rate limits and
// scrape-time state read from PostgreSQL. It is served on the internal listener only.
package metrics
