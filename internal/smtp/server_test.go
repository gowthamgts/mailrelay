package smtp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	netsmtp "net/smtp"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gowthamgts/mailrelay/internal/metrics"
	"github.com/gowthamgts/mailrelay/internal/models"
	"github.com/gowthamgts/mailrelay/internal/storage"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func writeTestCertificate(t *testing.T, certPath, keyPath string, serial int64) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func startTestServer(t *testing.T, cfg models.SMTPConfig) (string, <-chan *models.ParsedEmail) {
	t.Helper()
	received := make(chan *models.ParsedEmail, 2)
	cfg.Domain = "localhost"
	cfg.ReadTimeout = 5 * time.Second
	cfg.WriteTimeout = 5 * time.Second
	srv := NewServer(context.Background(), cfg, models.AuthConfig{SPF: models.AuthModeOff, DKIM: models.AuthModeOff, DMARC: models.AuthModeOff}, func(_ context.Context, email *models.ParsedEmail, _ []byte) error { received <- email; return nil })
	if err := srv.configureTLS(); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.smtp.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		<-done
	})
	return listener.Addr().String(), received
}

func testClient(t *testing.T, addr string) *netsmtp.Client {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	client, err := netsmtp.NewClient(conn, "localhost")
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func TestSTARTTLS(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	cert := writeTestCertificate(t, certPath, keyPath, 1)
	beforeTLS := counterValue(metrics.SMTPSessionsTotal.WithLabelValues("tls"))
	addr, received := startTestServer(t, models.SMTPConfig{TLSCert: certPath, TLSKey: keyPath, RequireTLS: true})
	client := testClient(t, addr)
	if ok, _ := client.Extension("STARTTLS"); !ok {
		t.Fatal("STARTTLS not advertised")
	}
	err := client.Mail("sender@example.com")
	var smtpErr *textproto.Error
	if !errors.As(err, &smtpErr) || smtpErr.Code != 530 {
		t.Fatalf("MAIL before STARTTLS = %v, want 530", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	if err := client.StartTLS(&tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12}); err != nil {
		t.Fatal(err)
	}
	if state, ok := client.TLSConnectionState(); !ok || state.Version < tls.VersionTLS12 {
		t.Fatal("missing TLS state")
	}
	if ok, _ := client.Extension("STARTTLS"); ok {
		t.Fatal("STARTTLS advertised after upgrade")
	}
	sendTestEmail(t, client)
	select {
	case email := <-received:
		if !email.AuthResult.TLS {
			t.Fatal("TLS not recorded")
		}
		store, err := storage.Open(filepath.Join(dir, "emails.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		record := &models.EmailRecord{ParsedEmail: email, AuthResult: email.AuthResult, Status: models.EmailStatusReceived}
		if err := store.SaveEmail(context.Background(), record); err != nil {
			t.Fatal(err)
		}
		saved, err := store.GetEmail(context.Background(), record.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !saved.AuthResult.TLS || !saved.ParsedEmail.AuthResult.TLS {
			t.Fatal("TLS not persisted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("email not received")
	}
	client.Quit()
	// Renew in place, without restarting the listener.
	renewed := writeTestCertificate(t, certPath, keyPath, 2)
	roots = x509.NewCertPool()
	roots.AddCert(renewed)
	client = testClient(t, addr)
	if err := client.StartTLS(&tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12}); err != nil {
		t.Fatal(err)
	}
	if got := counterValue(metrics.SMTPSessionsTotal.WithLabelValues("tls")); got != beforeTLS+2 {
		t.Fatalf("TLS sessions = %v, want %v", got, beforeTLS+2)
	}
	state, _ := client.TLSConnectionState()
	if state.PeerCertificates[0].SerialNumber.Int64() != 2 {
		t.Fatal("certificate was not reloaded")
	}
	client.Quit()
}

func sendTestEmail(t *testing.T, client *netsmtp.Client) {
	t.Helper()
	if err := client.Mail("sender@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := client.Rcpt("recipient@example.com"); err != nil {
		t.Fatal(err)
	}
	writer, err := client.Data()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("From: sender@example.com\r\nTo: recipient@example.com\r\nSubject: transport test\r\n\r\nHello\r\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPlaintextSMTP(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "unconfigured", true: "opportunistic"}[enabled], func(t *testing.T) {
			cfg := models.SMTPConfig{}
			if enabled {
				dir := t.TempDir()
				cfg.TLSCert, cfg.TLSKey = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
				writeTestCertificate(t, cfg.TLSCert, cfg.TLSKey, 1)
			}
			before := counterValue(metrics.SMTPSessionsTotal.WithLabelValues("plaintext"))
			addr, received := startTestServer(t, cfg)
			client := testClient(t, addr)
			if ok, _ := client.Extension("STARTTLS"); ok != enabled {
				t.Fatalf("STARTTLS = %v, want %v", ok, enabled)
			}
			sendTestEmail(t, client)
			select {
			case email := <-received:
				if email.AuthResult.TLS {
					t.Fatal("plaintext email marked TLS")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("email not received")
			}
			client.Quit()
			if got := counterValue(metrics.SMTPSessionsTotal.WithLabelValues("plaintext")); got != before+1 {
				t.Fatalf("sessions = %v, want %v", got, before+1)
			}
		})
	}
}

func TestInvalidTLSFiles(t *testing.T) {
	srv := NewServer(context.Background(), models.SMTPConfig{TLSCert: "missing.pem", TLSKey: "missing.key"}, models.AuthConfig{}, nil)
	defer srv.cancel()
	if err := srv.ListenAndServe(); err == nil {
		t.Fatal("expected startup certificate error")
	}
}

func counterValue(counter prometheus.Counter) float64 {
	var metric dto.Metric
	if err := counter.Write(&metric); err != nil {
		panic(err)
	}
	return metric.GetCounter().GetValue()
}

func TestTLSMinimumVersion(t *testing.T) {
	dir := t.TempDir()
	cfg := models.SMTPConfig{TLSCert: filepath.Join(dir, "cert.pem"), TLSKey: filepath.Join(dir, "key.pem")}
	cert := writeTestCertificate(t, cfg.TLSCert, cfg.TLSKey, 1)
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	addr, _ := startTestServer(t, cfg)
	client := testClient(t, addr)
	if err := client.StartTLS(&tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11}); err == nil {
		t.Fatal("TLS 1.1 handshake unexpectedly succeeded")
	}
	client.Close()
}

// Callers constructing the SMTP server directly must receive the same TLS
// validation as callers using the application config loader.
func TestServerTLSConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cfg       models.SMTPConfig
		wantError string
	}{
		{"cert without key", models.SMTPConfig{TLSCert: "cert.pem"}, "smtp.tls_cert and smtp.tls_key must be set together"},
		{"key without cert", models.SMTPConfig{TLSKey: "key.pem"}, "smtp.tls_cert and smtp.tls_key must be set together"},
		{"required without certificate", models.SMTPConfig{RequireTLS: true}, "smtp.require_tls requires a TLS certificate and key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewServer(context.Background(), tc.cfg, models.AuthConfig{}, nil)
			defer srv.cancel()
			if err := srv.configureTLS(); err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("configureTLS() = %v, want %q", err, tc.wantError)
			}
			if srv.smtp.TLSConfig != nil {
				t.Fatal("invalid config enabled TLS")
			}
		})
	}
}

func TestSTARTTLSInvalidRenewalRecovery(t *testing.T) {
	dir := t.TempDir()
	cfg := models.SMTPConfig{TLSCert: filepath.Join(dir, "cert.pem"), TLSKey: filepath.Join(dir, "key.pem")}
	cert := writeTestCertificate(t, cfg.TLSCert, cfg.TLSKey, 1)
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	addr, _ := startTestServer(t, cfg)
	if err := os.WriteFile(cfg.TLSKey, []byte("invalid renewed key"), 0600); err != nil {
		t.Fatal(err)
	}
	client := testClient(t, addr)
	if err := client.StartTLS(&tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12}); err == nil {
		t.Fatal("handshake succeeded with invalid renewed key")
	}
	client.Close()
	// Restoring a valid pair lets the same listener recover on the next handshake.
	renewed := writeTestCertificate(t, cfg.TLSCert, cfg.TLSKey, 2)
	roots = x509.NewCertPool()
	roots.AddCert(renewed)
	client = testClient(t, addr)
	if err := client.StartTLS(&tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12}); err != nil {
		t.Fatal(err)
	}
	state, _ := client.TLSConnectionState()
	if state.PeerCertificates[0].SerialNumber.Int64() != 2 {
		t.Fatal("listener did not recover with renewed certificate")
	}
	client.Quit()
}
