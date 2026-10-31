// Package reviews owns native preview review (README-MVP-PRODUCT-EXPANSION.md,
// Feature 3): a pull request's testing instructions, written in Previa by
// people who can build it, and its findings: threads that Reviewers,
// Developers and Admins start, reply to, edit, remove, moderate, resolve and
// reopen inside Previa.
//
// A finding records the deployment (publication) its author had in front of
// them and is never relabelled when a newer one goes live; a draft written
// against an older deployment asks which one it belongs to. Writes compare
// revisions, deduplicate repeated submissions, lock in a fixed order (pull
// request, the writer's membership, the pull request's counters), enforce
// bounded content and record a timeline event in the same transaction. Only
// a link reaches GitHub; review text, status and counts stay in Previa.
package reviews
