package smtp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path"
	"strings"
	"sync"
	"time"

	gosmtp "github.com/emersion/go-smtp"
	"github.com/gowthamgts/mailrelay/internal/metrics"
	"github.com/gowthamgts/mailrelay/internal/models"
)

// EmailHandler is called for each successfully received email.
// rawData contains the original RFC 5322 message bytes.
type EmailHandler func(ctx context.Context, email *models.ParsedEmail, rawData []byte)

// RejectionHandler is called when an email is rejected at the SMTP level
// (e.g. SPF/DKIM/DMARC enforcement failure). It allows the rejection to be
// recorded before the 550 error is returned to the sender.
type RejectionHandler func(ctx context.Context, from string, to []string, authResult *models.AuthResult, rawData []byte, reason string)

// Server wraps go-smtp to accept inbound email.
type Server struct {
	smtp             *gosmtp.Server
	cfg              models.SMTPConfig
	authMu           sync.RWMutex
	authCfg          models.AuthConfig
	handler          EmailHandler
	rejectionHandler RejectionHandler

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewServer creates a new SMTP server with a base context for handler lifecycle.
func NewServer(ctx context.Context, cfg models.SMTPConfig, authCfg models.AuthConfig, handler EmailHandler) *Server {
	handlerCtx, cancel := context.WithCancel(ctx)
	s := &Server{
		cfg:     cfg,
		authCfg: authCfg,
		handler: handler,
		ctx:     handlerCtx,
		cancel:  cancel,
	}

	srv := gosmtp.NewServer(s)
	srv.Addr = cfg.Addr
	srv.Domain = cfg.Domain
	srv.MaxMessageBytes = cfg.MaxMessageBytes
	srv.MaxRecipients = cfg.MaxRecipients
	srv.ReadTimeout = cfg.ReadTimeout
	srv.WriteTimeout = cfg.WriteTimeout
	srv.AllowInsecureAuth = true

	s.smtp = srv
	return s
}

// ListenAndServe starts the SMTP server.
func (s *Server) ListenAndServe() error {
	slog.Info("SMTP server starting", "addr", s.cfg.Addr, "domain", s.cfg.Domain)
	return s.smtp.ListenAndServe()
}

// Shutdown gracefully shuts down the SMTP server: stops accepting new
// connections, cancels the handler context, waits for in-flight handlers
// to finish, then closes the underlying server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.cancel()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		slog.Info("all in-flight email handlers finished")
	case <-ctx.Done():
		slog.Warn("timed out waiting for in-flight email handlers")
	}

	return s.smtp.Shutdown(ctx)
}

// NewSession implements the go-smtp Backend interface.
func (s *Server) NewSession(c *gosmtp.Conn) (gosmtp.Session, error) {
	metrics.SMTPConnectionsTotal.Inc()
	return &session{
		server:     s,
		remoteAddr: c.Conn().RemoteAddr(),
	}, nil
}

type session struct {
	server     *Server
	remoteAddr net.Addr
	from       string
	to         []string
}

func (s *session) Reset() {
	s.from = ""
	s.to = nil
}

func (s *session) Logout() error {
	return nil
}

func (s *session) Mail(from string, opts *gosmtp.MailOptions) error {
	s.from = from
	return nil
}

func (s *session) Rcpt(to string, opts *gosmtp.RcptOptions) error {
	if len(s.server.cfg.AllowedRecipients) > 0 {
		if !matchRecipient(to, s.server.cfg.AllowedRecipients) {
			slog.Warn("rejected recipient", "to", to)
			metrics.SMTPRecipientsRejectedTotal.Inc()
			return &gosmtp.SMTPError{
				Code:         550,
				EnhancedCode: gosmtp.EnhancedCode{5, 7, 1},
				Message:      "Recipient not allowed",
			}
		}
	}
	s.to = append(s.to, to)
	return nil
}

func (s *session) Data(r io.Reader) error {
	start := time.Now()

	rawData, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("reading email data: %w", err)
	}

	metrics.SMTPEmailSizeBytes.Observe(float64(len(rawData)))

	authResult, err := VerifyAuth(context.Background(), s.server.getAuthConfig(), s.remoteAddr, s.from, rawData)
	if err != nil {
		slog.Error("auth check enforcement failure", "error", err, "from", s.from)
		metrics.SMTPEmailErrors.WithLabelValues("auth").Inc()
		if s.server.rejectionHandler != nil {
			s.server.rejectionHandler(s.server.ctx, s.from, s.to, authResult, rawData, err.Error())
		}
		return &gosmtp.SMTPError{
			Code:         550,
			EnhancedCode: gosmtp.EnhancedCode{5, 7, 1},
			Message:      err.Error(),
		}
	}

	email, err := ParseEmail(bytes.NewReader(rawData))
	if err != nil {
		slog.Error("failed to parse email", "error", err)
		metrics.SMTPEmailErrors.WithLabelValues("parse").Inc()
		return &gosmtp.SMTPError{
			Code:         550,
			EnhancedCode: gosmtp.EnhancedCode{5, 6, 0},
			Message:      "Failed to parse email",
		}
	}

	email.EnvelopeFrom = s.from
	email.EnvelopeTo = s.to
	email.AuthResult = authResult

	slog.Info("email received",
		"from", s.from,
		"to", s.to,
		"subject", email.Subject,
		"spf", authResult.SPF,
		"dkim", authResult.DKIM,
		"dmarc", authResult.DMARC,
	)

	metrics.SMTPEmailsReceivedTotal.Inc()
	metrics.SMTPEmailProcessingDuration.Observe(time.Since(start).Seconds())

	s.server.wg.Add(1)
	go func() {
		defer s.server.wg.Done()
		s.server.handler(s.server.ctx, email, rawData)
	}()

	return nil
}

// SetRejectionHandler sets a callback invoked when an email is rejected at
// the SMTP level (auth failure). The handler runs synchronously before the
// 550 error is returned to the sender.
func (s *Server) SetRejectionHandler(h RejectionHandler) {
	s.rejectionHandler = h
}

// SetAuthConfig hot-reloads the authentication configuration.
func (s *Server) SetAuthConfig(cfg models.AuthConfig) {
	s.authMu.Lock()
	s.authCfg = cfg
	s.authMu.Unlock()
}

func (s *Server) getAuthConfig() models.AuthConfig {
	s.authMu.RLock()
	defer s.authMu.RUnlock()
	return s.authCfg
}

// matchRecipient checks if a recipient matches any allowed pattern.
func matchRecipient(recipient string, patterns []string) bool {
	recipient = strings.ToLower(recipient)
	for _, pattern := range patterns {
		matched, err := path.Match(strings.ToLower(pattern), recipient)
		if err == nil && matched {
			return true
		}
	}
	return false
}
