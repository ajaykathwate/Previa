package dashboard

// Installation contains display data only, with no backend or GitHub dependency.
type Installation struct {
	ID              int64
	Account, Status string
	Repositories    []Repository
}
type Repository struct {
	FullName   string
	Accessible bool
}
