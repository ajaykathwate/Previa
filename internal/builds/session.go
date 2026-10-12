package builds

import (
	"context"
	"strings"

	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth"
	"github.com/moby/buildkit/session/secrets/secretsprovider"
	"google.golang.org/grpc"
)

// sessionAttachables hands BuildKit what a build may use besides its
// context: secrets that Dockerfile steps mount (RUN --mount=type=secret)
// and registry logins for private base images. A build without them gets
// neither provider, so a step asking for a secret fails with a clear error.
func sessionAttachables(job Job) []session.Attachable {
	if job.BaseRepoID <= 0 || job.HeadRepoID != job.BaseRepoID {
		return nil
	}
	var attachables []session.Attachable
	if len(job.Secrets) > 0 {
		attachables = append(attachables, secretsprovider.FromMap(job.Secrets))
	}
	if len(job.Registries) > 0 {
		attachables = append(attachables, registryAuth{logins: job.Registries})
	}
	return attachables
}

// registryAuth answers BuildKit's credential requests from the owner's
// registry logins. It implements only Credentials: BuildKit then fetches
// registry tokens itself, and hosts without a login are pulled anonymously.
type registryAuth struct {
	auth.UnimplementedAuthServer
	logins []RegistryLogin
}

func (a registryAuth) Register(server *grpc.Server) { auth.RegisterAuthServer(server, a) }

func (a registryAuth) Credentials(_ context.Context, req *auth.CredentialsRequest) (*auth.CredentialsResponse, error) {
	host := registryKey(req.Host)
	for _, l := range a.logins {
		if registryKey(l.Host) == host {
			return &auth.CredentialsResponse{Username: l.Username, Secret: l.Secret}, nil
		}
	}
	return &auth.CredentialsResponse{}, nil
}

// registryKey treats Docker Hub's names as one registry.
func registryKey(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	switch host {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return "docker.io"
	}
	return host
}
