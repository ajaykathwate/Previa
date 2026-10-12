package secrets

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// RegistryAuthName is the build secret that logs builds in to registries
// for private base images: one "host username token" per line. It is used
// only for pulls and is not given to Dockerfile steps.
const RegistryAuthName = "REGISTRY_AUTH"

// RegistryLogin is one registry a build may pull private base images from.
type RegistryLogin struct {
	Host, Username, Secret string
}

// BuildSecrets are the repository secrets an owner allowed Dockerfile steps
// of trusted builds to read (RUN --mount=type=secret,id=NAME), and the
// registry logins for private base images. Call Wipe once the build ends.
type BuildSecrets struct {
	values     map[string][]byte
	registries []RegistryLogin
	extra      []string // values only the redactor needs (registry tokens)
	Notices    []string // safe messages that mention names only
	Withheld   bool     // build secrets exist or may exist but the source was untrusted
}

// Names are the secrets Dockerfile steps may read, sorted.
func (b *BuildSecrets) Names() []string {
	if b == nil {
		return nil
	}
	names := make([]string, 0, len(b.values))
	for name := range b.values {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Values maps each name to its value, for BuildKit's secret provider.
func (b *BuildSecrets) Values() map[string][]byte {
	if b == nil {
		return nil
	}
	return b.values
}

// Registries are the registry logins for base image pulls.
func (b *BuildSecrets) Registries() []RegistryLogin {
	if b == nil {
		return nil
	}
	return b.registries
}

// Redactor hides every build secret value (and each registry token) in the
// build log, plus extra platform credentials.
func (b *BuildSecrets) Redactor(extra ...string) (*Redactor, error) {
	var values []string
	if b != nil {
		for _, v := range b.values {
			values = append(values, string(v))
		}
		values = append(values, b.extra...)
	}
	return NewRedactor(append(values, extra...)...)
}

// Wipe clears the decrypted values held here.
func (b *BuildSecrets) Wipe() {
	if b == nil {
		return
	}
	for name, v := range b.values {
		clear(v)
		delete(b.values, name)
	}
	b.registries, b.extra = nil, nil
}

// registryHost is a registry name such as ghcr.io or registry.example.com:5000.
var registryHost = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?(:[0-9]{1,5})?$`)

// parseRegistryAuth reads "host username token" lines. Messages name line
// numbers only, never their text.
func parseRegistryAuth(value string) ([]RegistryLogin, []string) {
	var logins []RegistryLogin
	var notices []string
	seen := map[string]bool{}
	for i, line := range strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		host := ""
		if len(fields) == 3 {
			host = strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(fields[0], "https://"), "http://"))
		}
		if len(fields) != 3 || !validRegistryHost(host) || len(fields[2]) < MinRedactBytes || seen[host] {
			notices = append(notices, fmt.Sprintf("%s line %d is not \"host username token\" and was ignored.", RegistryAuthName, i+1))
			continue
		}
		seen[host] = true
		logins = append(logins, RegistryLogin{Host: host, Username: fields[1], Secret: fields[2]})
	}
	return logins, notices
}

func validRegistryHost(host string) bool {
	if !registryHost.MatchString(host) {
		return false
	}
	if _, port, ok := strings.Cut(host, ":"); ok {
		n, err := strconv.Atoi(port)
		return err == nil && n > 0 && n <= 65535
	}
	return true
}

func validateRegistryAuth(value []byte) error {
	logins, notices := parseRegistryAuth(string(value))
	if len(logins) == 0 || len(notices) != 0 {
		return &InvalidError{Field: "value", Message: "REGISTRY_AUTH needs one host username token per line, unique registry hosts with valid ports, and tokens of at least 4 characters."}
	}
	return nil
}

// ForBuild decrypts the secrets the owner allowed builds to read. Untrusted
// sources (forks, unknown heads) get none and nothing is decrypted, the same
// rule as ForContainer; a value that cannot be decrypted fails the build
// rather than running it without.
func (p *Provider) ForBuild(ctx context.Context, repoID int64, source Source) (*BuildSecrets, error) {
	if !source.Trusted() {
		return &BuildSecrets{Withheld: true, Notices: []string{"Build secrets were withheld: the pull request comes from a fork or its source could not be verified."}}, nil
	}
	sealed, err := p.store.SealedSecrets(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("read repository secrets: %w", err)
	}
	b := &BuildSecrets{values: map[string][]byte{}}
	var build []SealedSecret
	for _, s := range sealed {
		if s.Build {
			build = append(build, s)
		}
	}
	if len(build) == 0 {
		return b, nil
	}
	if p.keys == nil {
		return nil, fmt.Errorf("%w (no keyring is configured on this worker)", ErrSecretsUnavailable)
	}
	for _, s := range build {
		plain, err := p.keys.Open(repoID, s.Name, s.Version, s.Value)
		if err != nil {
			b.Wipe()
			return nil, fmt.Errorf("%w (secret %s, key version %d)", ErrSecretsUnavailable, s.Name, s.Version)
		}
		if s.Name == RegistryAuthName {
			logins, notices := parseRegistryAuth(string(plain))
			if len(notices) != 0 || len(logins) == 0 {
				clear(plain)
				b.Wipe()
				return nil, fmt.Errorf("%w (REGISTRY_AUTH has invalid registry entries)", ErrSecretsUnavailable)
			}
			b.registries, b.Notices = append(b.registries, logins...), append(b.Notices, notices...)
			b.extra = append(b.extra, string(plain))
			for _, l := range logins {
				b.extra = append(b.extra, l.Secret)
			}
			clear(plain)
			continue
		}
		b.values[s.Name] = plain
	}
	return b, nil
}
