package config

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/gowthamgts/mailrelay/internal/models"
	"github.com/knadh/koanf/parsers/json"
	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// Load reads configuration from a file (if it exists) and then layers
// environment variable overrides on top. The config file is optional;
// when absent the application runs with defaults + env vars.
//
// Environment variables use the prefix MAILRELAY_ with double-underscore
// as the nesting delimiter (e.g. MAILRELAY_SMTP__ADDR -> smtp.addr).
func Load(configPath string) (*models.AppConfig, error) {
	k := koanf.New(".")

	if _, err := os.Stat(configPath); err == nil {
		parser, err := parserForFile(configPath)
		if err != nil {
			return nil, err
		}
		if err := k.Load(file.Provider(configPath), parser); err != nil {
			return nil, fmt.Errorf("loading config: %w", err)
		}
	}

	if err := k.Load(env.Provider("MAILRELAY_", ".", envKeyReplacer), nil); err != nil {
		return nil, fmt.Errorf("loading env vars: %w", err)
	}

	cfg := defaults()

	if err := k.Unmarshal("", cfg); err != nil {
		return nil, fmt.Errorf("unmarshalling config: %w", err)
	}

	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return cfg, nil
}

// envKeyReplacer transforms MAILRELAY_SECTION__KEY into section.key.
func envKeyReplacer(s string) string {
	s = strings.TrimPrefix(s, "MAILRELAY_")
	s = strings.ReplaceAll(s, "__", ".")
	return strings.ToLower(s)
}

func parserForFile(p string) (koanf.Parser, error) {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".yaml", ".yml":
		return yaml.Parser(), nil
	case ".json":
		return json.Parser(), nil
	case ".toml":
		return toml.Parser(), nil
	default:
		return nil, fmt.Errorf("unsupported config file extension: %s", filepath.Ext(p))
	}
}

func defaults() *models.AppConfig {
	return &models.AppConfig{
		LogLevel: "info",
		SMTP: models.SMTPConfig{
			Addr:            "0.0.0.0:25",
			MaxMessageBytes: 25 * 1024 * 1024, // 25 MB
			MaxRecipients:   100,
			ReadTimeout:     60 * time.Second,
			WriteTimeout:    60 * time.Second,
		},
		Auth: models.AuthConfig{
			SPF:   models.AuthModeLog,
			DKIM:  models.AuthModeLog,
			DMARC: models.AuthModeLog,
		},
		HTTP: models.HTTPConfig{
			Addr: "127.0.0.1:2623",
			Auth: models.HTTPAuthConfig{
				Enabled:            false,
				CookieName:         "mailrelay_session",
				CookieSameSite:     "lax",
				SessionTTL:         12 * time.Hour,
				AllowedEmailDomains: nil,
			},
			ProtectMetrics: false,
		},
		Retry: models.RetryConfig{
			MaxRetries:     3,
			InitialWait:    1 * time.Second,
			MaxWait:        30 * time.Second,
			Timeout:        30 * time.Second,
			RetryOnTimeout: true,
		},
		WebUI: models.WebUIConfig{
			Enabled:       false,
			DBPath:        "mailrelay.db",
			RetentionDays: 7,
		},
	}
}

func validate(cfg *models.AppConfig) error {
	if cfg.WebUI.Enabled && cfg.WebUI.DBPath == "" {
		return fmt.Errorf("webui.db_path is required when webui is enabled")
	}

	if cfg.WebUI.Enabled && cfg.WebUI.RawEmailDir != "" {
		if err := os.MkdirAll(cfg.WebUI.RawEmailDir, 0o750); err != nil {
			return fmt.Errorf("creating raw_email_dir %q: %w", cfg.WebUI.RawEmailDir, err)
		}
	}

	authModes := []*models.AuthMode{&cfg.Auth.SPF, &cfg.Auth.DKIM, &cfg.Auth.DMARC}
	authNames := []string{"spf", "dkim", "dmarc"}
	for i, mode := range authModes {
		switch *mode {
		case "":
			*mode = models.AuthModeLog
		case models.AuthModeOff, models.AuthModeLog, models.AuthModeEnforce:
		default:
			return fmt.Errorf("auth.%s: invalid mode %q (must be off, log, or enforce)", authNames[i], *mode)
		}
	}

	// Validate allowed_recipients patterns are valid globs.
	for _, pattern := range cfg.SMTP.AllowedRecipients {
		if _, err := path.Match(pattern, "test@example.com"); err != nil {
			return fmt.Errorf("invalid allowed_recipients pattern %q: %w", pattern, err)
		}
	}

	return nil
}
