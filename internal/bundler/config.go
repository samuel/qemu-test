package bundler

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

type Config struct {
	Sources []Source `json:"sources"`
}

// Source describes one input to a bundle build. A plain Source downloads a
// single file to Dst; an archive Source downloads an archive and selects files
// from it via Files (glob -> destination). It is the JSON config element.
type Source struct {
	URL     string            `json:"url"`
	Archive bool              `json:"archive,omitempty"`
	Dst     string            `json:"dst,omitempty"`   // output path for a plain file
	Files   map[string]string `json:"files,omitempty"` // glob -> dst, archive sources only
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
		case s.URL == "":
			return nil, fmt.Errorf("source %d: url is required", i)
		case s.Archive && len(s.Files) == 0:
			return nil, fmt.Errorf("source %d (%s): an archive source requires a non-empty \"files\" map", i, s.URL)
		case !s.Archive && s.Dst == "":
			return nil, fmt.Errorf("source %d (%s): a plain source requires \"dst\"", i, s.URL)
		case !s.Archive && len(s.Files) > 0:
			return nil, fmt.Errorf("source %d (%s): \"files\" is only valid with \"archive\": true", i, s.URL)
		}
	}
	return &cfg, nil
}
