package components

// NavItem identifies the selected navigation item independently of page titles.
type NavItem string

const (
	NavNone         NavItem = ""
	NavOverview     NavItem = "overview"
	NavRepositories NavItem = "repositories"
	NavEnvironments NavItem = "environments"
	NavBuilds       NavItem = "builds"
	NavTools        NavItem = "tools"
	NavFAQ          NavItem = "faq"
)

// Crumb is the previous level in the header's trail: the page one step up.
// An empty Href shows it as plain text (the workspace root).
type Crumb struct {
	Label, Href string
}

// WorkspaceCrumb is the trail's root for top-level workspace pages.
var WorkspaceCrumb = Crumb{Label: "Workspace"}
