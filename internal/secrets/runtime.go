package secrets

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Source is the verified origin of the code a preview will run, from fresh
// upstream verification (environments.UpstreamSnapshot).
type Source struct {
	BaseRepoID int64 // GitHub repository ID of the PR's base
	HeadRepoID int64 // GitHub repository ID of the PR's head; 0 when unknown or deleted
}

// Trusted reports whether repository secrets may be given to this code: only
// when the head is provably the base repository. Fork PRs, deleted heads and
// missing or ambiguous identity are untrusted, whatever the build mode, retry or
// approval, because the fork's author controls the code.
func (s Source) Trusted() bool { return s.BaseRepoID > 0 && s.HeadRepoID == s.BaseRepoID }

// SealedSecret is one stored ciphertext, read by the worker after authorization.
type SealedSecret struct {
	Name    string
	Value   []byte
	Version int32
}

// SealedStore reads a repository's ciphertext without owner checks; callers have
// already authorized the build.
type SealedStore interface {
	SealedSecrets(ctx context.Context, repoID int64) ([]SealedSecret, error)
}

// ErrSecretsUnavailable means at least one value could not be decrypted, so the
// container must not start with a partial environment.
var ErrSecretsUnavailable = errors.New("repository secrets could not be decrypted; check SECRETS_ENCRYPTION_KEYS on the worker")

// Provider assembles the runtime environment for one container.
type Provider struct {
	store SealedStore
	keys  *Keyring
}

// NewProvider needs the store; keys may be nil on a worker without
// SECRETS_ENCRYPTION_KEYS, which then starts previews of repositories without
// secrets and refuses (fails closed) any repository that has some.
func NewProvider(store SealedStore, keys *Keyring) (*Provider, error) {
	if store == nil {
		return nil, errors.New("secret provider requires a store")
	}
	return &Provider{store: store, keys: keys}, nil
}

// ContainerEnv is the environment for exactly one container creation. Pass
// Environ only to the runtime's container configuration, immediately before
// creating the container, then call Wipe. Never pass it to image builds: secrets
// are runtime-only and must not reach build arguments, the build context or image
// layers.
type ContainerEnv struct {
	vars     []string
	secrets  [][]byte
	names    []string // of the decrypted secrets
	Notices  []string // safe messages that mention names only
	Withheld bool     // secrets existed or may exist but the source was untrusted
}

// Environ returns NAME=value entries sorted by name.
func (e *ContainerEnv) Environ() []string { return append([]string(nil), e.vars...) }

// SecretNames are the names of the variables that hold repository secrets.
// Names are not secret; values are.
func (e *ContainerEnv) SecretNames() []string { return append([]string(nil), e.names...) }

// Redactor redacts this environment's secret values plus extra platform
// credentials (for example the installation token used to fetch source).
func (e *ContainerEnv) Redactor(extra ...string) (*Redactor, error) {
	values := make([]string, 0, len(e.secrets)+len(extra))
	for _, s := range e.secrets {
		values = append(values, string(s))
	}
	return NewRedactor(append(values, extra...)...)
}

// Wipe clears decrypted values held here. Strings already handed to the runtime
// (Environ) are Go-managed copies and cannot be wiped; keep their lifetime short.
func (e *ContainerEnv) Wipe() {
	for _, s := range e.secrets {
		clear(s)
	}
	e.secrets = nil
	e.vars = nil
}

// ForContainer merges the non-secret YAML env with the repository's secrets.
// Untrusted sources get the YAML env only, and nothing is decrypted. For trusted
// sources a stored secret overrides a YAML entry with the same name, because the
// owner's value must win over contributor-editable YAML; the override is noted by
// name only.
func (p *Provider) ForContainer(ctx context.Context, repoID int64, source Source, yamlEnv []string) (*ContainerEnv, error) {
	merged := map[string]string{}
	for _, entry := range yamlEnv {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("invalid YAML env entry")
		}
		merged[name] = value
	}
	env := &ContainerEnv{}
	if !source.Trusted() {
		env.Withheld = true
		env.Notices = append(env.Notices, "Repository secrets were withheld: the pull request comes from a fork or its source could not be verified.")
		env.vars = sortedEnv(merged)
		return env, nil
	}
	sealed, err := p.store.SealedSecrets(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("read repository secrets: %w", err)
	}
	if len(sealed) > 0 && p.keys == nil {
		return nil, fmt.Errorf("%w (no keyring is configured on this worker)", ErrSecretsUnavailable)
	}
	for _, s := range sealed {
		plain, err := p.keys.Open(repoID, s.Name, s.Version, s.Value)
		if err != nil {
			env.Wipe()
			return nil, fmt.Errorf("%w (secret %s, key version %d)", ErrSecretsUnavailable, s.Name, s.Version)
		}
		if _, overridden := merged[s.Name]; overridden {
			env.Notices = append(env.Notices, fmt.Sprintf("Repository secret %s overrides the value in .previewrc.yaml.", s.Name))
		}
		merged[s.Name] = string(plain)
		env.secrets = append(env.secrets, plain)
		env.names = append(env.names, s.Name)
	}
	env.vars = sortedEnv(merged)
	return env, nil
}

func sortedEnv(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	env := make([]string, 0, len(names))
	for _, name := range names {
		env = append(env, name+"="+values[name])
	}
	return env
}
