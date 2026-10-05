package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
)

// Environments: production turns on the checks that a developer machine
// cannot always meet (file modes on /mnt/c, plain-HTTP localhost).
const (
	Development = "development"
	Production  = "production"
)

// EnvironmentConfig is shared by the server and the worker.
type EnvironmentConfig struct {
	AppEnv string `envconfig:"APP_ENV" default:"development"`
}

// Production reports whether APP_ENV=production.
func (c EnvironmentConfig) Production() bool { return c.AppEnv == Production }

func (c EnvironmentConfig) check() error {
	if c.AppEnv != Development && c.AppEnv != Production {
		return errors.New("APP_ENV must be development or production")
	}
	return nil
}

// checkListenAddress requires host:port with a numeric port.
func checkListenAddress(name, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" || strings.ContainsAny(host, "/@") {
		return fmt.Errorf("%s must be host:port", name)
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return fmt.Errorf("%s must be host:port", name)
		}
	}
	return nil
}

// ParseTrustedProxies reads TRUSTED_PROXIES: comma-separated addresses or
// CIDR ranges of the reverse proxies allowed to set X-Forwarded-* headers.
func ParseTrustedProxies(value string) ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	for _, raw := range strings.Split(value, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(raw); err == nil {
			prefixes = append(prefixes, prefix.Masked())
			continue
		}
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			return nil, fmt.Errorf("TRUSTED_PROXIES entry %q is not an address or CIDR range", raw)
		}
		prefixes = append(prefixes, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return prefixes, nil
}

// CheckPrivateFile refuses a credential file that other users could read:
// it must be a regular file with no group or other permission bits.
func CheckPrivateFile(name, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s cannot be read: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file", name)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s must not be readable by group or others (chmod 600; it is %#o)", name, info.Mode().Perm())
	}
	return nil
}
