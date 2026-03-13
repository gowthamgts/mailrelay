package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gowthamgts/mailrelay/internal/models"
)

func TestParserForFile(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"yaml", "config.yaml", false},
		{"yml", "config.yml", false},
		{"json", "config.json", false},
		{"toml", "config.toml", false},
		{"unsupported", "config.xml", true},
		{"no extension", "config", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := parserForFile(tt.path)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p == nil {
				t.Fatal("expected non-nil parser")
			}
		})
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *models.AppConfig
		wantErr string
	}{
		{
			"empty config is valid",
			&models.AppConfig{},
			"",
		},
		{
			"invalid glob pattern",
			&models.AppConfig{
				SMTP: models.SMTPConfig{AllowedRecipients: []string{"[invalid"}},
			},
			"invalid allowed_recipients pattern",
		},
		{
			"valid config with webui",
			&models.AppConfig{
				WebUI: models.WebUIConfig{Enabled: true, DBPath: "test.db"},
			},
			"",
		},
		{
			"webui enabled without db_path",
			&models.AppConfig{
				WebUI: models.WebUIConfig{Enabled: true},
			},
			"webui.db_path is required",
		},
		{
			"invalid auth mode",
			&models.AppConfig{
				Auth: models.AuthConfig{
					SPF:   models.AuthModeLog,
					DKIM:  "invalid",
					DMARC: models.AuthModeOff,
					ARC:   models.AuthModeEnforce,
				},
			},
			"invalid mode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validate(tt.cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if got := err.Error(); !contains(got, tt.wantErr) {
				t.Errorf("error %q does not contain %q", got, tt.wantErr)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	yamlContent := `
log_level: debug
smtp:
  addr: "0.0.0.0:2525"
`
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.LogLevel != "debug" {
		t.Errorf("log_level = %q, want debug", cfg.LogLevel)
	}
	if cfg.SMTP.Addr != "0.0.0.0:2525" {
		t.Errorf("smtp addr = %q, want 0.0.0.0:2525", cfg.SMTP.Addr)
	}
	if cfg.HTTP.Addr != "127.0.0.1:2623" {
		t.Errorf("default http addr = %q, want 127.0.0.1:2623", cfg.HTTP.Addr)
	}
}

func TestLoadUnsupportedExtension(t *testing.T) {
	dir := t.TempDir()
	xmlPath := filepath.Join(dir, "config.xml")
	if err := os.WriteFile(xmlPath, []byte("<config/>"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(xmlPath)
	if err == nil {
		t.Fatal("expected error for unsupported extension")
	}
}

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	cfg, err := Load("/nonexistent/config.yaml")
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("log_level = %q, want info", cfg.LogLevel)
	}
	if cfg.SMTP.Addr != "0.0.0.0:25" {
		t.Errorf("smtp addr = %q, want 0.0.0.0:25", cfg.SMTP.Addr)
	}
}

func TestEnvVarsOverrideFile(t *testing.T) {
	yamlContent := `
log_level: info
smtp:
  addr: "0.0.0.0:25"
`
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("MAILRELAY_LOG_LEVEL", "debug")
	t.Setenv("MAILRELAY_SMTP__ADDR", "0.0.0.0:2525")

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("log_level = %q, want debug (env override)", cfg.LogLevel)
	}
	if cfg.SMTP.Addr != "0.0.0.0:2525" {
		t.Errorf("smtp addr = %q, want 0.0.0.0:2525 (env override)", cfg.SMTP.Addr)
	}
}

func TestEnvVarsWithoutConfigFile(t *testing.T) {
	t.Setenv("MAILRELAY_LOG_LEVEL", "warn")
	t.Setenv("MAILRELAY_WEBUI__ENABLED", "true")
	t.Setenv("MAILRELAY_WEBUI__DB_PATH", "/data/mailrelay.db")
	t.Setenv("MAILRELAY_HTTP__ADDR", "0.0.0.0:9090")

	cfg, err := Load("/nonexistent/config.yaml")
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.LogLevel != "warn" {
		t.Errorf("log_level = %q, want warn", cfg.LogLevel)
	}
	if !cfg.WebUI.Enabled {
		t.Error("webui.enabled = false, want true")
	}
	if cfg.WebUI.DBPath != "/data/mailrelay.db" {
		t.Errorf("webui.db_path = %q, want /data/mailrelay.db", cfg.WebUI.DBPath)
	}
	if cfg.HTTP.Addr != "0.0.0.0:9090" {
		t.Errorf("http.addr = %q, want 0.0.0.0:9090", cfg.HTTP.Addr)
	}
}

func TestEnvKeyReplacer(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"MAILRELAY_LOG_LEVEL", "log_level"},
		{"MAILRELAY_SMTP__ADDR", "smtp.addr"},
		{"MAILRELAY_WEBUI__DB_PATH", "webui.db_path"},
		{"MAILRELAY_WEBUI__RAW_EMAIL_DIR", "webui.raw_email_dir"},
		{"MAILRELAY_RETRY__MAX_RETRIES", "retry.max_retries"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := envKeyReplacer(tt.input)
			if got != tt.want {
				t.Errorf("envKeyReplacer(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && containsSubstr(s, substr)
}

func containsSubstr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
