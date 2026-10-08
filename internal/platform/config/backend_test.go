package config

import (
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestServiceCatalog(t *testing.T) {
	pinned := "@sha256:" + strings.Repeat("a", 64)
	catalog, err := ParseServiceCatalog("postgres:16=postgres:16-alpine" + pinned + ", postgres:17=registry.example:5000/pg" + pinned + ",redis:7.4=redis" + pinned)
	if err != nil {
		t.Fatal(err)
	}
	if image, ok := catalog.Image("postgres", "17"); !ok || image != "registry.example:5000/pg"+pinned {
		t.Fatalf("postgres 17: %q %v", image, ok)
	}
	if _, ok := catalog.Image("postgres", "15"); ok {
		t.Fatal("an unlisted version was offered")
	}
	if got := catalog.Versions("postgres"); !slices.Equal(got, []string{"17", "16"}) {
		t.Fatalf("versions newest first: %v", got)
	}
	if got := catalog.Offers(); !slices.Equal(got, []string{"postgres 17, 16", "redis 7.4"}) {
		t.Fatalf("offers: %v", got)
	}

	// Empty means Previa's pinned catalog; none offers nothing.
	defaults, err := ParseServiceCatalog("")
	if err != nil || defaults.Empty() || !slices.Equal(defaults.Versions("redis"), []string{"8", "7"}) {
		t.Fatalf("default catalog: %v %v", defaults.Offers(), err)
	}
	none, err := ParseServiceCatalog("none")
	if err != nil || !none.Empty() || len(none.Offers()) != 0 {
		t.Fatalf("none: %v %v", none.Offers(), err)
	}

	for _, bad := range []string{
		"postgres=postgres" + pinned,                         // no version
		"mysql:8=mysql" + pinned,                             // unsupported kind
		"postgres:latest=postgres" + pinned,                  // not a numeric version
		"postgres:17=postgres:17",                            // not pinned by digest
		"postgres:17=postgres@sha256:abc",                    // malformed digest
		"postgres:17=post gres" + pinned,                     // not a reference
		"postgres:17=a" + pinned + ",postgres:17=b" + pinned, // twice
	} {
		if _, err := ParseServiceCatalog(bad); err == nil {
			t.Errorf("accepted catalog %q", bad)
		}
	}
}

func TestOutboundPortsDefaultTagMatchesTheConstant(t *testing.T) {
	field, _ := reflect.TypeFor[OutboundConfig]().FieldByName("PreviewOutboundPorts")
	if field.Tag.Get("default") != DefaultOutboundPorts {
		t.Fatalf("default tag %q, constant %q", field.Tag.Get("default"), DefaultOutboundPorts)
	}
}

func TestOutboundPorts(t *testing.T) {
	ports, err := OutboundConfig{}.OutboundPorts()
	if err != nil || !slices.Equal(ports, []int{1433, 3306, 5432, 5671, 5672, 6379, 6380, 9092, 27017}) {
		t.Fatalf("defaults: %v %v", ports, err)
	}
	if ports, err := (OutboundConfig{PreviewOutboundPorts: "none"}).OutboundPorts(); err != nil || ports != nil {
		t.Fatalf("none: %v %v", ports, err)
	}
	if ports, err := (OutboundConfig{PreviewOutboundPorts: " 6379, 22 "}).OutboundPorts(); err != nil || !slices.Equal(ports, []int{22, 6379}) {
		t.Fatalf("custom: %v %v", ports, err)
	}
	many := make([]string, MaxOutboundPorts+1)
	for i := range many {
		many[i] = strconv.Itoa(10000 + i)
	}
	for _, bad := range []string{"0", "65536", "abc", "443", "80", "3128", "22,22", strings.Join(many, ",")} {
		if _, err := (OutboundConfig{PreviewOutboundPorts: bad}).OutboundPorts(); err == nil {
			t.Errorf("accepted ports %q", bad)
		}
	}
}

func TestPreviewBackendSettings(t *testing.T) {
	base := PreviewConfig{PreviewTmpfsSize: 256 << 20, PreviewPIDs: 512, PreviewStartupTimeout: time.Minute, PreviewMaxPerRepository: 10, PreviewSetupTimeout: 5 * time.Minute}
	if err := base.check(); err != nil {
		t.Fatal(err)
	}
	for _, tweak := range []func(*PreviewConfig){
		func(c *PreviewConfig) { c.PreviewSetupTimeout = time.Second },
		func(c *PreviewConfig) { c.PreviewSetupTimeout = time.Hour },
		func(c *PreviewConfig) { c.PreviewServiceCatalog = "postgres:17=postgres:17" },
	} {
		c := base
		tweak(&c)
		if err := c.check(); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}
