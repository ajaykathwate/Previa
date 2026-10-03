package dashboard

// Summary contains database-derived workspace counts, scoped to the signed-in user.
type Summary struct{ Installations, Repositories, Available int64 }
