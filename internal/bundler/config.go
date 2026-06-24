package bundler

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// abiPlaceholder is the token substituted with the resolved kernel ABI in a
// DebianRef's AbiDep and Package templates.
const abiPlaceholder = "{abi}"

type Config struct {
	Sources []Source `json:"sources"`
}

// Source describes one input to a bundle build. A plain Source downloads a
// single file to Dst; an archive Source downloads an archive and selects files
// from it via Files (glob -> destination). The download location is either an
// explicit URL or a Debian package resolved from a repository index (exactly one
// of the two). It is the JSON config element.
type Source struct {
	URL     string            `json:"url,omitempty"`
	Debian  *DebianRef        `json:"debian,omitempty"` // resolve URL from a Debian Packages index
	Archive bool              `json:"archive,omitempty"`
	Dst     string            `json:"dst,omitempty"`   // output path for a plain file
	Files   map[string]string `json:"files,omitempty"` // glob -> dst, archive sources only
}

// DebianRef resolves a source URL from a Debian repository's "Packages" index
// instead of pinning a kernel version in the config, so a bundle tracks the
// suite's latest point release automatically (picked up on -refresh). The current
// kernel ABI is read from the meta-package Via's dependency named by AbiDep (a
// package-name template with an {abi} placeholder), then substituted into Package
// to locate the target stanza, whose Filename gives the download path under
// Mirror. See resolveDebianURL.
type DebianRef struct {
	Mirror    string `json:"mirror"`    // repository root, e.g. https://deb.debian.org/debian
	Suite     string `json:"suite"`     // release suite, e.g. trixie or unstable
	Component string `json:"component"` // archive component, e.g. main
	Arch      string `json:"arch"`      // Debian architecture, e.g. amd64, arm64, ppc64
	Via       string `json:"via"`       // meta-package whose Depends names the current ABI, e.g. linux-image-amd64
	AbiDep    string `json:"abiDep"`    // dependency template with an {abi} placeholder, e.g. linux-image-{abi}-amd64
	Package   string `json:"package"`   // target package template, {abi} substituted, e.g. linux-image-{abi}-amd64-unsigned
}

// label returns a human-readable identifier for the source, used in error and log
// messages; a Debian source has no URL until it is resolved.
func (s Source) label() string {
	switch {
	case s.URL != "":
		return s.URL
	case s.Debian != nil:
		return "debian:" + s.Debian.Package
	default:
		return "?"
	}
}

func (d *DebianRef) validate() error {
	for _, f := range []struct{ name, val string }{
		{"mirror", d.Mirror}, {"suite", d.Suite}, {"component", d.Component},
		{"arch", d.Arch}, {"via", d.Via}, {"abiDep", d.AbiDep}, {"package", d.Package},
	} {
		if f.val == "" {
			return fmt.Errorf("%s is required", f.name)
		}
	}
	if !strings.Contains(d.AbiDep, abiPlaceholder) {
		return fmt.Errorf("abiDep %q must contain the %s placeholder", d.AbiDep, abiPlaceholder)
	}
	if !strings.Contains(d.Package, abiPlaceholder) {
		return fmt.Errorf("package %q must contain the %s placeholder", d.Package, abiPlaceholder)
	}
	return nil
}

// LoadConfig reads and validates the JSON bundle config.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	if len(cfg.Sources) == 0 {
		return nil, errors.New("config has no sources")
	}
	for i, s := range cfg.Sources {
		switch {
		case s.URL == "" && s.Debian == nil:
			return nil, fmt.Errorf("source %d: exactly one of \"url\" or \"debian\" is required", i)
		case s.URL != "" && s.Debian != nil:
			return nil, fmt.Errorf("source %d (%s): \"url\" and \"debian\" are mutually exclusive", i, s.label())
		}
		if s.Debian != nil {
			if err := s.Debian.validate(); err != nil {
				return nil, fmt.Errorf("source %d: debian: %w", i, err)
			}
		}
		switch {
		case s.Archive && len(s.Files) == 0:
			return nil, fmt.Errorf("source %d (%s): an archive source requires a non-empty \"files\" map", i, s.label())
		case !s.Archive && s.Dst == "":
			return nil, fmt.Errorf("source %d (%s): a plain source requires \"dst\"", i, s.label())
		case !s.Archive && len(s.Files) > 0:
			return nil, fmt.Errorf("source %d (%s): \"files\" is only valid with \"archive\": true", i, s.label())
		}
	}
	return &cfg, nil
}
