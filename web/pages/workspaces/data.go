package workspaces

import (
	"fmt"
	"net/url"
	"strconv"
)

type Workspace struct {
	ID, Revision                    int64
	Name, Role                      string
	Description, OwnerName, Created string
	ProfileRevision, LogoVersion    int64
}
type Member struct {
	ID, Revision        int64
	Login, Avatar, Role string
	Repositories        []int64
}
type Repository struct {
	ID        int64
	Name      string
	Available bool
}
type Invitation struct {
	ID, Revision         int64
	Login, Role, Expires string
	Expired              bool
}
type Transfer struct {
	TargetID, Revision int64
	Login, Expires     string
	Accept             bool
}
type Page struct {
	Workspace
	ActorID, PageNumber, PageCount, MemberCount, RepositoryCount, InvitationCount int64
	Tab, CSRF, Message, InvitationURL, Error, FormName, FormDescription           string
	Search                                                                        string
	InvitationView                                                                bool
	Workspaces                                                                    []Workspace
	Members                                                                       []Member
	Admins                                                                        []Member
	Repositories                                                                  []Repository
	Invitations                                                                   []Invitation
	Transfer                                                                      *Transfer
}

func (p Page) Base() string { return "/workspaces/" + strconv.FormatInt(p.ID, 10) }
func (p Page) Manage() bool { return p.Role == "owner" || p.Role == "admin" }
func (p Page) Owner() bool  { return p.Role == "owner" }
func ID(id int64) string    { return strconv.FormatInt(id, 10) }
func HasRepository(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
func MemberForm(id int64) string { return fmt.Sprintf("member-%d", id) }

func (p Page) PageURL(page int64) string {
	q := url.Values{"page": {ID(page)}}
	if p.Search != "" {
		q.Set("q", p.Search)
	}
	if p.InvitationView {
		q.Set("view", "invitations")
	}
	return p.Base() + "/members?" + q.Encode()
}
func CountLabel(count int64, singular, plural string) string {
	if count == 1 {
		return "1 " + singular
	}
	return ID(count) + " " + plural
}
func (p Page) RepositoryName(id int64) string {
	for _, r := range p.Repositories {
		if r.ID == id {
			return r.Name
		}
	}
	return ""
}
