package workspaces

import (
	"fmt"
	"strconv"
)

type Workspace struct {
	ID, Revision int64
	Name, Role   string
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
	ActorID, PageNumber, PageCount, MemberCount int64
	Tab, CSRF, Message, InvitationURL           string
	Workspaces                                  []Workspace
	Members                                     []Member
	Admins                                      []Member
	Repositories                                []Repository
	Invitations                                 []Invitation
	Transfer                                    *Transfer
	Previews                                    []Preview
}
type Preview struct {
	RepoID                                                    int64
	Number                                                    int32
	Repository, Title, Latest, Deployed, URL, Status, Expires string
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

func (p Page) PageURL(page int64) string { return p.Base() + "/" + p.Tab + "?page=" + ID(page) }
