package components

import (
	"context"
	"strings"
	"testing"
)

func TestNavbarGetStartedOffersEverySignInMethod(t *testing.T) {
	for _, tc := range []struct {
		google      bool
		want, avoid string
	}{
		{google: false, want: `href="/auth/github/login"`, avoid: `href="/auth/login"`},
		{google: true, want: `href="/auth/login"`, avoid: `href="/auth/github/login"`},
	} {
		var page strings.Builder
		if err := Navbar(false).Render(WithGoogleLogin(context.Background(), tc.google), &page); err != nil {
			t.Fatal(err)
		}
		if html := page.String(); !strings.Contains(html, tc.want) || strings.Contains(html, tc.avoid) {
			t.Errorf("Google %t: Get started should link %s, not %s", tc.google, tc.want, tc.avoid)
		}
	}
}
