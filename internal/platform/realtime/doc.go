// Package realtime delivers committed-change notifications to browsers.
//
// PostgreSQL triggers raise NOTIFY events on the "previa_events" channel after a
// transaction commits. One Listener per server process receives them and hands
// them to a Hub, which fans events out to subscribers by topic. HTTP handlers
// stream a subscription as Server-Sent Events with Stream.
//
// Events are invalidation signals, not data: a browser that receives one
// refetches through its ordinary, authorized HTTP handler. Every event carries a
// Cursor so a reconnecting client can resume; when the Hub can no longer prove
// that nothing was missed, it sends a resync instead. The package knows nothing
// about features; topics such as "repository:42" are defined by their owners.
package realtime
