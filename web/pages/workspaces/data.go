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
	DeactivatedCount, InstallationCount                                           int64
	Tab, CSRF, Message, InvitationURL, Error, FormName, FormDescription           string
	ErrorField                                                                    string // "logo" or "details" places a settings error next to its field
	Search                                                                        string
	InvitationView, DeactivatedView                                               bool
	Workspaces                                                                    []Workspace
	Members                                                                       []Member
	Admins                                                                        []Member
	Repositories                                                                  []Repository
	Invitations                                                                   []Invitation
	Deactivated                                                                   []DeactivatedMember
	Received                                                                      []ReceivedInvitation
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
	if p.DeactivatedView {
		q.Set("view", "deactivated")
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
	Failed, Reinvite                   bool // Reinvite prefills a deactivated member's previous access
}

// DeactivatedMember was removed or left; they have no access until they accept
// a new invitation, which can offer their previous role and repositories.
type DeactivatedMember struct {
	Member
	RemovedAt, RemovedBy string
	Left, Invited        bool
}

// ReceivedInvitation is a pending invitation addressed to the signed-in person.
type ReceivedInvitation struct {
	ID                                int64
	Workspace, Role, Inviter, Expires string
	Repositories                      []string
}

// MemberDetail is a member's profile as seen by a teammate in one workspace.
type MemberDetail struct {
	Page
	Member                                                         Member
	Active, Left                                                   bool
	Joined, Removed, RemovedBy, ProfileURL, Company, Location, Bio string
	Previous                                                       []int64
	Activity                                                       []components.ActivityItem
}

func (d MemberDetail) PublicReferences() []components.ResourceRef {
	refs := append(d.Page.PublicReferences(), components.ResourceRef{Kind: "user", ID: d.Member.ID})
	return append(refs, components.ActivityRefs(d.Activity)...)
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
	for _, m := range p.Deactivated {
		refs = append(refs, components.ResourceRef{Kind: "user", ID: m.ID})
	}
	for _, v := range p.Received {
		refs = append(refs, components.ResourceRef{Kind: "invitation", ID: v.ID})
	}
	return refs
}

// DeleteConfirmation is what an owner types, in capitals, to delete a workspace.
const DeleteConfirmation = "DELETE"

// CanManageMember reports whether the viewer may change or remove an active
// member: never the owner, and admins only by the owner.
func (p Page) CanManageMember(m Member) bool {
	return m.Role != "owner" && (p.Owner() || p.Manage() && m.Role != "admin")
}

// SetupDone counts the finished steps of a new workspace's checklist.
func (p Page) SetupDone() int64 {
	done := int64(1)
	for _, ok := range []bool{p.MemberCount > 1 || p.InvitationCount > 0, p.RepositoryCount > 0 || p.InstallationCount > 0, p.Description != "" || p.LogoVersion > 0} {
		if ok {
			done++
		}
	}
	return done
}

// JoinNames lists repository names compactly, naming at most three.
func JoinNames(names []string) string {
	if len(names) <= 3 {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:3], ", ") + " and " + CountLabel(int64(len(names)-3), "more", "more")
}

// displayURL drops the scheme from a profile link for a compact label.
func displayURL(raw string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://"), "/")
}
