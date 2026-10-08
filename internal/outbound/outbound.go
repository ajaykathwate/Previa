package outbound

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/egress"
)

// MaxDestinations bounds one repository's approvals.
const MaxDestinations = 10

var (
	// ErrUnavailable: the repository does not exist or is not this user's.
	ErrUnavailable = errors.New("repository is unavailable or not owned by this user")
	// ErrDisabled: the operator turned outbound connections off.
	ErrDisabled = errors.New("outbound connections are turned off on this server")
	// ErrAccessRemoved: GitHub access was removed; approvals can be removed
	// but not added.
	ErrAccessRemoved = errors.New("GitHub access to this repository was removed")
	// ErrLimit: the repository already has MaxDestinations approvals.
	ErrLimit = errors.New("this repository already has the maximum number of approved destinations")
	// ErrPortTaken: another destination is approved on that port.
	ErrPortTaken = errors.New("another destination is already approved on this port")
)

// InvalidError explains why a destination was rejected.
type InvalidError struct {
	Field   string // host or port
	Message string
}

func (e *InvalidError) Error() string { return e.Message }

// Approved is one approved destination as its owner sees it.
type Approved struct {
	ID        int64
	Host      string
	Port      int
	CreatedAt time.Time
}

// RepositoryView identifies an owned repository.
type RepositoryView struct {
	ID        int64
	FullName  string
	Available bool // GitHub access is current; adding approvals requires it
}

// Store keeps approvals under owner-scoped queries. Add and Remove also wake
// the worker's refresh in the same transaction, so a removal reaches running
// previews within seconds.
type Store interface {
	Repository(ctx context.Context, userID, repoID int64) (RepositoryView, error)
	List(ctx context.Context, userID, repoID int64) ([]Approved, error)
	Add(ctx context.Context, userID, repoID int64, d egress.Destination, limit int) error
	Remove(ctx context.Context, userID, repoID, id int64) (egress.Destination, error)
}

// Service applies the rules: owner only, the operator's ports, DNS names that
// are not obviously internal, one destination per port, at most
// MaxDestinations per repository.
type Service struct {
	store Store
	ports []int // allowed destination ports; none: the feature is off
}

func NewService(store Store, ports []int) (*Service, error) {
	if store == nil {
		return nil, errors.New("outbound connections need a store")
	}
	return &Service{store: store, ports: slices.Clone(ports)}, nil
}

// Enabled reports whether the operator allows outbound connections.
func (s *Service) Enabled() bool { return len(s.ports) > 0 }

// Ports are the destination ports owners may approve.
func (s *Service) Ports() []int { return slices.Clone(s.ports) }

func (s *Service) Repository(ctx context.Context, userID, repoID int64) (RepositoryView, error) {
	return s.store.Repository(ctx, userID, repoID)
}

func (s *Service) List(ctx context.Context, userID, repoID int64) ([]Approved, error) {
	return s.store.List(ctx, userID, repoID)
}

// internalSuffixes name hosts that never resolve publicly; approving them
// could only fail at connection time, so they are refused at once.
var internalSuffixes = []string{".local", ".localhost", ".internal", ".lan", ".home.arpa", ".test", ".invalid", ".example", ".onion"}

// Parse validates an owner's input into a destination.
func (s *Service) Parse(host, port string) (egress.Destination, error) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return egress.Destination{}, &InvalidError{Field: "host", Message: "Enter a host name, not an IP address: the egress proxy resolves the name itself and connects only to public addresses."}
	}
	n, err := strconv.Atoi(strings.TrimSpace(port))
	d := egress.Destination{Host: host, Port: n}
	switch {
	case host == "" || !egress.ValidDestination(egress.Destination{Host: host, Port: 1}):
		return egress.Destination{}, &InvalidError{Field: "host", Message: "Enter a full host name, such as db.example.com."}
	case slices.ContainsFunc(internalSuffixes, func(suffix string) bool { return strings.HasSuffix(host, suffix) }):
		return egress.Destination{}, &InvalidError{Field: "host", Message: "That name is internal; previews can only reach destinations on the public internet."}
	case err != nil || n < 1 || n > 65535:
		return egress.Destination{}, &InvalidError{Field: "port", Message: "Enter a port number."}
	case !slices.Contains(s.ports, n):
		return egress.Destination{}, &InvalidError{Field: "port", Message: fmt.Sprintf("This server allows ports %s.", joinPorts(s.ports))}
	}
	return d, nil
}

// Add approves a destination for the owner's repository.
func (s *Service) Add(ctx context.Context, userID, repoID int64, host, port string) (egress.Destination, error) {
	if !s.Enabled() {
		return egress.Destination{}, ErrDisabled
	}
	d, err := s.Parse(host, port)
	if err != nil {
		return egress.Destination{}, err
	}
	if err := s.store.Add(ctx, userID, repoID, d, MaxDestinations); err != nil {
		return egress.Destination{}, err
	}
	return d, nil
}

// Remove withdraws an approval; it works even after access was removed.
func (s *Service) Remove(ctx context.Context, userID, repoID, id int64) (egress.Destination, error) {
	return s.store.Remove(ctx, userID, repoID, id)
}

func joinPorts(ports []int) string {
	text := make([]string, len(ports))
	for i, port := range ports {
		text[i] = strconv.Itoa(port)
	}
	return strings.Join(text, ", ")
}
