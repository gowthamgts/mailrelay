package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/gowthamgts/mailrelay/internal/config"
	"github.com/gowthamgts/mailrelay/internal/httpauth"
	"github.com/gowthamgts/mailrelay/internal/httpcsrf"
	"github.com/gowthamgts/mailrelay/internal/models"
	"github.com/gowthamgts/mailrelay/internal/rules"
	smtpserver "github.com/gowthamgts/mailrelay/internal/smtp"
	"github.com/gowthamgts/mailrelay/internal/storage"
	"github.com/gowthamgts/mailrelay/internal/webhook"
	"github.com/gowthamgts/mailrelay/internal/webui"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	logLevel := setupLogger(cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("mailrelay starting",
		"config", *configPath,
		"smtp_addr", cfg.SMTP.Addr,
		"webui", cfg.WebUI.Enabled,
	)

	// Warn if running as a potential open relay.
	if len(cfg.SMTP.AllowedRecipients) == 0 &&
		!strings.HasPrefix(cfg.SMTP.Addr, "127.0.0.1:") &&
		!strings.HasPrefix(cfg.SMTP.Addr, "localhost:") {
		slog.Warn("smtp.allowed_recipients is empty and SMTP is listening on a non-localhost address; the server may behave as an open relay. Configure smtp.allowed_recipients for production use.")
	}

	engine := rules.NewEngine()
	dispatcher := webhook.NewDispatcher(cfg.Retry)

	var store *storage.Store
	var uiHandler *webui.Handler

	if cfg.WebUI.Enabled {
		store, err = storage.Open(cfg.WebUI.DBPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to open database: %v\n", err)
			os.Exit(1)
		}
		defer store.Close()
		slog.Info("web UI enabled", "db", cfg.WebUI.DBPath, "retention_days", cfg.WebUI.RetentionDays)

		// Load saved settings from DB and apply on top of file/env config.
		if saved, err := store.LoadSettings(ctx); err != nil {
			slog.Warn("failed to load saved settings", "error", err)
		} else if saved != nil {
			applySettings(cfg, saved)
			setLogLevel(logLevel, cfg.LogLevel)
			dispatcher.SetRetryConfig(cfg.Retry)
			slog.Info("applied saved settings from database")
		}

		uiHandler, err = webui.NewHandler(store, engine, dispatcher, cfg.WebUI.RawEmailDir, cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to initialize web UI: %v\n", err)
			os.Exit(1)
		}

		if err := uiHandler.LoadRules(ctx); err != nil {
			slog.Warn("failed to load rules from database", "error", err)
		}
	}

	var events *webui.EventBus
	if uiHandler != nil {
		events = uiHandler.Events
	}

	handler := buildHandler(engine, dispatcher, store, events, cfg.WebUI.RawEmailDir)
	srv := smtpserver.NewServer(ctx, cfg.SMTP, cfg.Auth, handler)

	if store != nil {
		srv.SetRejectionHandler(buildRejectionHandler(store, events, cfg.WebUI.RawEmailDir))
	}

	if uiHandler != nil {
		uiHandler.SetSMTPServer(srv)
		uiHandler.SetLogLevel(logLevel)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	httpMux := http.NewServeMux()
	httpMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	httpMux.Handle("/metrics", promhttp.Handler())

	var authManager *httpauth.Manager
	if cfg.HTTP.Auth.Enabled {
		mgr, err := httpauth.NewManager(cfg.HTTP.Auth, cfg.HTTP)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to initialize HTTP auth: %v\n", err)
			os.Exit(1)
		}
		authManager = mgr

		httpMux.Handle("/auth/login", authManager.LoginHandler())
		httpMux.Handle("/auth/callback", authManager.CallbackHandler())
		httpMux.Handle("/auth/logout", authManager.LogoutHandler())
	}

	if uiHandler != nil {
		uiHandler.Register(httpMux)
		slog.Info("web UI registered", "url", fmt.Sprintf("http://%s/", cfg.HTTP.Addr))
	}

	var rootHandler http.Handler = httpMux
	// Security headers are applied to all HTTP responses.
	rootHandler = withSecurityHeaders(rootHandler)

	if authManager != nil {
		allow := func(r *http.Request) bool {
			// Health check is always public.
			if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
				return true
			}
			// Metrics can be optionally protected.
			if r.Method == http.MethodGet && r.URL.Path == "/metrics" && !cfg.HTTP.ProtectMetrics {
				return true
			}
			// Login / callback / logout remain public entrypoints.
			if strings.HasPrefix(r.URL.Path, "/auth/") {
				return true
			}
			// Static assets are always behind whatever auth is applied to HTML.
			if strings.HasPrefix(r.URL.Path, "/static/") {
				return false
			}
			return false
		}
		rootHandler = authManager.AuthMiddleware(rootHandler, allow)
	}

	// CSRF protection for state-changing requests to the web UI and APIs.
	rootHandler = csrfMiddleware(rootHandler, cfg.HTTP)

	httpSrv := &http.Server{
		Addr:    cfg.HTTP.Addr,
		Handler: rootHandler,
	}
	go func() {
		slog.Info("HTTP server starting", "addr", cfg.HTTP.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server error", "error", err)
		}
	}()

	var retentionDays atomic.Int32
	retentionDays.Store(int32(cfg.WebUI.RetentionDays))
	if uiHandler != nil {
		uiHandler.SetRetentionDays(&retentionDays)
	}

	if cfg.WebUI.Enabled && store != nil && cfg.WebUI.RetentionDays > 0 {
		go runCleanup(ctx, store, &retentionDays, cfg.WebUI.RawEmailDir)
	}

	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received")
	case err := <-errCh:
		slog.Error("SMTP server error", "error", err)
		stop()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	slog.Info("shutting down...")
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("SMTP server shutdown error", "error", err)
	}
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP server shutdown error", "error", err)
	}
	slog.Info("mailrelay stopped")
}

func buildHandler(engine *rules.Engine, dispatcher *webhook.Dispatcher, store *storage.Store, events *webui.EventBus, rawEmailDir string) smtpserver.EmailHandler {
	return func(ctx context.Context, email *models.ParsedEmail, rawData []byte) {
		var emailID string

		if store != nil {
			emailID = uuid.New().String()
			record := &models.EmailRecord{
				ID:           emailID,
				ReceivedAt:   time.Now().UTC(),
				EnvelopeFrom: email.EnvelopeFrom,
				EnvelopeTo:   email.EnvelopeTo,
				Subject:      email.Subject,
				Status:       models.EmailStatusReceived,
				ParsedEmail:  email,
				AuthResult:   email.AuthResult,
			}
			if err := store.SaveEmail(ctx, record); err != nil {
				slog.Error("failed to save email record", "error", err)
			}

			if rawEmailDir != "" && len(rawData) > 0 {
				emlPath := filepath.Join(rawEmailDir, emailID+".eml")
				if err := os.WriteFile(emlPath, rawData, 0o640); err != nil {
					slog.Error("failed to save raw email", "path", emlPath, "error", err)
				}
			}
		}

		matched := engine.Match(email)
		if len(matched) == 0 {
			slog.Info("no rules matched", "from", email.EnvelopeFrom, "subject", email.Subject)
			if store != nil {
				store.UpdateEmailStatus(ctx, emailID, models.EmailStatusDropped)
			}
			if events != nil {
				events.Notify()
			}
			return
		}

		slog.Info("rules matched",
			"count", len(matched),
			"from", email.EnvelopeFrom,
			"subject", email.Subject,
		)

		results := dispatcher.Dispatch(ctx, email, matched)

		if store != nil {
			for _, res := range results {
				delivery := &models.DeliveryRecord{
					EmailID:      emailID,
					RuleName:     res.RuleName,
					Status:       res.Status,
					StatusCode:   res.StatusCode,
					ErrorMessage: res.Error,
					Attempts:     res.Attempts,
				}
				if err := store.SaveDelivery(ctx, delivery); err != nil {
					slog.Error("failed to save delivery record", "error", err)
				}
			}

			newStatus := webui.ComputeStatus(results)
			store.UpdateEmailStatus(ctx, emailID, newStatus)
		}

		if events != nil {
			events.Notify()
		}
	}
}

func buildRejectionHandler(store *storage.Store, events *webui.EventBus, rawEmailDir string) smtpserver.RejectionHandler {
	return func(ctx context.Context, from string, to []string, authResult *models.AuthResult, rawData []byte, reason string) {
		emailID := uuid.New().String()

		// Best-effort parse to extract subject and headers.
		var parsedEmail *models.ParsedEmail
		if email, err := smtpserver.ParseEmail(bytes.NewReader(rawData)); err == nil {
			email.EnvelopeFrom = from
			email.EnvelopeTo = to
			email.AuthResult = authResult
			parsedEmail = email
		} else {
			parsedEmail = &models.ParsedEmail{
				EnvelopeFrom: from,
				EnvelopeTo:   to,
				AuthResult:   authResult,
			}
		}

		record := &models.EmailRecord{
			ID:              emailID,
			ReceivedAt:      time.Now().UTC(),
			EnvelopeFrom:    from,
			EnvelopeTo:      to,
			Subject:         parsedEmail.Subject,
			Status:          models.EmailStatusRejected,
			RejectionReason: reason,
			ParsedEmail:     parsedEmail,
			AuthResult:      authResult,
		}
		if err := store.SaveEmail(ctx, record); err != nil {
			slog.Error("failed to save rejected email record", "error", err)
		}

		if rawEmailDir != "" && len(rawData) > 0 {
			emlPath := filepath.Join(rawEmailDir, emailID+".eml")
			if err := os.WriteFile(emlPath, rawData, 0o640); err != nil {
				slog.Error("failed to save raw email", "path", emlPath, "error", err)
			}
		}

		if events != nil {
			events.Notify()
		}
	}
}

func runCleanup(ctx context.Context, store *storage.Store, retentionDays *atomic.Int32, rawEmailDir string) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			days := int(retentionDays.Load())
			if days <= 0 {
				continue
			}

			var expiredIDs []string
			if rawEmailDir != "" {
				ids, err := store.GetExpiredEmailIDs(ctx, days)
				if err != nil {
					slog.Error("failed to get expired email IDs", "error", err)
				} else {
					expiredIDs = ids
				}
			}

			deleted, err := store.Cleanup(ctx, days)
			if err != nil {
				slog.Error("cleanup failed", "error", err)
			} else if deleted > 0 {
				slog.Info("cleaned up old emails", "deleted", deleted)
			}

			for _, id := range expiredIDs {
				emlPath := filepath.Join(rawEmailDir, id+".eml")
				if err := os.Remove(emlPath); err != nil && !os.IsNotExist(err) {
					slog.Error("failed to remove raw email file", "path", emlPath, "error", err)
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func setupLogger(level string) *slog.LevelVar {
	lv := &slog.LevelVar{}
	setLogLevel(lv, level)
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv})
	slog.SetDefault(slog.New(handler))
	return lv
}

func setLogLevel(lv *slog.LevelVar, level string) {
	switch level {
	case "debug":
		lv.Set(slog.LevelDebug)
	case "warn":
		lv.Set(slog.LevelWarn)
	case "error":
		lv.Set(slog.LevelError)
	default:
		lv.Set(slog.LevelInfo)
	}
}

// withSecurityHeaders wraps an HTTP handler to add common security headers.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		// CSP is kept intentionally permissive to avoid breaking the existing UI
		// while still constraining script/style origins.
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self' 'unsafe-inline' https://unpkg.com; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'self'; base-uri 'self'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

// csrfMiddleware enforces CSRF protection on unsafe HTTP methods for the web UI.
func csrfMiddleware(next http.Handler, httpCfg models.HTTPConfig) http.Handler {
	secure := !strings.HasPrefix(httpCfg.Addr, "127.0.0.1:") && !strings.HasPrefix(httpCfg.Addr, "localhost:")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			// Exempt auth endpoints from CSRF checks; they already employ OAuth state.
			if strings.HasPrefix(r.URL.Path, "/auth/") {
				next.ServeHTTP(w, r)
				return
			}
			// Ensure a token exists for this client and validate it.
			httpcsrf.EnsureToken(w, r, secure)
			if !httpcsrf.Validate(r) {
				http.Error(w, "CSRF token invalid or missing", http.StatusForbidden)
				return
			}
		default:
			// For safe methods, just make sure a token is present so templates can use it.
			httpcsrf.EnsureToken(w, r, secure)
		}

		next.ServeHTTP(w, r)
	})
}


// applySettings merges saved settings into the running config.
// Only hot-reloadable fields are applied.
func applySettings(dst *models.AppConfig, src *models.AppConfig) {
	dst.LogLevel = src.LogLevel
	dst.Auth = src.Auth
	dst.Retry = src.Retry
	dst.WebUI.RetentionDays = src.WebUI.RetentionDays
}
