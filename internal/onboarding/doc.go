// Package onboarding guides a repository to its first usable preview
// (README-MVP-PRODUCT-EXPANSION.md, Feature 2): a five-step checklist on the
// repository's Setup tab for one open pull request, a bounded preflight of
// that commit's configuration run by a worker, the record of who opened a
// preview from Previa, and the Overview's role-aware next steps.
//
// It composes state other features own. Connection, deployment and build
// facts are read on every view, so nothing can claim a step done after it
// stopped being true; only the setup checks and preview opens are stored. A
// check never builds, starts a preview or enables Automatic builds: building
// stays the existing, separately authorized Build action.
package onboarding
