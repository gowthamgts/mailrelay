package webui

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	texttemplate "text/template"
	"time"

	"github.com/gowthamgts/mailrelay/internal/httpcsrf"
	"github.com/gowthamgts/mailrelay/internal/models"
	"github.com/gowthamgts/mailrelay/internal/rules"
	smtpserver "github.com/gowthamgts/mailrelay/internal/smtp"
	"github.com/gowthamgts/mailrelay/internal/storage"
	"github.com/gowthamgts/mailrelay/internal/webhook"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// EventBus broadcasts notifications to SSE clients when new emails arrive.
type EventBus struct {
	mu      sync.Mutex
	clients map[chan struct{}]struct{}
}

func NewEventBus() *EventBus {
	return &EventBus{clients: make(map[chan struct{}]struct{})}
}

func (b *EventBus) Subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *EventBus) Unsubscribe(ch chan struct{}) {
	b.mu.Lock()
	delete(b.clients, ch)
	close(ch)
	b.mu.Unlock()
}

func (b *EventBus) Notify() {
	b.mu.Lock()
	for ch := range b.clients {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	b.mu.Unlock()
}

type Handler struct {
	store       *storage.Store
	engine      *rules.Engine
	dispatcher  *webhook.Dispatcher
	Events      *EventBus
	templates   *template.Template
	rawEmailDir string

	cfgMu         sync.RWMutex
	config        *models.AppConfig
	smtpServer    *smtpserver.Server
	logLevel      *slog.LevelVar
	retentionDays *atomic.Int32
}

func NewHandler(store *storage.Store, engine *rules.Engine, dispatcher *webhook.Dispatcher, rawEmailDir string, config *models.AppConfig) (*Handler, error) {
	funcMap := template.FuncMap{
		"sub": func(a, b int) int { return a - b },
		"add": func(a, b int) int { return a + b },
		"timeAgo": func(t time.Time) string {
			d := time.Since(t)
			switch {
			case d < time.Minute:
				return "just now"
			case d < time.Hour:
				m := int(d.Minutes())
				if m == 1 {
					return "1 minute ago"
				}
				return strconv.Itoa(m) + " minutes ago"
			case d < 24*time.Hour:
				h := int(d.Hours())
				if h == 1 {
					return "1 hour ago"
				}
				return strconv.Itoa(h) + " hours ago"
			default:
				days := int(d.Hours() / 24)
				if days == 1 {
					return "1 day ago"
				}
				return strconv.Itoa(days) + " days ago"
			}
		},
		"formatTime": func(t time.Time) string {
			return t.Format("Jan 02, 2006 15:04:05 UTC")
		},
		"statusColor": func(s models.EmailStatus) string {
			switch s {
			case models.EmailStatusDelivered:
				return "green"
			case models.EmailStatusDropped:
				return "amber"
			case models.EmailStatusFailed:
				return "red"
			case models.EmailStatusPartialFailure:
				return "orange"
			case models.EmailStatusRejected:
				return "red"
			case models.EmailStatusReceived:
				return "blue"
			default:
				return "gray"
			}
		},
		"deliveryStatusColor": func(s string) string {
			switch s {
			case "success":
				return "green"
			case "failed":
				return "red"
			case "rejected":
				return "orange"
			case "error":
				return "red"
			case "cancelled":
				return "gray"
			default:
				return "gray"
			}
		},
		"joinStrings": func(s []string) string {
			return strings.Join(s, ", ")
		},
		"pageNumbers": func(current, total int) []int {
			var pages []int
			for i := max(1, current-2); i <= min(total, current+2); i++ {
				pages = append(pages, i)
			}
			return pages
		},
		"statusLabel": func(s models.EmailStatus) string {
			switch s {
			case models.EmailStatusPartialFailure:
				return "Partial Failure"
			default:
				str := string(s)
				if len(str) > 0 {
					return strings.ToUpper(str[:1]) + str[1:]
				}
				return str
			}
		},
		"deliveryStatusLabel": func(s string) string {
			if len(s) > 0 {
				return strings.ToUpper(s[:1]) + s[1:]
			}
			return s
		},
		"attachmentIndex": func(i int) string {
			return strconv.Itoa(i)
		},
		"headersToText": func(h map[string]string) string {
			if len(h) == 0 {
				return ""
			}
			var lines []string
			for k, v := range h {
				lines = append(lines, k+": "+v)
			}
			return strings.Join(lines, "\n")
		},
		"formatBytes": func(b int64) string {
			const (
				kb = 1024
				mb = 1024 * kb
				gb = 1024 * mb
			)
			switch {
			case b >= gb:
				return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
			case b >= mb:
				return fmt.Sprintf("%.1f MB", float64(b)/float64(mb))
			case b >= kb:
				return fmt.Sprintf("%.1f KB", float64(b)/float64(kb))
			default:
				return fmt.Sprintf("%d B", b)
			}
		},
		"formatDuration": func(d time.Duration) string {
			if d >= time.Minute {
				return fmt.Sprintf("%.0fm", d.Minutes())
			}
			return fmt.Sprintf("%.0fs", d.Seconds())
		},
		"durationSeconds": func(d time.Duration) int {
			return int(d.Seconds())
		},
		"dict": func(pairs ...any) map[string]any {
			m := make(map[string]any, len(pairs)/2)
			for i := 0; i+1 < len(pairs); i += 2 {
				key, _ := pairs[i].(string)
				m[key] = pairs[i+1]
			}
			return m
		},
	}

	tmpl, err := template.New("").Funcs(funcMap).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}

	return &Handler{
		store:       store,
		engine:      engine,
		dispatcher:  dispatcher,
		Events:      NewEventBus(),
		templates:   tmpl,
		rawEmailDir: rawEmailDir,
		config:      config,
	}, nil
}

// LoadRules loads enabled rules from the database into the engine.
// Call this at startup after the handler is created.
func (h *Handler) LoadRules(ctx context.Context) error {
	return h.reloadRules(ctx)
}

// SetSMTPServer sets the SMTP server reference for hot-reloading auth config.
func (h *Handler) SetSMTPServer(srv *smtpserver.Server) {
	h.smtpServer = srv
}

// SetLogLevel sets the log level variable for runtime changes.
func (h *Handler) SetLogLevel(lv *slog.LevelVar) {
	h.logLevel = lv
}

// SetRetentionDays sets the shared retention days atomic for the cleanup goroutine.
func (h *Handler) SetRetentionDays(days *atomic.Int32) {
	h.retentionDays = days
}

// cacheControl wraps an http.Handler and sets Cache-Control headers for static assets.
func cacheControl(h http.Handler, maxAge int) http.Handler {
	val := fmt.Sprintf("public, max-age=%d", maxAge)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", val)
		h.ServeHTTP(w, r)
	})
}

func (h *Handler) Register(mux *http.ServeMux) {
	// Cache static assets for 7 days — they're embedded at build time and only change on redeploy.
	mux.Handle("GET /static/", cacheControl(http.StripPrefix("/", http.FileServerFS(staticFS)), 604800))
	mux.HandleFunc("GET /{$}", h.handleList)
	mux.HandleFunc("GET /emails/{id}", h.handleDetail)
	mux.HandleFunc("POST /emails/{id}/replay", h.handleReplay)
	mux.HandleFunc("DELETE /emails/{id}", h.handleDelete)
	mux.HandleFunc("GET /emails/{id}/raw", h.handleRaw)
	mux.HandleFunc("GET /emails/{id}/attachments/{index}", h.handleAttachment)
	mux.HandleFunc("POST /emails/delete-all", h.handleDeleteAll)
	mux.HandleFunc("GET /api/stats", h.handleStats)
	mux.HandleFunc("GET /api/stats-html", h.handleStatsHTML)
	mux.HandleFunc("GET /api/events", h.handleSSE)
	mux.HandleFunc("GET /settings", h.handleSettings)
	mux.HandleFunc("POST /settings", h.handleSettingsSave)
	mux.HandleFunc("GET /rules", h.handleRulesList)
	mux.HandleFunc("POST /rules", h.handleRuleCreate)
	mux.HandleFunc("GET /rules/{id}/edit", h.handleRuleEditForm)
	mux.HandleFunc("POST /rules/{id}", h.handleRuleUpdate)
	mux.HandleFunc("DELETE /rules/{id}", h.handleRuleDelete)
	mux.HandleFunc("POST /rules/{id}/toggle", h.handleRuleToggle)
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	perPage := 10

	filter := storage.EmailFilter{
		Status: r.URL.Query().Get("status"),
		Search: r.URL.Query().Get("q"),
		Limit:  perPage,
		Offset: (page - 1) * perPage,
	}

	result, err := h.store.ListEmails(r.Context(), filter)
	if err != nil {
		slog.Error("failed to list emails", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	stats, err := h.store.Stats(r.Context())
	if err != nil {
		slog.Error("failed to get stats", "error", err)
		stats = &storage.Stats{}
	}

	totalPages := int(math.Ceil(float64(result.Total) / float64(perPage)))
	if totalPages < 1 {
		totalPages = 1
	}

	data := map[string]any{
		"Emails":     result.Emails,
		"Total":      result.Total,
		"Page":       page,
		"TotalPages": totalPages,
		"PerPage":    perPage,
		"Filter":     filter,
		"Stats":      stats,
		"CSRFToken":  h.csrfToken(w, r),
	}

	isHTMX := r.Header.Get("HX-Request") == "true"
	if isHTMX {
		h.render(w, "list_partial", data)
	} else {
		h.render(w, "list", data)
	}
}

func (h *Handler) handleDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	email, err := h.store.GetEmail(r.Context(), id)
	if err != nil {
		slog.Error("failed to get email", "id", id, "error", err)
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	deliveries, err := h.store.GetDeliveriesForEmail(r.Context(), id)
	if err != nil {
		slog.Error("failed to get deliveries", "id", id, "error", err)
		deliveries = nil
	}

	data := map[string]any{
		"Email":       email,
		"Deliveries":  deliveries,
		"HasRawEmail": h.hasRawEmail(id),
		"CSRFToken":   h.csrfToken(w, r),
	}

	if r.Header.Get("HX-Request") == "true" {
		h.render(w, "detail_modal", data)
	} else {
		http.Redirect(w, r, "/", http.StatusFound)
	}
}

func (h *Handler) handleReplay(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	email, err := h.store.GetEmail(r.Context(), id)
	if err != nil || email.ParsedEmail == nil {
		slog.Error("failed to get email for replay", "id", id, "error", err)
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	matched := h.engine.Match(email.ParsedEmail)
	if len(matched) == 0 {
		h.store.UpdateEmailStatus(r.Context(), id, models.EmailStatusDropped)
		w.Header().Set("HX-Trigger", "refreshList")
		h.handleDetail(w, r)
		return
	}

	results := h.dispatcher.Dispatch(r.Context(), email.ParsedEmail, matched)

	for _, res := range results {
		delivery := &models.DeliveryRecord{
			EmailID:      id,
			RuleName:     res.RuleName,
			Status:       res.Status,
			StatusCode:   res.StatusCode,
			ErrorMessage: res.Error,
			ResponseBody: res.ResponseBody,
			Attempts:     res.Attempts,
		}
		if err := h.store.SaveDelivery(r.Context(), delivery); err != nil {
			slog.Error("failed to save replay delivery", "error", err)
		}
	}

	newStatus := computeStatus(results)
	h.store.UpdateEmailStatus(r.Context(), id, newStatus)

	h.Events.Notify()
	w.Header().Set("HX-Trigger", "refreshList")
	h.handleDetail(w, r)
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	if err := h.store.DeleteEmail(r.Context(), id); err != nil {
		slog.Error("failed to delete email", "id", id, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	h.removeRawEmail(id)

	h.Events.Notify()
	w.Header().Set("HX-Trigger", "emailDeleted")
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) handleDeleteAll(w http.ResponseWriter, r *http.Request) {
	if h.rawEmailDir != "" {
		ids, err := h.store.AllEmailIDs(r.Context())
		if err != nil {
			slog.Error("failed to list email IDs for delete-all", "error", err)
		} else {
			for _, id := range ids {
				h.removeRawEmail(id)
			}
		}
	}

	deleted, err := h.store.DeleteAllEmails(r.Context())
	if err != nil {
		slog.Error("failed to delete all emails", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	slog.Info("deleted all emails", "count", deleted)
	h.Events.Notify()
	w.Header().Set("HX-Trigger", "emailDeleted")
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) handleRaw(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" || h.rawEmailDir == "" {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	emlPath := filepath.Join(h.rawEmailDir, id+".eml")
	data, err := os.ReadFile(emlPath)
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Type", "message/rfc822")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.eml"`, id))
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.Write(data)
}

func (h *Handler) handleAttachment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	indexStr := r.PathValue("index")
	if id == "" || indexStr == "" {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	idx, err := strconv.Atoi(indexStr)
	if err != nil || idx < 0 {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	email, err := h.store.GetEmail(r.Context(), id)
	if err != nil || email.ParsedEmail == nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	if idx >= len(email.ParsedEmail.Attachments) {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	att := email.ParsedEmail.Attachments[idx]
	decoded, err := base64.StdEncoding.DecodeString(att.Content)
	if err != nil {
		slog.Error("failed to decode attachment", "id", id, "index", idx, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	filename := att.Filename
	if filename == "" {
		filename = fmt.Sprintf("attachment-%d", idx)
	}

	w.Header().Set("Content-Type", att.ContentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Write(decoded)
}

func (h *Handler) hasRawEmail(id string) bool {
	if h.rawEmailDir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(h.rawEmailDir, id+".eml"))
	return err == nil
}

func (h *Handler) removeRawEmail(id string) {
	if h.rawEmailDir == "" {
		return
	}
	emlPath := filepath.Join(h.rawEmailDir, id+".eml")
	if err := os.Remove(emlPath); err != nil && !os.IsNotExist(err) {
		slog.Error("failed to remove raw email file", "path", emlPath, "error", err)
	}
}

func (h *Handler) handleStats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.store.Stats(r.Context())
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

func (h *Handler) handleStatsHTML(w http.ResponseWriter, r *http.Request) {
	stats, err := h.store.Stats(r.Context())
	if err != nil {
		slog.Error("failed to get stats", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	h.render(w, "stats_cards", stats)
}

func (h *Handler) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := h.Events.Subscribe()
	defer h.Events.Unsubscribe(ch)

	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-ch:
			fmt.Fprintf(w, "event: refresh\ndata: {}\n\n")
			flusher.Flush()
		case <-ping.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// reloadRules fetches enabled rules from the DB and updates the engine.
func (h *Handler) reloadRules(ctx context.Context) error {
	records, err := h.store.ListEnabledRules(ctx)
	if err != nil {
		return err
	}
	rules := make([]models.Rule, len(records))
	for i, r := range records {
		rules[i] = r.ToRule()
	}
	h.engine.SetRules(rules)
	return nil
}

func (h *Handler) handleSettings(w http.ResponseWriter, r *http.Request) {
	h.cfgMu.RLock()
	cfg := *h.config
	h.cfgMu.RUnlock()

	data := map[string]any{
		"Config":    &cfg,
		"Saved":     r.URL.Query().Get("saved") == "1",
		"CSRFToken": h.csrfToken(w, r),
	}
	h.render(w, "settings", data)
}

func (h *Handler) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	// Validate log level
	logLevel := r.FormValue("log_level")
	switch logLevel {
	case "debug", "info", "warn", "error":
	default:
		h.renderSettingsWithError(w, r, "Invalid log level.")
		return
	}

	// Validate auth modes
	authFields := map[string]*models.AuthMode{
		"auth_spf":   new(models.AuthMode),
		"auth_dkim":  new(models.AuthMode),
		"auth_dmarc": new(models.AuthMode),
	}
	for field, mode := range authFields {
		val := models.AuthMode(r.FormValue(field))
		switch val {
		case models.AuthModeOff, models.AuthModeLog, models.AuthModeEnforce:
			*mode = val
		default:
			h.renderSettingsWithError(w, r, fmt.Sprintf("Invalid auth mode for %s.", field))
			return
		}
	}

	// Validate retry config
	maxRetries, err := strconv.Atoi(r.FormValue("retry_max_retries"))
	if err != nil || maxRetries < 0 {
		h.renderSettingsWithError(w, r, "Max retries must be a non-negative number.")
		return
	}
	initialWaitSec, err := strconv.Atoi(r.FormValue("retry_initial_wait_seconds"))
	if err != nil || initialWaitSec < 1 {
		h.renderSettingsWithError(w, r, "Initial wait must be at least 1 second.")
		return
	}
	maxWaitSec, err := strconv.Atoi(r.FormValue("retry_max_wait_seconds"))
	if err != nil || maxWaitSec < 1 {
		h.renderSettingsWithError(w, r, "Max wait must be at least 1 second.")
		return
	}
	if initialWaitSec > maxWaitSec {
		h.renderSettingsWithError(w, r, "Initial wait cannot exceed max wait.")
		return
	}

	// Validate retention days
	retentionDays, err := strconv.Atoi(r.FormValue("retention_days"))
	if err != nil || retentionDays < 0 {
		h.renderSettingsWithError(w, r, "Retention days must be a non-negative number.")
		return
	}

	// Build updated config (only hot-reloadable fields)
	h.cfgMu.Lock()
	h.config.LogLevel = logLevel
	h.config.Auth.SPF = *authFields["auth_spf"]
	h.config.Auth.DKIM = *authFields["auth_dkim"]
	h.config.Auth.DMARC = *authFields["auth_dmarc"]
	h.config.Retry.MaxRetries = maxRetries
	h.config.Retry.InitialWait = time.Duration(initialWaitSec) * time.Second
	h.config.Retry.MaxWait = time.Duration(maxWaitSec) * time.Second
	h.config.WebUI.RetentionDays = retentionDays

	// Snapshot for persistence
	cfgCopy := *h.config
	h.cfgMu.Unlock()

	// Persist to database
	if err := h.store.SaveSettings(r.Context(), &cfgCopy); err != nil {
		slog.Error("failed to save settings", "error", err)
		h.renderSettingsWithError(w, r, "Failed to save settings.")
		return
	}

	// Apply runtime changes
	if h.logLevel != nil {
		switch logLevel {
		case "debug":
			h.logLevel.Set(slog.LevelDebug)
		case "warn":
			h.logLevel.Set(slog.LevelWarn)
		case "error":
			h.logLevel.Set(slog.LevelError)
		default:
			h.logLevel.Set(slog.LevelInfo)
		}
	}

	if h.smtpServer != nil {
		h.smtpServer.SetAuthConfig(cfgCopy.Auth)
	}

	h.dispatcher.SetRetryConfig(cfgCopy.Retry)

	if h.retentionDays != nil {
		h.retentionDays.Store(int32(retentionDays))
	}

	slog.Info("settings updated via web UI")
	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
}

func (h *Handler) renderSettingsWithError(w http.ResponseWriter, r *http.Request, errMsg string) {
	h.cfgMu.RLock()
	cfg := *h.config
	h.cfgMu.RUnlock()

	data := map[string]any{
		"Config":    &cfg,
		"Error":     errMsg,
		"CSRFToken": h.csrfToken(w, r),
	}
	w.WriteHeader(http.StatusUnprocessableEntity)
	h.render(w, "settings", data)
}

func (h *Handler) handleRulesList(w http.ResponseWriter, r *http.Request) {
	dbRules, err := h.store.ListRules(r.Context())
	if err != nil {
		slog.Error("failed to list rules", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	data := map[string]any{
		"Rules":     dbRules,
		"CSRFToken": h.csrfToken(w, r),
	}
	h.render(w, "rules", data)
}

func (h *Handler) handleRuleCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	rule, errMsg := parseRuleForm(r)
	if errMsg != "" {
		h.renderRulesWithError(w, r, errMsg, rule, false)
		return
	}

	pos, _ := h.store.NextRulePosition(r.Context())
	rule.Position = pos
	rule.Enabled = true

	if err := h.store.SaveRule(r.Context(), rule); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			h.renderRulesWithError(w, r, "A rule with this name already exists.", rule, false)
			return
		}
		slog.Error("failed to save rule", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if err := h.reloadRules(r.Context()); err != nil {
		slog.Error("failed to reload rules", "error", err)
	}

	http.Redirect(w, r, "/rules", http.StatusSeeOther)
}

func (h *Handler) handleRuleEditForm(w http.ResponseWriter, r *http.Request) {
	// Edit is now handled client-side via modals; redirect to the rules list.
	http.Redirect(w, r, "/rules", http.StatusSeeOther)
}

func (h *Handler) handleRuleUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	existing, err := h.store.GetRule(r.Context(), id)
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	rule, errMsg := parseRuleForm(r)
	if errMsg != "" {
		rule.ID = id
		h.renderRulesWithError(w, r, errMsg, rule, true)
		return
	}

	existing.Name = rule.Name
	existing.Match = rule.Match
	existing.Webhook = rule.Webhook

	if err := h.store.UpdateRule(r.Context(), existing); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			rule.ID = id
			h.renderRulesWithError(w, r, "A rule with this name already exists.", rule, true)
			return
		}
		slog.Error("failed to update rule", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if err := h.reloadRules(r.Context()); err != nil {
		slog.Error("failed to reload rules", "error", err)
	}

	http.Redirect(w, r, "/rules", http.StatusSeeOther)
}

func (h *Handler) handleRuleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.store.DeleteRule(r.Context(), id); err != nil {
		slog.Error("failed to delete rule", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if err := h.reloadRules(r.Context()); err != nil {
		slog.Error("failed to reload rules", "error", err)
	}

	w.Header().Set("HX-Redirect", "/rules")
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) handleRuleToggle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rule, err := h.store.GetRule(r.Context(), id)
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	rule.Enabled = !rule.Enabled
	if err := h.store.UpdateRule(r.Context(), rule); err != nil {
		slog.Error("failed to toggle rule", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if err := h.reloadRules(r.Context()); err != nil {
		slog.Error("failed to reload rules", "error", err)
	}

	http.Redirect(w, r, "/rules", http.StatusSeeOther)
}

func parseRuleForm(r *http.Request) (*models.RuleRecord, string) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		return nil, "Rule name is required."
	}

	webhookURL := strings.TrimSpace(r.FormValue("webhook_url"))
	if webhookURL == "" {
		return nil, "Webhook URL is required."
	}

	method := strings.TrimSpace(r.FormValue("webhook_method"))
	if method == "" {
		method = "POST"
	}

	payloadTemplate := r.FormValue("payload_template")
	if strings.TrimSpace(payloadTemplate) != "" {
		if _, err := texttemplate.New("validate").Parse(payloadTemplate); err != nil {
			return nil, fmt.Sprintf("Invalid payload template: %v", err)
		}
	}

	headersRaw := strings.TrimSpace(r.FormValue("webhook_headers"))
	var headers map[string]string
	if headersRaw != "" {
		headers = make(map[string]string)
		for _, line := range strings.Split(headersRaw, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, ":", 2)
			if len(parts) != 2 {
				return nil, fmt.Sprintf("Invalid header format: %q (expected Key: Value)", line)
			}
			headers[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}

	rule := &models.RuleRecord{
		Name: name,
		Match: models.MatcherConfig{
			ToEmail:    strings.TrimSpace(r.FormValue("match_to_email")),
			FromEmail:  strings.TrimSpace(r.FormValue("match_from_email")),
			Subject:    strings.TrimSpace(r.FormValue("match_subject")),
			FromDomain: strings.TrimSpace(r.FormValue("match_from_domain")),
			ToDomain:   strings.TrimSpace(r.FormValue("match_to_domain")),
		},
		Webhook: models.WebhookConfig{
			URL:             webhookURL,
			Method:          method,
			Headers:         headers,
			PayloadTemplate: payloadTemplate,
		},
	}

	return rule, ""
}

func (h *Handler) renderRulesWithError(w http.ResponseWriter, r *http.Request, errMsg string, rule *models.RuleRecord, isEdit bool) {
	dbRules, _ := h.store.ListRules(r.Context())

	data := map[string]any{
		"Rules":     dbRules,
		"Error":     errMsg,
		"FormRule":  rule,
		"CSRFToken": h.csrfToken(w, r),
	}
	if isEdit {
		data["EditRule"] = rule
	}

	w.WriteHeader(http.StatusUnprocessableEntity)
	h.render(w, "rules", data)
}

func (h *Handler) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.templates.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("failed to render template", "name", name, "error", err)
	}
}

func (h *Handler) csrfToken(w http.ResponseWriter, r *http.Request) string {
	h.cfgMu.RLock()
	httpCfg := h.config.HTTP
	h.cfgMu.RUnlock()

	secure := !strings.HasPrefix(httpCfg.Addr, "127.0.0.1:") && !strings.HasPrefix(httpCfg.Addr, "localhost:")
	return httpcsrf.EnsureToken(w, r, secure)
}

// ComputeStatus determines the aggregate email status from delivery results.
func ComputeStatus(results []models.DeliveryResult) models.EmailStatus {
	return computeStatus(results)
}

func computeStatus(results []models.DeliveryResult) models.EmailStatus {
	if len(results) == 0 {
		return models.EmailStatusDropped
	}
	successes := 0
	for _, r := range results {
		if r.Status == "success" {
			successes++
		}
	}
	switch {
	case successes == len(results):
		return models.EmailStatusDelivered
	case successes > 0:
		return models.EmailStatusPartialFailure
	default:
		return models.EmailStatusFailed
	}
}
