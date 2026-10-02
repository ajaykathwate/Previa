package components

// NavItem identifies the selected navigation item independently of page titles.
type NavItem string

const (
	NavNone     NavItem = ""
	NavOverview NavItem = "overview"
)
