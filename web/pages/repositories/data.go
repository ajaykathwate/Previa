package repositories

import (
	"net/url"
	"strconv"
)

type Row struct {
	FullName, Account, Status string
	InstallationID            int64
	Accessible                bool
}
type Page struct {
	Rows                                            []Row
	Total, Number, Pages, Start, End, Installations int64
	Search, Access                                  string
	Enabled, Connected                              bool
}

func PageURL(number int64, search, access string) string {
	values := url.Values{"page": {strconv.FormatInt(number, 10)}}
	if search != "" {
		values.Set("q", search)
	}
	if access != "" && access != "all" {
		values.Set("access", access)
	}
	return "/repositories?" + values.Encode()
}
func PageNumbers(current, pages int64) []int64 {
	start := max(int64(1), current-2)
	end := min(pages, start+4)
	start = max(int64(1), end-4)
	result := make([]int64, 0, 5)
	for number := start; number <= end; number++ {
		result = append(result, number)
	}
	return result
}
