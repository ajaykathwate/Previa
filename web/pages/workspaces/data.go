package workspaces

import (
	"fmt"
	"github.com/ajaykathwatee/previa/web/components"
	"net/url"
	"strconv"
	"strings"
)

type Workspace struct {
	ID, Revision                    int64
	Name, Role                      string
	Description, OwnerName, Created string
	ProfileRevision, LogoVersion    int64
}
type Member struct {
	ID, Revision, PhotoVersion               int64
	Login, Avatar, Role, FirstName, LastName string
	Repositories                             []int64
}

// AvatarSrc prefers the member's uploaded photo (PhotoVersion > 0) over GitHub's.
func (m Member) AvatarSrc() string {
	if m.PhotoVersion > 0 {
		return "/avatars/" + ID(m.ID) + "?v=" + ID(m.PhotoVersion)
	}
	return m.Avatar
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
	ErrorField                                                                    string // "logo" or "details" places a settings error next to its field
	Search                                                                        string
	InvitationView                                                                bool
	Workspaces                                                                    []Workspace
	Members                                                                       []Member
	Admins                                                                        []Member
	Repositories                                                                  []Repository
	Invitations                                                                   []Invitation
	Transfer                                                                      *Transfer
	Invite                                                                        InviteForm
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

type InviteForm struct {
	Login, Role, LoginError, FormError string
	Repositories                       []int64
	Failed                             bool
}

func (f InviteForm) SelectedRole() string {
	if f.Role == "admin" || f.Role == "reviewer" {
		return f.Role
	}
	return "developer"
}
func (m Member) Name() string {
	if n := strings.TrimSpace(m.FirstName + " " + m.LastName); n != "" {
		return n
	}
	return m.Login
}

func (p Page) PublicReferences() []components.ResourceRef {
	refs := components.ResourceRefs("workspace", p.ID)
	for _, w := range p.Workspaces {
		refs = append(refs, components.ResourceRef{Kind: "workspace", ID: w.ID})
	}
	for _, m := range p.Members {
		refs = append(refs, components.ResourceRef{Kind: "user", ID: m.ID})
	}
	for _, m := range p.Admins {
		refs = append(refs, components.ResourceRef{Kind: "user", ID: m.ID})
	}
	for _, repo := range p.Repositories {
		refs = append(refs, components.ResourceRef{Kind: "repository", ID: repo.ID})
	}
	for _, v := range p.Invitations {
		refs = append(refs, components.ResourceRef{Kind: "invitation", ID: v.ID})
	}
	return refs
}
