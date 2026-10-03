package components

// NavItem identifies the selected navigation item independently of page titles.
type NavItem string

const (
	NavNone     NavItem = ""
	NavOverview NavItem = "overview"
 NavRepositories NavItem="repositories"
 NavEnvironments NavItem="environments"
 NavBuilds NavItem="builds"
 NavTools NavItem="tools"
)
