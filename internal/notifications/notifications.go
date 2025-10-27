package notifications

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/RouXx67/PulseUP/internal/alerts"
	"github.com/rs/zerolog/log"
)

// Webhook configuration constants
const (
	// HTTP client settings
	WebhookTimeout         = 30 * time.Second
	WebhookMaxResponseSize = 1 * 1024 * 1024 // 1 MB max response size
	WebhookMaxRedirects    = 3               // Maximum number of redirects to follow
	WebhookTestTimeout     = 10 * time.Second

	// Retry settings
	WebhookInitialBackoff = 1 * time.Second
	WebhookMaxBackoff     = 30 * time.Second
	WebhookDefaultRetries = 3

	// History settings
	WebhookHistoryMaxSize = 100

	// Rate limiting settings
	WebhookRateLimitWindow = 1 * time.Minute // Time window for rate limiting
	WebhookRateLimitMax    = 10              // Max requests per window per webhook
)

// createSecureWebhookClient creates an HTTP client with security controls
func createSecureWebhookClient(timeout time.Duration) *http.Client {
	redirectCount := 0
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Limit number of redirects to prevent redirect loops
			if len(via) >= WebhookMaxRedirects {
				return fmt.Errorf("stopped after %d redirects", WebhookMaxRedirects)
			}

			redirectCount++
			newURL := req.URL.String()

			// Prevent redirects to localhost or private networks (SSRF protection)
			if err := ValidateWebhookURL(newURL); err != nil {
				log.Warn().
					Str("original", via[0].URL.String()).
					Str("redirect", newURL).
					Err(err).
					Msg("Blocked webhook redirect to unsafe URL")
				return fmt.Errorf("redirect to unsafe URL blocked: %w", err)
			}

			log.Debug().
				Str("from", via[len(via)-1].URL.String()).
				Str("to", newURL).
				Int("redirectCount", redirectCount).
				Msg("Following webhook redirect")

			return nil
		},
	}
}

// TestNodeInfo contains information about nodes for test notifications
type TestNodeInfo struct {
	NodeName    string
	InstanceURL string
}

// WebhookDelivery tracks webhook delivery attempts for debugging
type WebhookDelivery struct {
	WebhookName   string    `json:"webhookName"`
	WebhookURL    string    `json:"webhookUrl"`
	Service       string    `json:"service"`
	AlertID       string    `json:"alertId"`
	Timestamp     time.Time `json:"timestamp"`
	StatusCode    int       `json:"statusCode"`
	Success       bool      `json:"success"`
	ErrorMessage  string    `json:"errorMessage,omitempty"`
	RetryAttempts int       `json:"retryAttempts"`
	PayloadSize   int       `json:"payloadSize"`
}

// webhookRateLimit tracks rate limiting for webhook deliveries
type webhookRateLimit struct {
	lastSent  time.Time
	sentCount int
}

// NotificationManager handles sending notifications
type NotificationManager struct {
	mu                sync.RWMutex
	emailConfig       EmailConfig
	webhooks          []WebhookConfig
	appriseConfig     AppriseConfig
	enabled           bool
	cooldown          time.Duration
	lastNotified      map[string]notificationRecord
	groupWindow       time.Duration
	pendingAlerts     []*alerts.Alert
	groupTimer        *time.Timer
	groupByNode       bool
	publicURL         string // Full URL to access Pulse
	groupByGuest      bool
	webhookHistory    []WebhookDelivery            // Keep last 100 webhook deliveries for debugging
	webhookRateLimits map[string]*webhookRateLimit // Track rate limits per webhook URL
	appriseExec       appriseExecFunc
}

type appriseExecFunc func(ctx context.Context, path string, args []string) ([]byte, error)

// copyEmailConfig returns a defensive copy of EmailConfig including its slices to avoid data races.
func copyEmailConfig(cfg EmailConfig) EmailConfig {
	copy := cfg
	if len(cfg.To) > 0 {
		copy.To = append([]string(nil), cfg.To...)
	}
	return copy
}

// copyWebhookConfigs deep-copies webhook configurations to isolate concurrent writers from background senders.
func copyWebhookConfigs(webhooks []WebhookConfig) []WebhookConfig {
	if len(webhooks) == 0 {
		return nil
	}

	copies := make([]WebhookConfig, 0, len(webhooks))
	for _, webhook := range webhooks {
		clone := webhook
		if len(webhook.Headers) > 0 {
			headers := make(map[string]string, len(webhook.Headers))
			for k, v := range webhook.Headers {
				headers[k] = v
			}
			clone.Headers = headers
		}
		if len(webhook.CustomFields) > 0 {
			custom := make(map[string]string, len(webhook.CustomFields))
			for k, v := range webhook.CustomFields {
				custom[k] = v
			}
			clone.CustomFields = custom
		}
		copies = append(copies, clone)
	}

	return copies
}

func copyAppriseConfig(cfg AppriseConfig) AppriseConfig {
	copy := cfg
	if len(cfg.Targets) > 0 {
		copy.Targets = append([]string(nil), cfg.Targets...)
	}
	return copy
}

// NormalizeAppriseConfig cleans and normalizes Apprise configuration values.
func NormalizeAppriseConfig(cfg AppriseConfig) AppriseConfig {
	normalized := cfg

	mode := strings.ToLower(strings.TrimSpace(string(normalized.Mode)))
	switch mode {
	case string(AppriseModeHTTP):
		normalized.Mode = AppriseModeHTTP
	default:
		normalized.Mode = AppriseModeCLI
	}

	normalized.CLIPath = strings.TrimSpace(normalized.CLIPath)
	if normalized.CLIPath == "" {
		normalized.CLIPath = "apprise"
	}

	if normalized.TimeoutSeconds <= 0 {
		normalized.TimeoutSeconds = 15
	} else if normalized.TimeoutSeconds > 120 {
		normalized.TimeoutSeconds = 120
	} else if normalized.TimeoutSeconds < 5 {
		normalized.TimeoutSeconds = 5
	}

	cleanTargets := make([]string, 0, len(normalized.Targets))
	seen := make(map[string]struct{}, len(normalized.Targets))
	for _, target := range normalized.Targets {
		trimmed := strings.TrimSpace(target)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		if _, exists := seen[lower]; exists {
			continue
		}
		seen[lower] = struct{}{}
		cleanTargets = append(cleanTargets, trimmed)
	}
	normalized.Targets = cleanTargets

	normalized.ServerURL = strings.TrimSpace(normalized.ServerURL)
	normalized.ServerURL = strings.TrimRight(normalized.ServerURL, "/")

	normalized.ConfigKey = strings.TrimSpace(normalized.ConfigKey)

	normalized.APIKey = strings.TrimSpace(normalized.APIKey)
	normalized.APIKeyHeader = strings.TrimSpace(normalized.APIKeyHeader)
	if normalized.APIKeyHeader == "" {
		normalized.APIKeyHeader = "X-API-KEY"
	}

	switch normalized.Mode {
	case AppriseModeCLI:
		if len(normalized.Targets) == 0 {
			normalized.Enabled = false
		}
	case AppriseModeHTTP:
		if normalized.ServerURL == "" {
			normalized.Enabled = false
		}
	}

	return normalized
}

func defaultAppriseExec(ctx context.Context, path string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	return cmd.CombinedOutput()
}

type notificationRecord struct {
	lastSent   time.Time
	alertStart time.Time
}

// Alert represents an alert (interface to avoid circular dependency)
type Alert interface {
	GetID() string
	GetResourceName() string
	GetType() string
	GetLevel() string
	GetValue() float64
	GetThreshold() float64
	GetMessage() string
	GetNode() string
	GetInstance() string
	GetStartTime() time.Time
}

// EmailConfig holds email notification settings
type EmailConfig struct {
	Enabled  bool     `json:"enabled"`
	Provider string   `json:"provider"` // Email provider name (Gmail, SendGrid, etc.)
	SMTPHost string   `json:"server"`   // Changed from smtpHost to server for frontend consistency
	SMTPPort int      `json:"port"`     // Changed from smtpPort to port for frontend consistency
	Username string   `json:"username"`
	Password string   `json:"password"`
	From     string   `json:"from"`
	To       []string `json:"to"`
	TLS      bool     `json:"tls"`
	StartTLS bool     `json:"startTLS"` // STARTTLS support
}

// WebhookConfig holds webhook settings
type WebhookConfig struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	URL          string            `json:"url"`
	Method       string            `json:"method"`
	Headers      map[string]string `json:"headers"`
	Enabled      bool              `json:"enabled"`
	Service      string            `json:"service"`  // discord, slack, teams, etc.
	Template     string            `json:"template"` // Custom payload template
	CustomFields map[string]string `json:"customFields,omitempty"`
}

// AppriseMode identifies how Pulse should deliver notifications through Apprise.
type AppriseMode string

const (
	AppriseModeCLI  AppriseMode = "cli"
	AppriseModeHTTP AppriseMode = "http"
)

// AppriseConfig holds Apprise notification settings.
type AppriseConfig struct {
	Enabled        bool        `json:"enabled"`
	Mode           AppriseMode `json:"mode,omitempty"`
	Targets        []string    `json:"targets"`
	CLIPath        string      `json:"cliPath,omitempty"`
	TimeoutSeconds int         `json:"timeoutSeconds,omitempty"`
	ServerURL      string      `json:"serverUrl,omitempty"`
	ConfigKey      string      `json:"configKey,omitempty"`
	APIKey         string      `json:"apiKey,omitempty"`
	APIKeyHeader   string      `json:"apiKeyHeader,omitempty"`
	SkipTLSVerify  bool        `json:"skipTlsVerify,omitempty"`
}

// NewNotificationManager creates a new notification manager
func NewNotificationManager(publicURL string) *NotificationManager {
	cleanURL := strings.TrimRight(strings.TrimSpace(publicURL), "/")
	if cleanURL != "" {
		log.Info().Str("publicURL", cleanURL).Msg("NotificationManager initialized with public URL")
	} else {
		log.Info().Msg("NotificationManager initialized without public URL - webhook links may not work")
	}
	return &NotificationManager{
		enabled:      true,
		cooldown:     5 * time.Minute,
		lastNotified: make(map[string]notificationRecord),
		webhooks:     []WebhookConfig{},
		appriseConfig: AppriseConfig{
			Enabled:        false,
			Mode:           AppriseModeCLI,
			Targets:        []string{},
			CLIPath:        "apprise",
			TimeoutSeconds: 15,
			APIKeyHeader:   "X-API-KEY",
		},
		groupWindow:       30 * time.Second,
		pendingAlerts:     make([]*alerts.Alert, 0),
		groupByNode:       true,
		groupByGuest:      false,
		webhookHistory:    make([]WebhookDelivery, 0, WebhookHistoryMaxSize),
		webhookRateLimits: make(map[string]*webhookRateLimit),
		publicURL:         cleanURL,
		appriseExec:       defaultAppriseExec,
	}
}

// SetPublicURL updates the public URL used for webhook payloads.
func (n *NotificationManager) SetPublicURL(publicURL string) {
	trimmed := strings.TrimRight(strings.TrimSpace(publicURL), "/")
	if trimmed == "" {
		return
	}

	n.mu.Lock()
	if n.publicURL == trimmed {
		n.mu.Unlock()
		return
	}
	n.publicURL = trimmed
	n.mu.Unlock()

	log.Info().Str("publicURL", trimmed).Msg("NotificationManager public URL updated")
}

// GetPublicURL returns the configured public URL for notifications.
func (n *NotificationManager) GetPublicURL() string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.publicURL
}

// SetEmailConfig updates email configuration
func (n *NotificationManager) SetEmailConfig(config EmailConfig) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.emailConfig = config
}

// SetAppriseConfig updates Apprise configuration.
func (n *NotificationManager) SetAppriseConfig(config AppriseConfig) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.appriseConfig = NormalizeAppriseConfig(config)
}

// GetAppriseConfig returns a copy of the Apprise configuration.
func (n *NotificationManager) GetAppriseConfig() AppriseConfig {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return copyAppriseConfig(n.appriseConfig)
}

// SetCooldown updates the cooldown duration
func (n *NotificationManager) SetCooldown(minutes int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.cooldown = time.Duration(minutes) * time.Minute
	log.Info().Int("minutes", minutes).Msg("Updated notification cooldown")
}

// SetGroupingWindow updates the grouping window duration
func (n *NotificationManager) SetGroupingWindow(seconds int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.groupWindow = time.Duration(seconds) * time.Second
	log.Info().Int("seconds", seconds).Msg("Updated notification grouping window")
}

// SetGroupingOptions updates grouping options
func (n *NotificationManager) SetGroupingOptions(byNode, byGuest bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.groupByNode = byNode
	n.groupByGuest = byGuest
	log.Info().Bool("byNode", byNode).Bool("byGuest", byGuest).Msg("Updated notification grouping options")
}

// AddWebhook adds a webhook configuration
func (n *NotificationManager) AddWebhook(webhook WebhookConfig) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.webhooks = append(n.webhooks, webhook)
}

// UpdateWebhook updates an existing webhook
func (n *NotificationManager) UpdateWebhook(id string, webhook WebhookConfig) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	for i, w := range n.webhooks {
		if w.ID == id {
			n.webhooks[i] = webhook
			return nil
		}
	}
	return fmt.Errorf("webhook not found: %s", id)
}

// DeleteWebhook removes a webhook
func (n *NotificationManager) DeleteWebhook(id string) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	for i, w := range n.webhooks {
		if w.ID == id {
			n.webhooks = append(n.webhooks[:i], n.webhooks[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("webhook not found: %s", id)
}

// GetWebhooks returns all webhook configurations
func (n *NotificationManager) GetWebhooks() []WebhookConfig {
	n.mu.RLock()
	defer n.mu.RUnlock()

	if len(n.webhooks) == 0 {
		return []WebhookConfig{}
	}

	webhooks := make([]WebhookConfig, len(n.webhooks))
	copy(webhooks, n.webhooks)
	return webhooks
}

// GetEmailConfig returns the email configuration
func (n *NotificationManager) GetEmailConfig() EmailConfig {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.emailConfig
}

// SendAlert sends notifications for an alert
func (n *NotificationManager) SendAlert(alert *alerts.Alert) {
	n.mu.Lock()
	defer n.mu.Unlock()

	log.Info().
		Str("alertID", alert.ID).
		Bool("enabled", n.enabled).
		Int("webhooks", len(n.webhooks)).
		Bool("emailEnabled", n.emailConfig.Enabled).
		Msg("SendAlert called")

	if !n.enabled {
		log.Debug().Msg("Notifications disabled, skipping")
		return
	}

	// Check cooldown
	record, exists := n.lastNotified[alert.ID]
	if exists && record.alertStart.Equal(alert.StartTime) && time.Since(record.lastSent) < n.cooldown {
		log.Info().
			Str("alertID", alert.ID).
			Str("resourceName", alert.ResourceName).
			Str("type", alert.Type).
			Dur("timeSince", time.Since(record.lastSent)).
			Dur("cooldown", n.cooldown).
			Dur("remainingCooldown", n.cooldown-time.Since(record.lastSent)).
			Msg("Alert notification in cooldown for active alert - notification suppressed")
		return
	}

	log.Info().
		Str("alertID", alert.ID).
		Str("resourceName", alert.ResourceName).
		Str("type", alert.Type).
		Float64("value", alert.Value).
		Float64("threshold", alert.Threshold).
		Bool("inCooldown", exists).
		Msg("Alert passed cooldown check - adding to pending notifications")

	// Add to pending alerts for grouping
	n.pendingAlerts = append(n.pendingAlerts, alert)

	// If this is the first alert in the group, start the timer
	if n.groupTimer == nil {
		n.groupTimer = time.AfterFunc(n.groupWindow, func() {
			n.sendGroupedAlerts()
		})
		log.Debug().
			Int("pendingCount", len(n.pendingAlerts)).
			Dur("groupWindow", n.groupWindow).
			Msg("Started alert grouping timer")
	}
}

// CancelAlert removes pending notifications for a resolved alert
func (n *NotificationManager) CancelAlert(alertID string) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if len(n.pendingAlerts) == 0 {
		return
	}

	filtered := n.pendingAlerts[:0]
	removed := 0
	for _, pending := range n.pendingAlerts {
		if pending == nil {
			continue
		}
		if pending.ID == alertID {
			removed++
			continue
		}
		filtered = append(filtered, pending)
	}

	if removed == 0 {
		return
	}

	for i := len(filtered); i < len(n.pendingAlerts); i++ {
		n.pendingAlerts[i] = nil
	}

	n.pendingAlerts = filtered

	if len(n.pendingAlerts) == 0 && n.groupTimer != nil {
		if n.groupTimer.Stop() {
			log.Debug().Str("alertID", alertID).Msg("Stopped grouping timer after alert cancellation")
		}
		n.groupTimer = nil
	}

	log.Debug().
		Str("alertID", alertID).
		Int("remaining", len(n.pendingAlerts)).
		Msg("Removed resolved alert from pending notifications")
}

// sendGroupedAlerts sends all pending alerts as a group
func (n *NotificationManager) sendGroupedAlerts() {
	n.mu.Lock()
	defer n.mu.Unlock()

	if len(n.pendingAlerts) == 0 {
		return
	}

	// Copy alerts to send
	alertsToSend := make([]*alerts.Alert, len(n.pendingAlerts))
	copy(alertsToSend, n.pendingAlerts)

	// Clear pending alerts
	n.pendingAlerts = n.pendingAlerts[:0]
	n.groupTimer = nil

	log.Info().
		Int("alertCount", len(alertsToSend)).
		Msg("Sending grouped alert notifications")

	// Snapshot configuration while holding the lock to avoid races with concurrent updates
	emailConfig := copyEmailConfig(n.emailConfig)
	webhooks := copyWebhookConfigs(n.webhooks)
	appriseConfig := copyAppriseConfig(n.appriseConfig)

	// Send notifications using the captured snapshots outside the lock to avoid blocking writers
	if emailConfig.Enabled {
		log.Info().
			Int("alertCount", len(alertsToSend)).
			Str("smtpHost", emailConfig.SMTPHost).
			Int("smtpPort", emailConfig.SMTPPort).
			Strs("recipients", emailConfig.To).
			Bool("hasAuth", emailConfig.Username != "" && emailConfig.Password != "").
			Msg("Email notifications enabled - sending grouped email")
		go n.sendGroupedEmail(emailConfig, alertsToSend)
	} else {
		log.Debug().
			Int("alertCount", len(alertsToSend)).
			Msg("Email notifications disabled - skipping email delivery")
	}

	for _, webhook := range webhooks {
		if webhook.Enabled {
			go n.sendGroupedWebhook(webhook, alertsToSend)
		}
	}

	if appriseConfig.Enabled {
		go n.sendGroupedApprise(appriseConfig, alertsToSend)
	}

	// Update last notified time for all alerts
	now := time.Now()
	for _, alert := range alertsToSend {
		n.lastNotified[alert.ID] = notificationRecord{
			lastSent:   now,
			alertStart: alert.StartTime,
		}
	}
}

// sendGroupedEmail sends a grouped email notification
func (n *NotificationManager) sendGroupedEmail(config EmailConfig, alertList []*alerts.Alert) {

	// Don't check for recipients here - sendHTMLEmail handles empty recipients
	// by using the From address as the recipient

	// Generate email using template
	subject, htmlBody, textBody := EmailTemplate(alertList, false)

	// Send using HTML-aware method
	n.sendHTMLEmail(subject, htmlBody, textBody, config)
}

func (n *NotificationManager) sendGroupedApprise(config AppriseConfig, alertList []*alerts.Alert) {
	if len(alertList) == 0 {
		return
	}

	cfg := NormalizeAppriseConfig(config)
	if !cfg.Enabled {
		return
	}

	title, body, notifyType := buildApprisePayload(alertList, n.publicURL)
	if title == "" && body == "" {
		log.Warn().Msg("Apprise notification skipped: failed to build payload")
		return
	}

	switch cfg.Mode {
	case AppriseModeHTTP:
		if err := n.sendAppriseViaHTTP(cfg, title, body, notifyType); err != nil {
			log.Warn().
				Err(err).
				Str("mode", string(cfg.Mode)).
				Str("serverUrl", cfg.ServerURL).
				Msg("Failed to send Apprise notification via API")
		}
	default:
		if err := n.sendAppriseViaCLI(cfg, title, body); err != nil {
			log.Warn().
				Err(err).
				Str("mode", string(cfg.Mode)).
				Str("cliPath", cfg.CLIPath).
				Strs("targets", cfg.Targets).
				Msg("Failed to send Apprise notification")
		}
	}
}

func buildApprisePayload(alertList []*alerts.Alert, publicURL string) (string, string, string) {
	validAlerts := make([]*alerts.Alert, 0, len(alertList))
	var primary *alerts.Alert
	for _, alert := range alertList {
		if alert == nil {
			continue
		}
		if primary == nil {
			primary = alert
		}
		validAlerts = append(validAlerts, alert)
	}

	if len(validAlerts) == 0 || primary == nil {
		return "", "", "info"
	}

	title := fmt.Sprintf("Pulse alert: %s", primary.ResourceName)
	if len(validAlerts) > 1 {
		title = fmt.Sprintf("Pulse alerts (%d)", len(validAlerts))
	}

	var bodyBuilder strings.Builder
	bodyBuilder.WriteString(primary.Message)
	bodyBuilder.WriteString("\n\n")

	for _, alert := range validAlerts {
		bodyBuilder.WriteString(fmt.Sprintf("[%s] %s", strings.ToUpper(string(alert.Level)), alert.ResourceName))
		bodyBuilder.WriteString(fmt.Sprintf(" — value %.2f (threshold %.2f)\n", alert.Value, alert.Threshold))
		if alert.Node != "" {
			bodyBuilder.WriteString(fmt.Sprintf("Node: %s\n", alert.Node))
		}
		if alert.Instance != "" && alert.Instance != alert.Node {
			bodyBuilder.WriteString(fmt.Sprintf("Instance: %s\n", alert.Instance))
		}
		bodyBuilder.WriteString("\n")
	}

	if publicURL != "" {
		bodyBuilder.WriteString("Dashboard: " + publicURL + "\n")
	}

	return title, bodyBuilder.String(), resolveAppriseNotificationType(validAlerts)
}

func resolveAppriseNotificationType(alertList []*alerts.Alert) string {
	notifyType := "info"
	for _, alert := range alertList {
		if alert == nil {
			continue
		}
		switch alert.Level {
		case alerts.AlertLevelCritical:
			return "failure"
		case alerts.AlertLevelWarning:
			notifyType = "warning"
		}
	}
	return notifyType
}

func (n *NotificationManager) sendAppriseViaCLI(cfg AppriseConfig, title, body string) error {
	if len(cfg.Targets) == 0 {
		return fmt.Errorf("no Apprise targets configured for CLI delivery")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()

	args := []string{"-t", title, "-b", body}
	args = append(args, cfg.Targets...)

	execFn := n.appriseExec
	if execFn == nil {
		execFn = defaultAppriseExec
	}

	output, err := execFn(ctx, cfg.CLIPath, args)
	if err != nil {
		if len(output) > 0 {
			log.Debug().
				Str("cliPath", cfg.CLIPath).
				Strs("targets", cfg.Targets).
				Str("output", string(output)).
				Msg("Apprise CLI output (error)")
		}
		return err
	}

	if len(output) > 0 {
		log.Debug().
			Str("cliPath", cfg.CLIPath).
			Strs("targets", cfg.Targets).
			Str("output", string(output)).
			Msg("Apprise CLI output")
	}
	return nil
}

func (n *NotificationManager) sendAppriseViaHTTP(cfg AppriseConfig, title, body, notifyType string) error {
	if cfg.ServerURL == "" {
		return fmt.Errorf("apprise server URL is not configured")
	}

	serverURL := cfg.ServerURL
	lowerURL := strings.ToLower(serverURL)
	if !strings.HasPrefix(lowerURL, "http://") && !strings.HasPrefix(lowerURL, "https://") {
		return fmt.Errorf("apprise server URL must start with http or https: %s", serverURL)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()

	notifyEndpoint := "/notify"
	if cfg.ConfigKey != "" {
		notifyEndpoint = "/notify/" + url.PathEscape(cfg.ConfigKey)
	}

	requestURL := strings.TrimRight(serverURL, "/") + notifyEndpoint

	payload := map[string]any{
		"body":  body,
		"title": title,
	}
	if len(cfg.Targets) > 0 {
		payload["urls"] = cfg.Targets
	}
	if notifyType != "" {
		payload["type"] = notifyType
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal Apprise payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return fmt.Errorf("failed to create Apprise request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	if cfg.APIKey != "" {
		if cfg.APIKeyHeader == "" {
			req.Header.Set("X-API-KEY", cfg.APIKey)
		} else {
			req.Header.Set(cfg.APIKeyHeader, cfg.APIKey)
		}
	}

	client := &http.Client{
		Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second,
	}

	if strings.HasPrefix(lowerURL, "https://") && cfg.SkipTLSVerify {
		client.Transport = &http.Transport{
			Proxy:           http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to reach Apprise server: %w", err)
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, WebhookMaxResponseSize)
	respBody, _ := io.ReadAll(limited)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if len(respBody) > 0 {
			return fmt.Errorf("apprise server returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
		}
		return fmt.Errorf("apprise server returned HTTP %d", resp.StatusCode)
	}

	if len(respBody) > 0 {
		log.Debug().
			Str("mode", string(cfg.Mode)).
			Str("serverUrl", cfg.ServerURL).
			Str("response", string(respBody)).
			Msg("Apprise API response")
	}

	return nil
}

// sendEmail sends an email notification
func (n *NotificationManager) sendEmail(alert *alerts.Alert) {
	n.mu.RLock()
	config := n.emailConfig
	n.mu.RUnlock()

	// Don't check for recipients here - sendHTMLEmail handles empty recipients
	// by using the From address as the recipient

	// Generate email using template
	subject, htmlBody, textBody := EmailTemplate([]*alerts.Alert{alert}, true)

	// Send using HTML-aware method
	n.sendHTMLEmail(subject, htmlBody, textBody, config)
}

// sendHTMLEmailWithError sends an HTML email with multipart content and returns any error
func (n *NotificationManager) sendHTMLEmailWithError(subject, htmlBody, textBody string, config EmailConfig) error {
	// Use From address as recipient if To is empty
	recipients := config.To
	if len(recipients) == 0 && config.From != "" {
		recipients = []string{config.From}
		log.Info().
			Str("from", config.From).
			Msg("Using From address as recipient since To is empty")
	}

	// Create enhanced email configuration with proper STARTTLS support
	enhancedConfig := EmailProviderConfig{
		EmailConfig: EmailConfig{
			From:     config.From,
			To:       recipients,
			SMTPHost: config.SMTPHost,
			SMTPPort: config.SMTPPort,
			Username: config.Username,
			Password: config.Password,
		},
		Provider:      config.Provider,
		StartTLS:      config.StartTLS, // Use the configured StartTLS setting
		MaxRetries:    2,
		RetryDelay:    3,
		RateLimit:     60,
		SkipTLSVerify: false,
		AuthRequired:  config.Username != "" && config.Password != "",
	}

	// Use enhanced email manager for better compatibility
	enhancedManager := NewEnhancedEmailManager(enhancedConfig)

	log.Info().
		Str("smtp", fmt.Sprintf("%s:%d", config.SMTPHost, config.SMTPPort)).
		Str("from", config.From).
		Strs("to", recipients).
		Bool("hasAuth", config.Username != "" && config.Password != "").
		Bool("startTLS", enhancedConfig.StartTLS).
		Msg("Attempting to send email via SMTP with enhanced support")

	err := enhancedManager.SendEmailWithRetry(subject, htmlBody, textBody)

	if err != nil {
		log.Error().
			Err(err).
			Str("smtp", fmt.Sprintf("%s:%d", config.SMTPHost, config.SMTPPort)).
			Strs("recipients", recipients).
			Msg("Failed to send email notification")
		return fmt.Errorf("failed to send email: %w", err)
	}

	log.Info().
		Strs("recipients", recipients).
		Int("recipientCount", len(recipients)).
		Msg("Email notification sent successfully")
	return nil
}

// sendHTMLEmail sends an HTML email with multipart content
func (n *NotificationManager) sendHTMLEmail(subject, htmlBody, textBody string, config EmailConfig) {
	// Use From address as recipient if To is empty
	recipients := config.To
	if len(recipients) == 0 && config.From != "" {
		recipients = []string{config.From}
		log.Info().
			Str("from", config.From).
			Msg("Using From address as recipient since To is empty")
	}

	// Create enhanced email configuration with proper STARTTLS support
	enhancedConfig := EmailProviderConfig{
		EmailConfig: EmailConfig{
			From:     config.From,
			To:       recipients,
			SMTPHost: config.SMTPHost,
			SMTPPort: config.SMTPPort,
			Username: config.Username,
			Password: config.Password,
		},
		Provider:      config.Provider,
		StartTLS:      config.StartTLS, // Use the configured StartTLS setting
		MaxRetries:    2,
		RetryDelay:    3,
		RateLimit:     60,
		SkipTLSVerify: false,
		AuthRequired:  config.Username != "" && config.Password != "",
	}

	// Use enhanced email manager for better compatibility
	enhancedManager := NewEnhancedEmailManager(enhancedConfig)

	log.Info().
		Str("smtp", fmt.Sprintf("%s:%d", config.SMTPHost, config.SMTPPort)).
		Str("from", config.From).
		Strs("to", recipients).
		Bool("hasAuth", config.Username != "" && config.Password != "").
		Bool("startTLS", enhancedConfig.StartTLS).
		Msg("Attempting to send email via SMTP with enhanced support")

	err := enhancedManager.SendEmailWithRetry(subject, htmlBody, textBody)

	if err != nil {
		log.Error().
			Err(err).
			Str("smtp", fmt.Sprintf("%s:%d", config.SMTPHost, config.SMTPPort)).
			Strs("recipients", recipients).
			Msg("Failed to send email notification")
	} else {
		log.Info().
			Strs("recipients", recipients).
			Int("recipientCount", len(recipients)).
			Msg("Email notification sent successfully")
	}
}

// sendEmailWithContent sends email with given content (plain text)
func (n *NotificationManager) sendEmailWithContent(subject, body string, config EmailConfig) {
	// For backward compatibility, send as plain text
	n.sendHTMLEmail(subject, "", body, config)
}

// sendGroupedWebhook sends a grouped webhook notification
func (n *NotificationManager) sendGroupedWebhook(webhook WebhookConfig, alertList []*alerts.Alert) {
	var jsonData []byte
	var err error

	if len(alertList) == 0 {
		log.Warn().
			Str("webhook", webhook.Name).
			Msg("Attempted to send grouped webhook with no alerts")
		return
	}

	primaryAlert := alertList[0]
	customFields := convertWebhookCustomFields(webhook.CustomFields)

	var templateData WebhookPayloadData
	var dataPrepared bool
	var urlRendered bool
	var serviceDataApplied bool

	prepareData := func() *WebhookPayloadData {
		if !dataPrepared {
			prepared := n.prepareWebhookData(primaryAlert, customFields)
			prepared.AlertCount = len(alertList)
			prepared.Alerts = alertList
			templateData = prepared
			dataPrepared = true
		}
		return &templateData
	}

	ensureURLAndServiceData := func() (*WebhookPayloadData, bool) {
		dataPtr := prepareData()

		if !urlRendered {
			rendered, renderErr := renderWebhookURL(webhook.URL, *dataPtr)
			if renderErr != nil {
				log.Error().
					Err(renderErr).
					Str("webhook", webhook.Name).
					Msg("Failed to render webhook URL template for grouped notification")
				return nil, false
			}
			webhook.URL = rendered
			urlRendered = true
		}

		if !serviceDataApplied {
			switch webhook.Service {
			case "telegram":
				chatID, chatErr := extractTelegramChatID(webhook.URL)
				if chatErr != nil {
					log.Error().
						Err(chatErr).
						Str("webhook", webhook.Name).
						Msg("Failed to extract Telegram chat_id for grouped notification")
					return nil, false
				}
				if chatID != "" {
					dataPtr.ChatID = chatID
					log.Debug().
						Str("webhook", webhook.Name).
						Str("chatID", chatID).
						Msg("Extracted Telegram chat_id from rendered URL for grouped notification")
				}
			case "pagerduty":
				if dataPtr.CustomFields == nil {
					dataPtr.CustomFields = make(map[string]interface{})
				}
				if routingKey, ok := webhook.Headers["routing_key"]; ok {
					dataPtr.CustomFields["routing_key"] = routingKey
				}
			case "pushover":
				dataPtr.CustomFields = ensurePushoverCustomFieldAliases(dataPtr.CustomFields)
			}
			serviceDataApplied = true
		}

		return dataPtr, true
	}

	// Check if webhook has a custom template first
	// Only use custom template if it's not empty
	if webhook.Template != "" && strings.TrimSpace(webhook.Template) != "" && len(alertList) > 0 {
		// Use custom template with enhanced message for grouped alerts
		alert := primaryAlert
		if len(alertList) > 1 {
			// Build a full list of all alerts
			summary := alert.Message
			otherAlerts := []string{}
			for i := 1; i < len(alertList); i++ { // Show ALL alerts
				otherAlerts = append(otherAlerts, fmt.Sprintf("• %s: %.1f%%", alertList[i].ResourceName, alertList[i].Value))
			}
			if len(otherAlerts) > 0 {
				// For custom templates, we need to escape newlines since they're likely
				// used in shell commands or other contexts that need escaping
				alert.Message = fmt.Sprintf("%s\\n\\n🔔 All %d alerts:\\n%s", summary, len(alertList), strings.Join(otherAlerts, "\\n"))
			}
		}

		enhanced := EnhancedWebhookConfig{
			WebhookConfig:   webhook,
			Service:         webhook.Service,
			PayloadTemplate: webhook.Template,
			CustomFields:    customFields,
		}

		if dataPtr, ok := ensureURLAndServiceData(); ok {
			jsonData, err = n.generatePayloadFromTemplateWithService(enhanced.PayloadTemplate, *dataPtr, webhook.Service)
		} else {
			return
		}
		if err != nil {
			log.Error().
				Err(err).
				Str("webhook", webhook.Name).
				Int("alertCount", len(alertList)).
				Msg("Failed to generate grouped payload from custom template")
			return
		}
	} else if webhook.Service != "" && webhook.Service != "generic" && len(alertList) > 0 {
		// For service-specific webhooks, use the first alert with a note about others
		// For simplicity, send the first alert with a note about others
		// Most webhook services work better with single structured payloads
		alert := primaryAlert

		enhanced := EnhancedWebhookConfig{
			WebhookConfig: webhook,
			Service:       webhook.Service,
			CustomFields:  customFields,
		}

		// Get service template
		templates := GetWebhookTemplates()
		templateFound := false
		for _, tmpl := range templates {
			if tmpl.Service == webhook.Service {
				enhanced.PayloadTemplate = tmpl.PayloadTemplate
				templateFound = true
				break
			}
		}

		if templateFound {
			// Modify message if multiple alerts - but format differently for Discord
			if len(alertList) > 1 {
				summary := alert.Message
				otherAlerts := []string{}
				for i := 1; i < len(alertList); i++ {
					otherAlerts = append(otherAlerts, fmt.Sprintf("• %s: %.1f%%", alertList[i].ResourceName, alertList[i].Value))
				}
				if len(otherAlerts) > 0 {
					// For Discord, format as a single line list to avoid newline issues
					// Discord embeds don't render \n in description anyway
					if webhook.Service == "discord" {
						// Use comma-separated list for Discord
						alert.Message = fmt.Sprintf("%s | 🔔 %d alerts: %s", summary, len(alertList), strings.Join(otherAlerts, ", "))
					} else {
						// For other services, escape newlines properly
						alert.Message = fmt.Sprintf("%s\\n\\n🔔 All %d alerts:\\n%s", summary, len(alertList), strings.Join(otherAlerts, "\\n"))
					}
				}
			}

			if dataPtr, ok := ensureURLAndServiceData(); ok {
				jsonData, err = n.generatePayloadFromTemplateWithService(enhanced.PayloadTemplate, *dataPtr, webhook.Service)
			} else {
				return
			}
			if err != nil {
				log.Error().
					Err(err).
					Str("webhook", webhook.Name).
					Int("alertCount", len(alertList)).
					Msg("Failed to generate payload for grouped alerts")
				return
			}
		} else {
			// No template found, use generic payload
			webhook.Service = "generic"
		}
	}

	// Use generic payload if no service or template not found
	// But ONLY if jsonData hasn't been set yet (from custom template)
	if jsonData == nil && (webhook.Service == "" || webhook.Service == "generic") {
		if _, ok := ensureURLAndServiceData(); !ok {
			return
		}

		// Use generic payload for other services
		payload := map[string]interface{}{
			"alerts":    alertList,
			"count":     len(alertList),
			"timestamp": time.Now().Unix(),
			"source":    "pulse-monitoring",
			"grouped":   true,
		}

		jsonData, err = json.Marshal(payload)
		if err != nil {
			log.Error().
				Err(err).
				Str("webhook", webhook.Name).
				Int("alertCount", len(alertList)).
				Msg("Failed to marshal grouped webhook payload")
			return
		}
	}

	if _, ok := ensureURLAndServiceData(); !ok {
		return
	}

	// Send using same request logic
	n.sendWebhookRequest(webhook, jsonData, "grouped")
}

// checkWebhookRateLimit checks if a webhook can be sent based on rate limits
func (n *NotificationManager) checkWebhookRateLimit(webhookURL string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	now := time.Now()
	limit, exists := n.webhookRateLimits[webhookURL]

	if !exists {
		// First time sending to this webhook
		n.webhookRateLimits[webhookURL] = &webhookRateLimit{
			lastSent:  now,
			sentCount: 1,
		}
		return true
	}

	// Check if we're still in the rate limit window
	if now.Sub(limit.lastSent) > WebhookRateLimitWindow {
		// Window expired, reset counter
		limit.lastSent = now
		limit.sentCount = 1
		return true
	}

	// Still in window, check if we've exceeded the limit
	if limit.sentCount >= WebhookRateLimitMax {
		log.Warn().
			Str("webhookURL", webhookURL).
			Int("sentCount", limit.sentCount).
			Dur("window", WebhookRateLimitWindow).
			Msg("Webhook rate limit exceeded, dropping request")
		return false
	}

	// Increment counter and allow
	limit.sentCount++
	return true
}

// sendWebhookRequest sends the actual webhook request
func (n *NotificationManager) sendWebhookRequest(webhook WebhookConfig, jsonData []byte, alertType string) {
	// Check rate limit before sending
	if !n.checkWebhookRateLimit(webhook.URL) {
		log.Warn().
			Str("webhook", webhook.Name).
			Str("url", webhook.URL).
			Msg("Webhook request dropped due to rate limiting")
		return
	}

	// Create request
	method := webhook.Method
	if method == "" {
		method = "POST"
	}

	// For Telegram webhooks, strip chat_id from URL if present
	// The chat_id should only be in the JSON body, not the URL
	webhookURL := webhook.URL
	if webhook.Service == "telegram" && strings.Contains(webhookURL, "chat_id=") {
		if u, err := url.Parse(webhookURL); err == nil {
			q := u.Query()
			q.Del("chat_id") // Remove chat_id from query params
			u.RawQuery = q.Encode()
			webhookURL = u.String()
			log.Debug().
				Str("original", webhook.URL).
				Str("cleaned", webhookURL).
				Msg("Stripped chat_id from Telegram webhook URL")
		}
	}

	req, err := http.NewRequest(method, webhookURL, bytes.NewBuffer(jsonData))
	if err != nil {
		log.Error().
			Err(err).
			Str("webhook", webhook.Name).
			Str("type", alertType).
			Msg("Failed to create webhook request")
		return
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Pulse-Monitoring/2.0")

	// Special handling for ntfy service
	if webhook.Service == "ntfy" {
		// Set Content-Type for ntfy (plain text)
		req.Header.Set("Content-Type", "text/plain")
		// Note: Dynamic headers for ntfy are set in sendWebhook for individual alerts
	}

	// Apply any custom headers from webhook config
	for key, value := range webhook.Headers {
		// Skip template-like headers (those with {{) to prevent errors
		if !strings.Contains(value, "{{") {
			req.Header.Set(key, value)
		}
	}

	// Debug log the payload for Telegram and Gotify webhooks
	if webhook.Service == "telegram" || webhook.Service == "gotify" {
		log.Debug().
			Str("webhook", webhook.Name).
			Str("service", webhook.Service).
			Str("url", webhookURL).
			Str("payload", string(jsonData)).
			Msg("Sending webhook with payload")
	}

	// Send request with secure client
	client := createSecureWebhookClient(WebhookTimeout)

	resp, err := client.Do(req)
	if err != nil {
		log.Error().
			Err(err).
			Str("webhook", webhook.Name).
			Str("type", alertType).
			Msg("Failed to send webhook")
		return
	}
	defer resp.Body.Close()

	// Read response body with size limit to prevent memory exhaustion
	limitedReader := io.LimitReader(resp.Body, WebhookMaxResponseSize)
	var respBody bytes.Buffer
	bytesRead, err := respBody.ReadFrom(limitedReader)
	if err != nil {
		log.Warn().
			Err(err).
			Str("webhook", webhook.Name).
			Str("type", alertType).
			Msg("Failed to read webhook response body")
		return
	}

	// Check if we hit the size limit
	if bytesRead >= WebhookMaxResponseSize {
		log.Warn().
			Str("webhook", webhook.Name).
			Int64("bytesRead", bytesRead).
			Int("maxSize", WebhookMaxResponseSize).
			Msg("Webhook response exceeded size limit, truncated")
	}

	responseBody := respBody.String()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		log.Info().
			Str("webhook", webhook.Name).
			Str("service", webhook.Service).
			Str("type", alertType).
			Int("status", resp.StatusCode).
			Int("payloadSize", len(jsonData)).
			Msg("Webhook notification sent successfully")

		// Log response body only in debug mode for successful requests
		if len(responseBody) > 0 {
			log.Debug().
				Str("webhook", webhook.Name).
				Str("response", responseBody).
				Msg("Webhook response body")
		}
	} else {
		log.Warn().
			Str("webhook", webhook.Name).
			Str("service", webhook.Service).
			Str("type", alertType).
			Int("status", resp.StatusCode).
			Str("response", responseBody).
			Msg("Webhook returned non-success status")
	}
}

// sendWebhook sends a webhook notification
func (n *NotificationManager) sendWebhook(webhook WebhookConfig, alert *alerts.Alert) {
	var jsonData []byte
	var err error

	customFields := convertWebhookCustomFields(webhook.CustomFields)
	data := n.prepareWebhookData(alert, customFields)

	// Render URL template if placeholders are present
	renderedURL, renderErr := renderWebhookURL(webhook.URL, data)
	if renderErr != nil {
		log.Error().
			Err(renderErr).
			Str("webhook", webhook.Name).
			Msg("Failed to render webhook URL template")
		return
	}
	webhook.URL = renderedURL

	// Service-specific data enrichment
	if webhook.Service == "telegram" {
		chatID, chatErr := extractTelegramChatID(renderedURL)
		if chatErr != nil {
			log.Error().
				Err(chatErr).
				Str("webhook", webhook.Name).
				Msg("Failed to extract Telegram chat_id - skipping webhook")
			return
		}
		if chatID != "" {
			data.ChatID = chatID
			log.Debug().
				Str("webhook", webhook.Name).
				Str("chatID", chatID).
				Msg("Extracted Telegram chat_id from rendered URL")
		}
	} else if webhook.Service == "pagerduty" {
		if data.CustomFields == nil {
			data.CustomFields = make(map[string]interface{})
		}
		if routingKey, ok := webhook.Headers["routing_key"]; ok {
			data.CustomFields["routing_key"] = routingKey
		}
	}

	// Check if webhook has a custom template first
	// Only use custom template if it's not empty
	if webhook.Template != "" && strings.TrimSpace(webhook.Template) != "" {
		// Use custom template provided by user
		enhanced := EnhancedWebhookConfig{
			WebhookConfig:   webhook,
			Service:         webhook.Service,
			PayloadTemplate: webhook.Template,
			CustomFields:    customFields,
		}

		jsonData, err = n.generatePayloadFromTemplateWithService(enhanced.PayloadTemplate, data, webhook.Service)
		if err != nil {
			log.Error().
				Err(err).
				Str("webhook", webhook.Name).
				Str("alertID", alert.ID).
				Msg("Failed to generate webhook payload from custom template")
			return
		}
	} else if webhook.Service != "" && webhook.Service != "generic" {
		// Check if this webhook has a service type and use the proper template
		// Convert to enhanced webhook to use template
		enhanced := EnhancedWebhookConfig{
			WebhookConfig: webhook,
			Service:       webhook.Service,
			CustomFields:  customFields,
		}

		// Get service template
		templates := GetWebhookTemplates()
		templateFound := false
		for _, tmpl := range templates {
			if tmpl.Service == webhook.Service {
				enhanced.PayloadTemplate = tmpl.PayloadTemplate
				templateFound = true
				break
			}
		}

		// Only use template if found, otherwise fall back to generic
		if templateFound {
			jsonData, err = n.generatePayloadFromTemplateWithService(enhanced.PayloadTemplate, data, webhook.Service)
			if err != nil {
				log.Error().
					Err(err).
					Str("webhook", webhook.Name).
					Str("service", webhook.Service).
					Str("alertID", alert.ID).
					Msg("Failed to generate webhook payload")
				return
			}
		} else {
			// No template found, use generic payload
			webhook.Service = "generic"
		}
	}

	// Use generic payload if no service or template not found
	// But ONLY if jsonData hasn't been set yet (from custom template)
	if jsonData == nil && (webhook.Service == "" || webhook.Service == "generic") {
		// Use generic payload for other services
		payload := map[string]interface{}{
			"alert":     alert,
			"timestamp": time.Now().Unix(),
			"source":    "pulse-monitoring",
		}

		jsonData, err = json.Marshal(payload)
		if err != nil {
			log.Error().
				Err(err).
				Str("webhook", webhook.Name).
				Str("alertID", alert.ID).
				Msg("Failed to marshal webhook payload")
			return
		}
	}

	// Send using common request logic
	n.sendWebhookRequest(webhook, jsonData, fmt.Sprintf("alert-%s", alert.ID))
}

func convertWebhookCustomFields(fields map[string]string) map[string]interface{} {
	if len(fields) == 0 {
		return nil
	}

	converted := make(map[string]interface{}, len(fields))
	for key, value := range fields {
		converted[key] = value
	}
	return converted
}

func ensurePushoverCustomFieldAliases(fields map[string]interface{}) map[string]interface{} {
	if fields == nil {
		return nil
	}

	if _, ok := fields["token"]; !ok || isEmptyInterface(fields["token"]) {
		if legacy, ok := fields["app_token"]; ok && !isEmptyInterface(legacy) {
			fields["token"] = legacy
		}
	}

	if _, ok := fields["user"]; !ok || isEmptyInterface(fields["user"]) {
		if legacy, ok := fields["user_token"]; ok && !isEmptyInterface(legacy) {
			fields["user"] = legacy
		}
	}

	return fields
}

func isEmptyInterface(value interface{}) bool {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v) == ""
	case fmt.Stringer:
		return strings.TrimSpace(v.String()) == ""
	case nil:
		return true
	default:
		return false
	}
}

// prepareWebhookData prepares data for template rendering
func (n *NotificationManager) prepareWebhookData(alert *alerts.Alert, customFields map[string]interface{}) WebhookPayloadData {
	duration := time.Since(alert.StartTime)

	// Construct full Pulse URL if publicURL is configured
	// The Instance field should contain the full URL to the Pulse dashboard
	instance := ""
	if n.publicURL != "" {
		// Remove trailing slash from publicURL if present
		instance = strings.TrimRight(n.publicURL, "/")
	} else if alert.Instance != "" && (strings.HasPrefix(alert.Instance, "http://") || strings.HasPrefix(alert.Instance, "https://")) {
		// If publicURL is not set but alert.Instance contains a full URL, use it
		instance = alert.Instance
	}

	resourceType := ""
	if alert.Metadata != nil {
		if rt, ok := alert.Metadata["resourceType"].(string); ok {
			resourceType = rt
		}
	}

	var metadataCopy map[string]interface{}
	if alert.Metadata != nil {
		metadataCopy = make(map[string]interface{}, len(alert.Metadata))
		for k, v := range alert.Metadata {
			metadataCopy[k] = v
		}
	}

	var ackTime string
	if alert.AckTime != nil {
		ackTime = alert.AckTime.Format(time.RFC3339)
	}

	return WebhookPayloadData{
		ID:                 alert.ID,
		Level:              string(alert.Level),
		Type:               alert.Type,
		ResourceName:       alert.ResourceName,
		ResourceID:         alert.ResourceID,
		Node:               alert.Node,
		Instance:           instance,
		Message:            alert.Message,
		Value:              alert.Value,
		Threshold:          alert.Threshold,
		ValueFormatted:     formatMetricValue(alert.Type, alert.Value),
		ThresholdFormatted: formatMetricThreshold(alert.Type, alert.Threshold),
		StartTime:          alert.StartTime.Format(time.RFC3339),
		Duration:           formatWebhookDuration(duration),
		Timestamp:          time.Now().Format(time.RFC3339),
		ResourceType:       resourceType,
		Acknowledged:       alert.Acknowledged,
		AckTime:            ackTime,
		AckUser:            alert.AckUser,
		Metadata:           metadataCopy,
		CustomFields:       customFields,
		AlertCount:         1,
	}
}

func templateFuncMap() template.FuncMap {
	return template.FuncMap{
		"title": func(s string) string {
			if s == "" {
				return s
			}
			return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
		},
		"upper":     strings.ToUpper,
		"lower":     strings.ToLower,
		"printf":    fmt.Sprintf,
		"urlquery":  template.URLQueryEscaper,
		"urlencode": template.URLQueryEscaper,
		"urlpath":   url.PathEscape,
		"pathescape": func(s string) string {
			return url.PathEscape(s)
		},
	}
}

// generatePayloadFromTemplate renders the payload using Go templates
func (n *NotificationManager) generatePayloadFromTemplate(templateStr string, data WebhookPayloadData) ([]byte, error) {
	return n.generatePayloadFromTemplateWithService(templateStr, data, "")
}

// generatePayloadFromTemplateWithService renders the payload using Go templates with service-specific handling
func (n *NotificationManager) generatePayloadFromTemplateWithService(templateStr string, data WebhookPayloadData, service string) ([]byte, error) {
	tmpl, err := template.New("webhook").Funcs(templateFuncMap()).Parse(templateStr)
	if err != nil {
		return nil, fmt.Errorf("invalid template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("template execution failed: %w", err)
	}

	// Skip JSON validation for services that use plain text payloads
	if service == "ntfy" {
		// ntfy uses plain text, not JSON
		return buf.Bytes(), nil
	}

	// Validate that the generated payload is valid JSON for other services
	var jsonCheck interface{}
	if err := json.Unmarshal(buf.Bytes(), &jsonCheck); err != nil {
		log.Error().
			Err(err).
			Str("payload", string(buf.Bytes())).
			Msg("Generated webhook payload is invalid JSON")
		return nil, fmt.Errorf("template produced invalid JSON: %w", err)
	}

	return buf.Bytes(), nil
}

// renderWebhookURL applies template rendering to webhook URLs and ensures the result is a valid URL
func renderWebhookURL(urlTemplate string, data WebhookPayloadData) (string, error) {
	trimmed := strings.TrimSpace(urlTemplate)
	if trimmed == "" {
		return "", fmt.Errorf("webhook URL cannot be empty")
	}

	if !strings.Contains(trimmed, "{{") {
		return trimmed, nil
	}

	tmpl, err := template.New("webhook_url").Funcs(templateFuncMap()).Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("invalid webhook URL template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("webhook URL template execution failed: %w", err)
	}

	rendered := strings.TrimSpace(buf.String())
	if rendered == "" {
		return "", fmt.Errorf("webhook URL template produced empty URL")
	}

	parsed, err := url.Parse(rendered)
	if err != nil {
		return "", fmt.Errorf("webhook URL template produced invalid URL: %w", err)
	}

	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("webhook URL template produced invalid URL: missing scheme or host")
	}

	return parsed.String(), nil
}

// formatWebhookDuration formats a duration in a human-readable way
func formatWebhookDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	} else if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	} else if d < 24*time.Hour {
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	} else {
		days := int(d.Hours()) / 24
		hours := int(d.Hours()) % 24
		return fmt.Sprintf("%dd %dh", days, hours)
	}
}

// extractTelegramChatID extracts and validates the chat_id from a Telegram webhook URL
func extractTelegramChatID(webhookURL string) (string, error) {
	if !strings.Contains(webhookURL, "chat_id=") {
		return "", fmt.Errorf("Telegram webhook URL missing chat_id parameter")
	}

	u, err := url.Parse(webhookURL)
	if err != nil {
		return "", fmt.Errorf("invalid URL format: %w", err)
	}

	chatID := u.Query().Get("chat_id")
	if chatID == "" {
		return "", fmt.Errorf("chat_id parameter is empty")
	}

	// Validate that chat_id is numeric (Telegram chat IDs are always numeric)
	// Handle negative IDs (group chats) and positive IDs (private chats)
	if strings.HasPrefix(chatID, "-") {
		if !isNumeric(chatID[1:]) {
			return "", fmt.Errorf("chat_id must be numeric, got: %s", chatID)
		}
	} else if !isNumeric(chatID) {
		return "", fmt.Errorf("chat_id must be numeric, got: %s", chatID)
	}

	return chatID, nil
}

// isNumeric checks if a string contains only digits
func isNumeric(s string) bool {
	for _, char := range s {
		if char < '0' || char > '9' {
			return false
		}
	}
	return len(s) > 0
}

// ValidateWebhookURL validates that a webhook URL is safe and properly formed
func ValidateWebhookURL(webhookURL string) error {
	if webhookURL == "" {
		return fmt.Errorf("webhook URL cannot be empty")
	}

	u, err := url.Parse(webhookURL)
	if err != nil {
		return fmt.Errorf("invalid URL format: %w", err)
	}

	// Must be HTTP or HTTPS
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("webhook URL must use http or https protocol")
	}

	// Get hostname for validation
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("webhook URL missing hostname")
	}

	// Block localhost and loopback addresses (SSRF protection)
	if host == "localhost" || host == "127.0.0.1" || host == "::1" || strings.HasPrefix(host, "127.") {
		return fmt.Errorf("webhook URLs pointing to localhost are not allowed for security reasons")
	}

	// Block link-local addresses
	if strings.HasPrefix(host, "169.254.") || strings.HasPrefix(host, "fe80:") {
		return fmt.Errorf("webhook URLs pointing to link-local addresses are not allowed")
	}

	// Block private IP ranges (10.x.x.x, 172.16-31.x.x, 192.168.x.x)
	// These are commonly used for internal services and pose SSRF risks
	if strings.HasPrefix(host, "10.") ||
		strings.HasPrefix(host, "192.168.") ||
		(strings.HasPrefix(host, "172.") && isPrivateRange172(host)) {
		// Log warning but allow (many legitimate webhook services run on private networks)
		log.Warn().
			Str("url", webhookURL).
			Msg("Webhook URL points to private network - ensure this is intentional")
	}

	// Block common metadata service endpoints (cloud providers)
	metadataHosts := []string{
		"169.254.169.254", // AWS, Azure, GCP metadata
		"metadata.google.internal",
		"metadata.goog",
	}
	for _, metadataHost := range metadataHosts {
		if host == metadataHost {
			return fmt.Errorf("webhook URLs pointing to cloud metadata services are not allowed")
		}
	}

	// Ensure hostname is not just an IP address without proper DNS
	// This helps prevent SSRF attacks using numeric IPs to bypass filters
	if u.Scheme == "https" && isNumericIP(host) {
		log.Warn().
			Str("url", webhookURL).
			Msg("Webhook URL uses numeric IP with HTTPS - certificate validation may fail")
	}

	return nil
}

// isNumericIP checks if a string is a numeric IP address
func isNumericIP(host string) bool {
	// Simple check: if it contains only digits, dots, and colons, it's likely an IP
	for _, char := range host {
		if !(char >= '0' && char <= '9') && char != '.' && char != ':' {
			return false
		}
	}
	return len(host) > 0 && (strings.Contains(host, ".") || strings.Contains(host, ":"))
}

// isPrivateRange172 checks if an IP is in the 172.16.0.0/12 range
func isPrivateRange172(host string) bool {
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return false
	}
	if parts[0] != "172" {
		return false
	}

	// Check if second octet is between 16 and 31
	if len(parts[1]) == 0 {
		return false
	}

	second := 0
	for _, char := range parts[1] {
		if char < '0' || char > '9' {
			return false
		}
		second = second*10 + int(char-'0')
	}

	return second >= 16 && second <= 31
}

// addWebhookDelivery adds a webhook delivery record to the history
func (n *NotificationManager) addWebhookDelivery(delivery WebhookDelivery) {
	n.mu.Lock()
	defer n.mu.Unlock()

	// Add to history
	n.webhookHistory = append(n.webhookHistory, delivery)

	// Keep only last 100 entries
	if len(n.webhookHistory) > 100 {
		// Remove oldest entry
		n.webhookHistory = n.webhookHistory[1:]
	}
}

// GetWebhookHistory returns recent webhook delivery history
func (n *NotificationManager) GetWebhookHistory() []WebhookDelivery {
	n.mu.RLock()
	defer n.mu.RUnlock()

	// Return a copy to avoid concurrent access issues
	history := make([]WebhookDelivery, len(n.webhookHistory))
	copy(history, n.webhookHistory)
	return history
}

// groupAlerts groups alerts based on configuration
func (n *NotificationManager) groupAlerts(alertList []*alerts.Alert) map[string][]*alerts.Alert {
	groups := make(map[string][]*alerts.Alert)

	if !n.groupByNode && !n.groupByGuest {
		// No grouping - all alerts in one group
		groups["all"] = alertList
		return groups
	}

	for _, alert := range alertList {
		var key string

		if n.groupByNode && n.groupByGuest {
			// Group by both node and guest type
			guestType := "unknown"
			if metadata, ok := alert.Metadata["resourceType"].(string); ok {
				guestType = metadata
			}
			key = fmt.Sprintf("%s-%s", alert.Node, guestType)
		} else if n.groupByNode {
			// Group by node only
			key = alert.Node
		} else if n.groupByGuest {
			// Group by guest type only
			if metadata, ok := alert.Metadata["resourceType"].(string); ok {
				key = metadata
			} else {
				key = "unknown"
			}
		}

		groups[key] = append(groups[key], alert)
	}

	return groups
}

// SendTestNotification sends a test notification
func (n *NotificationManager) SendTestNotification(method string) error {
	testAlert := &alerts.Alert{
		ID:           "test-alert",
		Type:         "cpu",
		Level:        "warning",
		ResourceID:   "test-resource",
		ResourceName: "Test Resource",
		Node:         "pve-node-01",
		Instance:     "https://192.168.1.100:8006",
		Message:      "This is a test alert from Pulse Monitoring to verify your notification settings are working correctly",
		Value:        95.5,
		Threshold:    90,
		StartTime:    time.Now().Add(-5 * time.Minute), // Show it's been active for 5 minutes
		LastSeen:     time.Now(),
		Metadata: map[string]interface{}{
			"resourceType": "vm",
		},
	}

	switch method {
	case "email":
		log.Info().
			Bool("enabled", n.emailConfig.Enabled).
			Str("smtp", n.emailConfig.SMTPHost).
			Int("port", n.emailConfig.SMTPPort).
			Str("from", n.emailConfig.From).
			Int("toCount", len(n.emailConfig.To)).
			Msg("Testing email notification")
		if !n.emailConfig.Enabled {
			return fmt.Errorf("email notifications are not enabled")
		}
		n.sendEmail(testAlert)
		return nil
	case "webhook":
		n.mu.RLock()
		if len(n.webhooks) == 0 {
			n.mu.RUnlock()
			return fmt.Errorf("no webhooks configured")
		}
		// Find first enabled webhook and copy it before releasing lock
		var webhookToTest *WebhookConfig
		for _, webhook := range n.webhooks {
			if webhook.Enabled {
				// Copy webhook to avoid race condition
				webhookCopy := webhook
				webhookToTest = &webhookCopy
				break
			}
		}
		n.mu.RUnlock()

		if webhookToTest == nil {
			return fmt.Errorf("no enabled webhooks found")
		}
		n.sendWebhook(*webhookToTest, testAlert)
		return nil
	default:
		return fmt.Errorf("unknown notification method: %s", method)
	}
}

// SendTestWebhook sends a test notification to a specific webhook
func (n *NotificationManager) SendTestWebhook(webhook WebhookConfig) error {
	// Create a test alert for webhook testing with realistic values
	// Use the configured publicURL if available, otherwise use a placeholder
	instanceURL := n.publicURL
	if instanceURL == "" {
		instanceURL = "http://your-pulse-instance:7655"
	}

	testAlert := &alerts.Alert{
		ID:           "test-webhook-" + webhook.ID,
		Type:         "cpu",
		Level:        "warning",
		ResourceID:   "webhook-test",
		ResourceName: "Test Alert",
		Node:         "test-node",
		Instance:     instanceURL, // Use the actual Pulse URL
		Message:      fmt.Sprintf("This is a test alert from Pulse to verify your %s webhook is working correctly", webhook.Name),
		Value:        85.5,
		Threshold:    80.0,
		StartTime:    time.Now().Add(-5 * time.Minute), // Alert started 5 minutes ago
		LastSeen:     time.Now(),
		Metadata: map[string]interface{}{
			"webhookName": webhook.Name,
			"webhookURL":  webhook.URL,
			"testTime":    time.Now().Format(time.RFC3339),
		},
	}

	// Send the test webhook
	n.sendWebhook(webhook, testAlert)
	return nil
}

// SendTestNotificationWithConfig sends a test notification using provided config
func (n *NotificationManager) SendTestNotificationWithConfig(method string, config *EmailConfig, nodeInfo *TestNodeInfo) error {
	// Use actual node info if provided, otherwise use defaults
	nodeName := "test-node"
	instanceURL := n.publicURL
	if instanceURL == "" {
		instanceURL = "https://proxmox.local:8006"
	}
	if nodeInfo != nil {
		if nodeInfo.NodeName != "" {
			nodeName = nodeInfo.NodeName
		}
		if nodeInfo.InstanceURL != "" {
			instanceURL = nodeInfo.InstanceURL
		}
	}

	testAlert := &alerts.Alert{
		ID:           "test-alert",
		Type:         "cpu",
		Level:        "warning",
		ResourceID:   "test-email-config",
		ResourceName: "Email Configuration Test",
		Node:         nodeName,
		Instance:     instanceURL,
		Message:      "This is a test alert to verify your email notification settings are working correctly",
		Value:        85.5,
		Threshold:    80,
		StartTime:    time.Now(),
		LastSeen:     time.Now(),
		Metadata: map[string]interface{}{
			"resourceType": "test",
		},
	}

	switch method {
	case "email":
		if config == nil {
			return fmt.Errorf("email configuration is required")
		}

		log.Info().
			Bool("enabled", config.Enabled).
			Str("smtp", config.SMTPHost).
			Int("port", config.SMTPPort).
			Str("from", config.From).
			Int("toCount", len(config.To)).
			Strs("to", config.To).
			Bool("smtpEmpty", config.SMTPHost == "").
			Bool("fromEmpty", config.From == "").
			Msg("Testing email notification with provided config")

		if !config.Enabled {
			return fmt.Errorf("email notifications are not enabled in the provided configuration")
		}

		if config.SMTPHost == "" || config.From == "" {
			return fmt.Errorf("email configuration is incomplete: SMTP host and from address are required")
		}

		// Generate email using template
		subject, htmlBody, textBody := EmailTemplate([]*alerts.Alert{testAlert}, true)

		// Send using provided config and return any error
		return n.sendHTMLEmailWithError(subject, htmlBody, textBody, *config)

	default:
		return fmt.Errorf("unsupported method for config-based testing: %s", method)
	}
}

// Stop gracefully stops the notification manager
func (n *NotificationManager) Stop() {
	n.mu.Lock()
	defer n.mu.Unlock()

	// Cancel any pending group timer
	if n.groupTimer != nil {
		n.groupTimer.Stop()
		n.groupTimer = nil
	}

	// Clear pending alerts
	n.pendingAlerts = nil

	log.Info().Msg("NotificationManager stopped")
}
