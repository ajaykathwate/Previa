package idle

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/idle/dbgen"
	idlepages "github.com/ajaykathwatee/previa/web/pages/idle"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// Enqueuer starts wakes on a worker; the server's River producer implements it.
type Enqueuer interface {
	Insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

const (
	// settle is how long after waking the visitor still waits, so the
	// router has loaded the preview's route again.
	settle = 3 * time.Second
	// rewake asks again when a wake was requested this long ago and has
	// not finished (its worker may have stopped).
	rewake = 2 * time.Minute
)

var labelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// WakePage is where the router sends a sleeping preview's visitors
// (routing.WakePath + "/<host>/<path>"). It is public, like the preview,
// and reads and sets no cookie. It wakes the preview and, once it runs,
// sends the visitor back to the address they opened: only ever a known
// preview's host, so it is never an open redirect.
type WakePage struct {
	pool       *pgxpool.Pool
	jobs       Enqueuer
	baseDomain string
	unknown    http.HandlerFunc
	logger     *slog.Logger
	now        func() time.Time
}

// NewWakePage serves wakes for previews under baseDomain; unknown answers
// anything else (the unavailable page).
func NewWakePage(pool *pgxpool.Pool, jobs Enqueuer, baseDomain string, unknown http.HandlerFunc, logger *slog.Logger) *WakePage {
	return &WakePage{pool: pool, jobs: jobs, baseDomain: baseDomain, unknown: unknown, logger: logger, now: time.Now}
}

func (p *WakePage) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, path, _ := strings.Cut(chi.URLParam(r, "*"), "/")
	label, ok := strings.CutSuffix(strings.ToLower(host), "."+p.baseDomain)
	if p.baseDomain == "" || !ok || !labelPattern.MatchString(label) {
		p.unknown(w, r)
		return
	}
	q := dbgen.New(p.pool)
	row, err := q.PreviewByHost(r.Context(), label)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (row.PrState != "open" || row.Status != "running")) {
		p.unknown(w, r)
		return
	}
	if err != nil {
		p.logger.ErrorContext(r.Context(), "wake page failed", "error", err)
		http.Error(w, "The preview could not be woken right now. Reload in a moment.", http.StatusServiceUnavailable)
		return
	}
	target := "https://" + label + "." + p.baseDomain + "/" + path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	header := w.Header()
	header.Set("Cache-Control", "no-store")
	header.Set("X-Robots-Tag", "noindex")
	header.Set("Referrer-Policy", "no-referrer")
	switch {
	case !row.AsleepAt.Valid && row.WokeAt.Valid && p.now().Sub(row.WokeAt.Time) < settle:
		p.waiting(w, r, label, 2)
	case !row.AsleepAt.Valid:
		http.Redirect(w, r, target, http.StatusFound)
	case row.WakeError != "" && r.Method != http.MethodPost:
		header.Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = idlepages.WakeFailed(label+"."+p.baseDomain, row.WakeError).Render(r.Context(), w)
	default:
		if row.WakeError != "" || !row.WakeRequestedAt.Valid || p.now().Sub(row.WakeRequestedAt.Time) > rewake {
			if err := p.request(r.Context(), row.ID); err != nil {
				p.logger.ErrorContext(r.Context(), "wake not requested", "environment_id", row.ID, "error", err)
				http.Error(w, "The preview could not be woken right now. Reload in a moment.", http.StatusServiceUnavailable)
				return
			}
		}
		if r.Method == http.MethodPost {
			http.Redirect(w, r, r.URL.RequestURI(), http.StatusSeeOther)
			return
		}
		p.waiting(w, r, label, 3)
	}
}

func (p *WakePage) request(ctx context.Context, environmentID int64) error {
	if _, err := dbgen.New(p.pool).RequestWake(ctx, environmentID); err != nil {
		return err
	}
	args := WakeArgs{EnvironmentID: environmentID}
	opts := args.InsertOpts()
	_, err := p.jobs.Insert(ctx, args, &opts)
	return err
}

// waiting shows the wake page and reloads it after seconds.
func (p *WakePage) waiting(w http.ResponseWriter, r *http.Request, label string, seconds int) {
	w.Header().Set("Refresh", strconv.Itoa(seconds))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	_ = idlepages.Waking(label+"."+p.baseDomain).Render(r.Context(), w)
}
