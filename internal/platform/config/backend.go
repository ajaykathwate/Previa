package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// DefaultServiceCatalog is Previa's pinned backend services: the official
// PostgreSQL and Redis images, each pinned by its multi-platform index
// digest. An empty PREVIEW_SERVICE_CATALOG uses it; operators replace it to
// pin other digests, and "none" offers no services at all.
const DefaultServiceCatalog = "postgres:17=docker.io/library/postgres:17-alpine@sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24," +
	"postgres:16=docker.io/library/postgres:16-alpine@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea," +
	"redis:8=docker.io/library/redis:8-alpine@sha256:3811787313eba226a2ef38658c6ccb91cd5e110edc89c37767de373120a0e5a0," +
	"redis:7=docker.io/library/redis:7-alpine@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499"

// ServiceKinds are the backend services Previa can run for a preview.
var ServiceKinds = []string{"postgres", "redis"}

var (
	serviceVersion = regexp.MustCompile(`^[0-9]{1,3}(\.[0-9]{1,3}){0,2}$`)
	// pinnedImage is an image reference that names its content by digest.
	pinnedImage = regexp.MustCompile(`^[a-z0-9][A-Za-z0-9._/:-]{0,254}@sha256:[a-f0-9]{64}$`)
)

// ServiceCatalog is the operator's pinned backend services: for each kind,
// the versions previews may request and the image, pinned by digest, that
// each version runs. Repository configuration can only choose among them.
type ServiceCatalog struct {
	images map[string]map[string]string // kind -> version -> image
}

// ParseServiceCatalog reads "kind:version=image@sha256:digest" pairs
// separated by commas. Empty means DefaultServiceCatalog; "none" offers no
// services.
func ParseServiceCatalog(value string) (ServiceCatalog, error) {
	value = strings.TrimSpace(value)
	switch value {
	case "":
		value = DefaultServiceCatalog
	case "none":
		return ServiceCatalog{}, nil
	}
	catalog := ServiceCatalog{images: map[string]map[string]string{}}
	for entry := range strings.SplitSeq(value, ",") {
		entry = strings.TrimSpace(entry)
		key, image, ok := strings.Cut(entry, "=")
		kind, version, versioned := strings.Cut(key, ":")
		switch {
		case !ok || !versioned:
			return ServiceCatalog{}, errors.New("PREVIEW_SERVICE_CATALOG entries must look like postgres:17=image@sha256:digest")
		case !slices.Contains(ServiceKinds, kind):
			return ServiceCatalog{}, fmt.Errorf("PREVIEW_SERVICE_CATALOG offers %q; only postgres and redis are supported", kind)
		case !serviceVersion.MatchString(version):
			return ServiceCatalog{}, fmt.Errorf("PREVIEW_SERVICE_CATALOG version %q of %s must be numeric, like 17 or 7.4", version, kind)
		case !pinnedImage.MatchString(image):
			return ServiceCatalog{}, fmt.Errorf("PREVIEW_SERVICE_CATALOG image for %s %s must be pinned by digest (name@sha256:...)", kind, version)
		case catalog.images[kind][version] != "":
			return ServiceCatalog{}, fmt.Errorf("PREVIEW_SERVICE_CATALOG lists %s %s twice", kind, version)
		}
		if catalog.images[kind] == nil {
			catalog.images[kind] = map[string]string{}
		}
		catalog.images[kind][version] = image
	}
	return catalog, nil
}

// Image is the pinned image of one offered version.
func (c ServiceCatalog) Image(kind, version string) (string, bool) {
	image, ok := c.images[kind][version]
	return image, ok
}

// Versions lists the offered versions of kind, newest first.
func (c ServiceCatalog) Versions(kind string) []string {
	versions := make([]string, 0, len(c.images[kind]))
	for version := range c.images[kind] {
		versions = append(versions, version)
	}
	slices.SortFunc(versions, func(a, b string) int { return compareVersions(b, a) })
	return versions
}

// Empty reports whether no service is offered.
func (c ServiceCatalog) Empty() bool { return len(c.images) == 0 }

// Offers describes the catalog for people, for example
// "postgres 17, 16; redis 8, 7"; empty when nothing is offered.
func (c ServiceCatalog) Offers() []string {
	var offers []string
	for _, kind := range ServiceKinds {
		if versions := c.Versions(kind); len(versions) > 0 {
			offers = append(offers, kind+" "+strings.Join(versions, ", "))
		}
	}
	return offers
}

func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(as), len(bs)) {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			return x - y
		}
	}
	return 0
}

// DefaultOutboundPorts are the destination ports owners may approve when the
// operator sets none: common databases, caches and message brokers.
const DefaultOutboundPorts = "5432,3306,6379,6380,27017,5671,5672,9092,1433"

// MaxOutboundPorts bounds how many ports the egress proxy listens on.
const MaxOutboundPorts = 16

// OutboundConfig limits the owner-approved outbound connections previews may
// open through the egress proxy. Both processes read it: the server to accept
// approvals, the worker to forward them.
type OutboundConfig struct {
	// PreviewOutboundPorts lists the destination ports owners may approve
	// (DefaultOutboundPorts unless set); "none" turns the feature off.
	PreviewOutboundPorts string `envconfig:"PREVIEW_OUTBOUND_PORTS" default:"5432,3306,6379,6380,27017,5671,5672,9092,1433"`
}

// OutboundPorts returns the allowed destination ports in ascending order,
// or none when the operator turned the feature off.
func (c OutboundConfig) OutboundPorts() ([]int, error) {
	value := strings.TrimSpace(c.PreviewOutboundPorts)
	switch value {
	case "":
		value = DefaultOutboundPorts
	case "none":
		return nil, nil
	}
	var ports []int
	for part := range strings.SplitSeq(value, ",") {
		port, err := strconv.Atoi(strings.TrimSpace(part))
		switch {
		case err != nil || port < 1 || port > 65535:
			return nil, errors.New("PREVIEW_OUTBOUND_PORTS must list ports from 1 to 65535, separated by commas, or be none")
		case port == 80 || port == 443:
			return nil, errors.New("PREVIEW_OUTBOUND_PORTS must not list 80 or 443: previews reach HTTP and HTTPS through the egress proxy already")
		case port == egressProxyPort:
			return nil, errors.New("PREVIEW_OUTBOUND_PORTS must not list 3128, the egress proxy's own port")
		case slices.Contains(ports, port):
			return nil, fmt.Errorf("PREVIEW_OUTBOUND_PORTS lists %d twice", port)
		}
		ports = append(ports, port)
	}
	if len(ports) > MaxOutboundPorts {
		return nil, fmt.Errorf("PREVIEW_OUTBOUND_PORTS may list at most %d ports", MaxOutboundPorts)
	}
	slices.Sort(ports)
	return ports, nil
}

// egressProxyPort is where the egress proxy serves HTTP(S) (egress.Port).
const egressProxyPort = 3128

func (c OutboundConfig) checkOutbound() error {
	_, err := c.OutboundPorts()
	return err
}
