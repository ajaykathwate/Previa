package repoconfig

import (
	"context"
	"errors"
	"fmt"
	"regexp"
)

// Request identifies an installation and an immutable source commit. Authorization
// to use this installation/repository remains the calling feature's responsibility.
type Request struct {
	InstallationID int64
	Owner          string
	Repository     string
	CommitSHA      string
}

var commitSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
var repositoryPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func (r Request) validate() error {
	if r.InstallationID <= 0 || !commitSHA.MatchString(r.CommitSHA) ||
		!repositoryPart.MatchString(r.Owner) || !repositoryPart.MatchString(r.Repository) ||
		r.Owner == "." || r.Owner == ".." || r.Repository == "." || r.Repository == ".." {
		return errors.New("config fetch requires an installation, repository identity and full 40-character commit SHA")
	}
	return nil
}

// FileFetcher is the consumer-defined boundary for commit-specific file access.
type FileFetcher interface {
	Fetch(context.Context, Request) ([]byte, error)
}

type Service struct {
	fetcher FileFetcher
	limits  Resources
}

func NewService(fetcher FileFetcher, limits Resources) (*Service, error) {
	if fetcher == nil {
		return nil, errors.New("repository config requires a file fetcher")
	}
	if err := limits.validate(); err != nil {
		return nil, err
	}
	return &Service{fetcher: fetcher, limits: limits}, nil
}

func (s *Service) Load(ctx context.Context, request Request) (Result, error) {
	if err := request.validate(); err != nil {
		return Result{}, err
	}
	data, err := s.fetcher.Fetch(ctx, request)
	if err != nil {
		return Result{}, fmt.Errorf("load repository preview configuration: %w", err)
	}
	defer clear(data)
	return Parse(data, s.limits)
}
