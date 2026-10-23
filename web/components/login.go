package components

import "context"

type googleLoginContextKey struct{}

func WithGoogleLogin(ctx context.Context, enabled bool) context.Context {
	return context.WithValue(ctx, googleLoginContextKey{}, enabled)
}
func GoogleLoginEnabled(ctx context.Context) bool {
	enabled, _ := ctx.Value(googleLoginContextKey{}).(bool)
	return enabled
}

// SignInURL is where a "Get started" that names no method goes: the choice of
// sign-in methods when Google is on, otherwise straight to GitHub.
func SignInURL(ctx context.Context) string {
	if GoogleLoginEnabled(ctx) {
		return "/auth/login"
	}
	return "/auth/github/login"
}

type githubConnectedContextKey struct{}

func WithGitHubConnected(ctx context.Context, connected bool) context.Context {
	return context.WithValue(ctx, githubConnectedContextKey{}, connected)
}
func GitHubConnected(ctx context.Context) bool {
	connected, ok := ctx.Value(githubConnectedContextKey{}).(bool)
	return !ok || connected
}
