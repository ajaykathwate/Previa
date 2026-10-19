package realtime

import "context"

type sessionCheckKey struct{}

// WithSessionCheck carries the session owner's revocation check to streams.
// It complements resource authorization; neither can replace the other.
func WithSessionCheck(ctx context.Context, check func(context.Context) error) context.Context {
	return context.WithValue(ctx, sessionCheckKey{}, check)
}

// CheckSession revalidates an authenticated stream before releasing content.
func CheckSession(ctx context.Context) error {
	if check, ok := ctx.Value(sessionCheckKey{}).(func(context.Context) error); ok {
		return check(ctx)
	}
	return nil
}
