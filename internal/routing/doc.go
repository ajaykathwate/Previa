// Package routing gives each pull request a stable public HTTPS hostname and
// publishes healthy candidates through Traefik. A route controller accepts
// route versions behind a durable (generation, revision) fence in PostgreSQL,
// materializes the whole accepted route table into one file Traefik watches
// (a file that only ever moves forward), and confirms through Traefik's
// loopback-only API that a version is served before older artifacts retire.
package routing
