package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kelseyhightower/envconfig"
	"golang.org/x/net/publicsuffix"
)

var (
	previewDomainPattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	containerNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
)

// RoutingConfig is how the worker publishes previews through the router.
// Routing is configured when PREVIEW_BASE_DOMAIN is set; every other routing
// setting is then validated. Nothing routes until execution is wired.
type RoutingConfig struct {
	// PreviewBaseDomain holds every preview hostname (<label>.<domain>).
	PreviewBaseDomain string `envconfig:"PREVIEW_BASE_DOMAIN"`
	// AppBaseURL is the dashboard origin; unknown preview hosts are
	// redirected to its unavailable page.
	AppBaseURL       string `envconfig:"APP_BASE_URL"`
	RouterContainer  string `envconfig:"ROUTER_CONTAINER" default:"previa-router"`
	RouterDynamicDir string `envconfig:"ROUTER_DYNAMIC_DIR"`
	RouterEntryPoint string `envconfig:"ROUTER_ENTRYPOINT" default:"websecure"`
	// RouterAPIAddress is Traefik's API inside the router container, bound to
	// loopback so nothing on any network can reach it.
	RouterAPIAddress string `envconfig:"ROUTER_API_ADDRESS" default:"127.0.0.1:8080"`
}

// RouteSyncConfig is what the route-sync maintenance command needs.
type RouteSyncConfig struct {
	SharedConfig
	RoutingConfig
}

// LoadRouteSync requires the database and a complete routing configuration.
func LoadRouteSync() (RouteSyncConfig, error) {
	var cfg RouteSyncConfig
	if err := envconfig.Process("", &cfg); err != nil {
		return RouteSyncConfig{}, fmt.Errorf("load route sync configuration: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return RouteSyncConfig{}, fmt.Errorf("validate route sync configuration: %w", err)
	}
	if !cfg.RoutingConfigured() {
		return RouteSyncConfig{}, errors.New("PREVIEW_BASE_DOMAIN is required to write the route table")
	}
	if err := cfg.check(); err != nil {
		return RouteSyncConfig{}, fmt.Errorf("validate routing configuration: %w", err)
	}
	return cfg, nil
}

// RoutingConfigured reports whether previews can be published.
func (c RoutingConfig) RoutingConfigured() bool { return strings.TrimSpace(c.PreviewBaseDomain) != "" }

func (c *RoutingConfig) check() error {
	if !c.RoutingConfigured() {
		if strings.TrimSpace(c.RouterDynamicDir) != "" {
			return errors.New("ROUTER_DYNAMIC_DIR needs PREVIEW_BASE_DOMAIN")
		}
		return nil
	}
	if err := CheckPreviewDomain(c.PreviewBaseDomain, c.AppBaseURL); err != nil {
		return err
	}
	dir := c.RouterDynamicDir
	switch {
	case strings.TrimSpace(dir) == "":
		return errors.New("ROUTER_DYNAMIC_DIR is required when PREVIEW_BASE_DOMAIN is set: the full path of the directory the router reads routes from, for example /home/you/.local/share/previa/router (make router-up creates it). Leave PREVIEW_BASE_DOMAIN empty to run without previews")
	case !filepath.IsAbs(dir) || filepath.Clean(dir) != dir:
		return fmt.Errorf("ROUTER_DYNAMIC_DIR must be a full, clean path such as /home/you/.local/share/previa/router, not %q: no ~, no relative parts, no trailing slash", dir)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return fmt.Errorf("ROUTER_DYNAMIC_DIR %s does not exist or is not a directory: run make router-up, which creates it and starts the router", dir)
	}
	if !containerNamePattern.MatchString(c.RouterContainer) {
		return errors.New("ROUTER_CONTAINER must be a Docker container name")
	}
	if !containerNamePattern.MatchString(c.RouterEntryPoint) {
		return errors.New("ROUTER_ENTRYPOINT must be a Traefik entry point name")
	}
	host, port, err := net.SplitHostPort(c.RouterAPIAddress)
	if ip := net.ParseIP(host); err != nil || port == "" || ip == nil || !ip.IsLoopback() {
		return errors.New("ROUTER_API_ADDRESS must be a loopback host:port inside the router container")
	}
	return nil
}

// CheckPreviewDomain validates the preview base domain against the dashboard
// origin. Previews run untrusted code, so the dashboard must never be one of
// their hosts: it may not equal the preview domain or sit below it.
func CheckPreviewDomain(domain, appBaseURL string) error {
	if len(domain) > 200 || !previewDomainPattern.MatchString(domain) {
		return errors.New("PREVIEW_BASE_DOMAIN must be a lowercase DNS name such as preview.example.com")
	}
	base, err := url.Parse(appBaseURL)
	if err != nil || base.Hostname() == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil {
		return errors.New("APP_BASE_URL must be the dashboard's HTTP(S) origin when PREVIEW_BASE_DOMAIN is set")
	}
	host := strings.ToLower(base.Hostname())
	if base.Scheme == "http" && host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return errors.New("APP_BASE_URL must use HTTPS except on localhost")
	}
	if host == domain || strings.HasSuffix(host, "."+domain) {
		return fmt.Errorf("APP_BASE_URL must not be %s or below it: previews run untrusted code and must never share the dashboard's host", domain)
	}
	return nil
}

// PreviewSameSite reports whether previews share the dashboard's registrable
// domain. That is allowed (dashboard cookies are host-only and, over HTTPS,
// __Host- prefixed), but a separate registrable domain isolates them fully.
func PreviewSameSite(domain, appBaseURL string) bool {
	base, err := url.Parse(appBaseURL)
	if err != nil {
		return false
	}
	dashboard, err := publicsuffix.EffectiveTLDPlusOne(strings.ToLower(base.Hostname()))
	if err != nil {
		return false
	}
	preview, err := publicsuffix.EffectiveTLDPlusOne(domain)
	return err == nil && dashboard == preview
}
