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

type githubConnectedContextKey struct{}

func WithGitHubConnected(ctx context.Context, connected bool) context.Context {
	return context.WithValue(ctx, githubConnectedContextKey{}, connected)
}
func GitHubConnected(ctx context.Context) bool {
	connected, ok := ctx.Value(githubConnectedContextKey{}).(bool)
	return !ok || connected
}
