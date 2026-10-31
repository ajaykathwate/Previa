package usage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/publicid"
	pages "github.com/ajaykathwatee/previa/web/pages/usage"
	"github.com/go-chi/chi/v5"
)

type Users interface {
	UserIDs(context.Context) (int64, int64, bool)
	Username(context.Context) (string, bool)
	CSRFToken(context.Context) string
}
type Handler struct {
	store  *Store
	users  Users
	logger *slog.Logger
}

func NewHandler(s *Store, u Users, l *slog.Logger) *Handler { return &Handler{s, u, l} }
func (h *Handler) Mount(r chi.Router, require func(http.Handler) http.Handler) {
	r.With(require).Get("/workspaces/{workspaceID}/usage", h.page)
}
func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	user, _, ok := h.users.UserIDs(r.Context())
	workspace, err := publicid.Param(r, "workspaceID", "workspace")
	if !ok || err != nil || workspace <= 0 {
		http.NotFound(w, r)
		return
	}
	o, err := h.store.Overview(r.Context(), user, workspace)
	if errors.Is(err, ErrUnavailable) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.logger.ErrorContext(r.Context(), "read workspace usage", "error", err)
		http.Error(w, "Usage is temporarily unavailable. Please try again.", http.StatusServiceUnavailable)
		return
	}
	at := func(t time.Time) string {
		if t.IsZero() || t.Equal(time.Unix(0, 0)) {
			return ""
		}
		return t.UTC().Format(time.RFC3339)
	}
	p := pages.Page{WorkspaceID: workspace, Name: o.Name, Manager: o.Manager, CSRF: h.users.CSRFToken(r.Context()), PeriodStart: at(o.Start), PeriodEnd: at(o.End), Measured: at(o.Measured), Cutover: at(o.Cutover), OperatorChecked: at(o.OperatorChecked),
		Included: pages.Minutes(o.Included * 1000000), Consumed: pages.Minutes(o.Consumed), Reserved: pages.Minutes(o.Reserved), Builds: o.Builds, BuildLimit: o.BuildLimit, Slots: o.Slots, SlotLimit: o.SlotLimit, Pending: o.Pending, Provisional: o.Provisional}
	p.OperatorKnown = !o.OperatorChecked.IsZero()
	p.PreviewExpiry = policyDuration(o.PreviewLifetime, "No automatic expiry configured")
	p.IdleSleep = policyDuration(o.PreviewIdle, "Idle sleep is off")
	p.LogRetention = policyDuration(o.LogRetention, "Unknown")
	if o.Daily > 0 {
		p.Daily = pages.Minutes(o.Daily * 1000000)
	}
	for _, v := range o.Repositories {
		p.Repositories = append(p.Repositories, pages.Repository{ID: v.ID, Name: v.Name, Consumed: pages.Minutes(v.Consumed), Reserved: pages.Minutes(v.Reserved), Attempts: v.Attempts, Provisional: v.Provisional})
	}
	for _, v := range o.Blocked {
		reason, action := pages.Explanation(v.Reason)
		label := v.Kind
		if v.Number > 0 {
			label += " #" + strconv.Itoa(v.Number)
		}
		p.Blocked = append(p.Blocked, pages.Work{RepoID: v.RepoID, Repository: v.Repository, Label: label, Reason: reason, Action: action, Until: at(v.Until)})
	}
	username, _ := h.users.Username(r.Context())
	w.Header().Set("Cache-Control", "no-store")
	publicid.Handler(r, pages.Index(username, p), p).ServeHTTP(w, r)
}

func policyDuration(seconds *int64, zero string) string {
	if seconds == nil {
		return "Unknown until an execution worker reports its policy"
	}
	if *seconds == 0 {
		return zero
	}
	if *seconds%86400 == 0 {
		return fmt.Sprintf("%d days", *seconds/86400)
	}
	if *seconds%3600 == 0 {
		return fmt.Sprintf("%d hours", *seconds/3600)
	}
	return fmt.Sprintf("%d minutes", *seconds/60)
}
