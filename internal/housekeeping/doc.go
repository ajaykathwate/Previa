// Package housekeeping keeps the real world matching the database
// (docs/15-housekeeping-and-capacity.md): bounded startup and periodic River
// jobs recover lost wake-ups and stuck work, expire previews, confirm live
// pull requests with GitHub, remove orphaned Docker objects after a grace
// period and prune caches, images and workspaces within budgets. Every
// deletion it decides is logged, and a dry run only logs. Lifecycle changes
// go through the environments package; housekeeping never edits lifecycle
// state itself.
package housekeeping
