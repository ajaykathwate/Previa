package routing

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/ajaykathwatee/previa/internal/environments"
)

// UnavailablePath is the dashboard page unknown and removed preview hosts are
// redirected to.
const UnavailablePath = "/previews/unavailable"

const unavailableName = "previa-unavailable"

// WakePath is the dashboard page a sleeping preview's visitors are sent to,
// followed by the preview's host and the path they asked for:
// /previews/wake/<host>/<path>?<query>.
const WakePath = "/previews/wake"

const wakeName = "previa-wake"

// AccessPath is the server's forward-authentication endpoint for private
// previews; the router calls it before every request to one.
const AccessPath = "/previews/auth"

const accessName = "previa-access"

var (
	domainPattern  = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	backendPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
)

// Settings describe how the router exposes previews.
type Settings struct {
	BaseDomain     string // previews are <label>.<BaseDomain>
	EntryPoint     string // the router's HTTPS entry point, with the wildcard certificate
	UnavailableURL string // absolute URL of the dashboard's unavailable page
	WakeURL        string // absolute URL of the dashboard's wake page (WakePath)
	// AccessURL is the server's AccessPath as the router reaches it; empty
	// when private previews cannot be checked, which then serve nothing.
	AccessURL string
}

func (s Settings) validate() error {
	if len(s.BaseDomain) > 200 || !domainPattern.MatchString(s.BaseDomain) {
		return errors.New("preview base domain must be a lowercase DNS name such as preview.example.com")
	}
	if !backendPattern.MatchString(s.EntryPoint) {
		return errors.New("router entry point name is invalid")
	}
	pages := []string{s.UnavailableURL, s.WakeURL}
	if s.AccessURL != "" {
		pages = append(pages, s.AccessURL)
	}
	for _, page := range pages {
		u, err := url.Parse(page)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || strings.ContainsAny(page, "$`\"\\ ") {
			return errors.New("unavailable and wake page URLs must be absolute HTTP(S) URLs without credentials")
		}
	}
	return nil
}

// routerName and serviceName are stable names in the router's configuration.
// A service name carries the route version, so the router's API shows which
// version it serves.
func routerName(environmentID int64) string { return "preview-" + strconv.FormatInt(environmentID, 10) }

func serviceName(environmentID, generation, revision int64) string {
	return fmt.Sprintf("preview-%d-g%d-r%d", environmentID, generation, revision)
}

// Render turns a route table snapshot into Traefik dynamic configuration (JSON,
// which Traefik's YAML file provider reads). Every active route sends its host
// to its candidate on the PR's network; any other host under the base domain
// is redirected to the dashboard's unavailable page. Routers set no TLS block:
// the entry point's wildcard certificate covers them all.
func Render(s Settings, table environments.RouteTable) ([]byte, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	config := dynamicConfig{HTTP: httpConfig{
		Routers: map[string]router{
			unavailableName: {
				Rule:        "HostRegexp(`^.+\\." + regexp.QuoteMeta(s.BaseDomain) + "$`)",
				EntryPoints: []string{s.EntryPoint},
				Service:     "noop@internal",
				Priority:    1,
				Middlewares: []string{unavailableName},
			},
		},
		Services: map[string]service{},
		Middlewares: map[string]middleware{
			unavailableName: {RedirectRegex: &redirectRegex{Regex: "^.*$", Replacement: s.UnavailableURL}},
			wakeName:        {RedirectRegex: &redirectRegex{Regex: "^https?://([^/]+)/?(.*)$", Replacement: s.WakeURL + "/${1}/${2}"}},
		},
	}}
	if s.AccessURL != "" {
		config.HTTP.Middlewares[accessName] = middleware{ForwardAuth: &forwardAuth{Address: s.AccessURL}}
	}
	for _, route := range table.Routes {
		if route.EnvironmentID <= 0 || !ValidLabel(route.Hostname) || !backendPattern.MatchString(route.Backend) || route.Port <= 0 || route.Port > 65535 {
			return nil, fmt.Errorf("route of environment %d is invalid", route.EnvironmentID)
		}
		if route.Private && s.AccessURL == "" {
			// Nothing can check its visitors: it serves nobody (fails closed).
			config.HTTP.Routers[routerName(route.EnvironmentID)] = router{
				Rule:        "Host(`" + Host(route.Hostname, s.BaseDomain) + "`)",
				EntryPoints: []string{s.EntryPoint},
				Service:     "noop@internal",
				Middlewares: []string{unavailableName},
			}
			continue
		}
		var guard []string
		if route.Private {
			guard = []string{accessName}
		}
		if route.Asleep {
			// Its app is stopped: visitors wait on the wake page, which
			// sends them back once it runs.
			config.HTTP.Routers[routerName(route.EnvironmentID)] = router{
				Rule:        "Host(`" + Host(route.Hostname, s.BaseDomain) + "`)",
				EntryPoints: []string{s.EntryPoint},
				Service:     "noop@internal",
				Middlewares: append(guard, wakeName),
			}
			continue
		}
		name := serviceName(route.EnvironmentID, route.Generation, route.Revision)
		config.HTTP.Routers[routerName(route.EnvironmentID)] = router{
			Rule:        "Host(`" + Host(route.Hostname, s.BaseDomain) + "`)",
			EntryPoints: []string{s.EntryPoint},
			Service:     name,
			Middlewares: guard,
		}
		config.HTTP.Services[name] = service{LoadBalancer: loadBalancer{
			Servers:        []server{{URL: "http://" + route.Backend + ":" + strconv.Itoa(route.Port)}},
			PassHostHeader: true,
		}}
	}
	content, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode router configuration: %w", err)
	}
	return append(content, '\n'), nil
}

type dynamicConfig struct {
	HTTP httpConfig `json:"http"`
}

type httpConfig struct {
	Routers     map[string]router     `json:"routers"`
	Services    map[string]service    `json:"services,omitempty"`
	Middlewares map[string]middleware `json:"middlewares"`
}

type router struct {
	Rule        string   `json:"rule"`
	EntryPoints []string `json:"entryPoints"`
	Service     string   `json:"service"`
	Priority    int      `json:"priority,omitempty"`
	Middlewares []string `json:"middlewares,omitempty"`
}

type service struct {
	LoadBalancer loadBalancer `json:"loadBalancer"`
}

type loadBalancer struct {
	Servers        []server `json:"servers"`
	PassHostHeader bool     `json:"passHostHeader"`
}

type server struct {
	URL string `json:"url"`
}

type middleware struct {
	RedirectRegex *redirectRegex `json:"redirectRegex,omitempty"`
	ForwardAuth   *forwardAuth   `json:"forwardAuth,omitempty"`
}

// forwardAuth asks Address before each request; a 2xx answer lets it
// through, anything else (a redirect to sign in, with its cookie) goes back
// to the visitor. Traefik sets X-Forwarded-Host, -Uri and -Method itself.
type forwardAuth struct {
	Address string `json:"address"`
}

type redirectRegex struct {
	Regex       string `json:"regex"`
	Replacement string `json:"replacement"`
}
