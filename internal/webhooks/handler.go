package webhooks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/go-github/v91/github"
)

const MaxBodyBytes = 2 << 20

var deliveryPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var ErrInvalidEvent = errors.New("invalid webhook event")
var ErrDeliveryConflict = errors.New("delivery ID reused with different contents")

type deliveryProcessor interface {
	Process(context.Context, string, string, string, any) error
}
type Handler struct {
	secret    []byte
	processor deliveryProcessor
	logger    *slog.Logger
}

func NewHandler(secret string, processor deliveryProcessor, logger *slog.Logger) *Handler {
	return &Handler{[]byte(secret), processor, logger}
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if len(h.secret) == 0 {
		http.Error(w, "Webhook authentication is not configured.", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			http.Error(w, "Webhook body is too large.", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "Could not read webhook body.", http.StatusBadRequest)
		}
		return
	}
	signature := r.Header.Get("X-Hub-Signature-256")
	if !strings.HasPrefix(signature, "sha256=") || github.ValidateSignature(signature, body, h.secret) != nil {
		http.Error(w, "Invalid webhook signature.", http.StatusUnauthorized)
		return
	}
	id, kind := r.Header.Get("X-GitHub-Delivery"), r.Header.Get("X-GitHub-Event")
	if !deliveryPattern.MatchString(id) || kind == "" || len(kind) > 100 || !json.Valid(body) {
		http.Error(w, "Invalid webhook envelope.", http.StatusBadRequest)
		return
	}
	if kind != "pull_request" && kind != "installation" && kind != "installation_repositories" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	event, err := github.ParseWebHook(kind, body)
	if err != nil {
		http.Error(w, "Invalid webhook payload.", http.StatusBadRequest)
		return
	}
	attrs := []any{"delivery_id", id, "event", kind}
	attrs = append(attrs, eventIdentity(event)...)
	sum := sha256.Sum256(body)
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	err = h.processor.Process(ctx, id, kind, hex.EncodeToString(sum[:]), event)
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, ErrInvalidEvent) {
			status = http.StatusBadRequest
		}
		if errors.Is(err, ErrDeliveryConflict) {
			status = http.StatusConflict
		}
		h.logger.WarnContext(ctx, "webhook not accepted", append(attrs, "status", status)...)
		http.Error(w, "Webhook could not be accepted. Retry transient failures.", status)
		return
	}
	h.logger.InfoContext(ctx, "webhook accepted", attrs...)
	w.WriteHeader(http.StatusNoContent)
}

// Only numeric provider identities are logged; no private payload text is used.
func eventIdentity(event any) []any {
	switch e := event.(type) {
	case *github.PullRequestEvent:
		return []any{"github_repo_id", e.GetRepo().GetID(), "pr_number", e.GetPullRequest().GetNumber(), "installation_id", e.GetInstallation().GetID()}
	case *github.InstallationEvent:
		return []any{"installation_id", e.GetInstallation().GetID()}
	case *github.InstallationRepositoriesEvent:
		return []any{"installation_id", e.GetInstallation().GetID()}
	default:
		return nil
	}
}
