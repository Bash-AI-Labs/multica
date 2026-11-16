package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/httptransport"
)

const (
	defaultNotificationDeliveryDelay = 10 * time.Minute
	notificationDeliveryTickInterval = time.Minute
	googleChatRequestTimeout         = 10 * time.Second
	googleChatMessageRuneBudget      = 4000
	notificationFieldRuneLimit       = 200
)

var (
	googleChatWebhookPathPattern = regexp.MustCompile(`^/v1/spaces/([A-Za-z0-9_-]+)/messages$`)
	// markdownLink matches markdown links and images, including Multica
	// mentions such as [@Ada](mention://member/<id>).
	markdownLink = regexp.MustCompile(`!?\[([^\]]*)\]\([^)\s]+\)`)
)

type NotificationDeliveryConfig struct {
	Enabled bool
	Delay   time.Duration
	// Types narrows delivery to these inbox item types; empty delivers all.
	Types        []string
	TickInterval time.Duration
	AppURL       string
}

func NotificationDeliveryConfigFromEnv() NotificationDeliveryConfig {
	return NotificationDeliveryConfig{
		Enabled:      notificationDeliveryBool("NOTIFICATION_DELIVERY_ENABLED", true),
		Delay:        notificationDeliveryDelay(os.Getenv("NOTIFICATION_DELIVERY_DELAY")),
		Types:        notificationDeliveryTypes(os.Getenv("NOTIFICATION_DELIVERY_TYPES")),
		TickInterval: notificationDeliveryTickInterval,
		AppURL:       notificationDeliveryAppURL(),
	}
}

func notificationDeliveryBool(name string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		slog.Warn("invalid notification delivery boolean; using default",
			"name", name,
			"value", raw,
			"default", fallback,
		)
		return fallback
	}
	return value
}

func notificationDeliveryDelay(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultNotificationDeliveryDelay
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value < 0 {
		slog.Warn("invalid notification delivery delay; using default",
			"value", raw,
			"default", defaultNotificationDeliveryDelay.String(),
		)
		return defaultNotificationDeliveryDelay
	}
	return value
}

// notificationDeliveryTypes parses NOTIFICATION_DELIVERY_TYPES. Empty or "all"
// returns nil, which delivers every inbox notification.
func notificationDeliveryTypes(raw string) []string {
	seen := map[string]bool{}
	var types []string
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if strings.EqualFold(item, "all") {
			return nil
		}
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		types = append(types, item)
	}
	return types
}

func notificationDeliveryAppURL() string {
	for _, name := range []string{"MULTICA_APP_URL", "FRONTEND_ORIGIN"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return strings.TrimRight(value, "/")
		}
	}
	return "https://multica.ai"
}

type NotificationEmailSender interface {
	Configured() bool
	SendHTML(to, subject, htmlBody string) error
}

type NotificationChatSender interface {
	Send(ctx context.Context, webhookURL, text string) error
}

type notificationDeliveryQuerier interface {
	ListPendingNotificationDeliveries(
		context.Context,
		db.ListPendingNotificationDeliveriesParams,
	) ([]db.ListPendingNotificationDeliveriesRow, error)
}

type NotificationDeliveryService struct {
	queries   notificationDeliveryQuerier
	email     NotificationEmailSender
	chat      NotificationChatSender
	config    NotificationDeliveryConfig
	watermark time.Time
}

func NewNotificationDeliveryService(
	queries notificationDeliveryQuerier,
	email NotificationEmailSender,
	chat NotificationChatSender,
	config NotificationDeliveryConfig,
) *NotificationDeliveryService {
	if config.TickInterval <= 0 {
		config.TickInterval = notificationDeliveryTickInterval
	}
	if config.AppURL == "" {
		config.AppURL = "https://multica.ai"
	}
	return &NotificationDeliveryService{
		queries: queries,
		email:   email,
		chat:    chat,
		config:  config,
	}
}

func (s *NotificationDeliveryService) Initialize(now time.Time) {
	s.watermark = now.Add(-s.config.Delay)
}

func (s *NotificationDeliveryService) Run(ctx context.Context) {
	if !s.config.Enabled {
		return
	}
	s.Initialize(time.Now())
	ticker := time.NewTicker(s.config.TickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := s.DeliverAt(ctx, now); err != nil {
				slog.Warn("notification delivery tick failed", "error", err)
			}
		}
	}
}

func (s *NotificationDeliveryService) DeliverAt(ctx context.Context, now time.Time) error {
	before := now.Add(-s.config.Delay)
	if s.watermark.IsZero() {
		s.watermark = before
		return nil
	}
	if !before.After(s.watermark) {
		return nil
	}

	rows, err := s.queries.ListPendingNotificationDeliveries(ctx, db.ListPendingNotificationDeliveriesParams{
		Types:  s.config.Types,
		After:  pgtype.Timestamptz{Time: s.watermark, Valid: true},
		Before: pgtype.Timestamptz{Time: before, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("list pending notification deliveries: %w", err)
	}

	batches := groupNotificationDeliveries(rows, s.config.AppURL)
	for _, batch := range batches {
		if s.email != nil && s.email.Configured() && batch.Preferences["email_notifications"] != "muted" {
			subject, body := buildNotificationEmail(batch)
			if err := s.email.SendHTML(batch.RecipientEmail, subject, body); err != nil {
				slog.Warn("notification email delivery failed",
					"workspace_id", batch.WorkspaceID,
					"recipient_id", batch.RecipientID,
					"item_count", len(batch.Items),
					"error", err,
				)
			} else {
				slog.Info("notification delivered", "channel", "email",
					"workspace_id", batch.WorkspaceID, "recipient_id", batch.RecipientID, "item_count", len(batch.Items))
			}
		}

		webhookURL := strings.TrimSpace(batch.Preferences["google_chat_webhook"])
		if webhookURL == "" || s.chat == nil {
			continue
		}
		if err := s.chat.Send(ctx, webhookURL, buildGoogleChatNotification(batch)); err != nil {
			slog.Warn("Google Chat notification delivery failed",
				"workspace_id", batch.WorkspaceID,
				"recipient_id", batch.RecipientID,
				"space_id", GoogleChatSpaceID(webhookURL),
				"item_count", len(batch.Items),
				"error", err,
			)
		} else {
			slog.Info("notification delivered", "channel", "google_chat",
				"workspace_id", batch.WorkspaceID, "recipient_id", batch.RecipientID, "item_count", len(batch.Items))
		}
	}

	s.watermark = before
	return nil
}

type notificationDeliveryItem struct {
	Type            string
	ActorName       string
	Body            string
	Details         map[string]string
	IssueIdentifier string
	IssueTitle      string
	IssueURL        string
}

type notificationDeliveryBatch struct {
	WorkspaceID    string
	WorkspaceName  string
	WorkspaceSlug  string
	RecipientID    string
	RecipientName  string
	RecipientEmail string
	SettingsURL    string
	Preferences    map[string]string
	Items          []notificationDeliveryItem
}

func groupNotificationDeliveries(
	rows []db.ListPendingNotificationDeliveriesRow,
	appURL string,
) []notificationDeliveryBatch {
	batches := make([]notificationDeliveryBatch, 0)
	indexes := make(map[string]int)

	for _, row := range rows {
		workspaceID := util.UUIDToString(row.WorkspaceID)
		recipientID := util.UUIDToString(row.RecipientID)
		key := workspaceID + ":" + recipientID
		index, ok := indexes[key]
		if !ok {
			preferences := map[string]string{}
			if err := json.Unmarshal(row.Preferences, &preferences); err != nil {
				preferences = map[string]string{}
			}
			batches = append(batches, notificationDeliveryBatch{
				WorkspaceID:    workspaceID,
				WorkspaceName:  row.WorkspaceName,
				WorkspaceSlug:  row.WorkspaceSlug,
				RecipientID:    recipientID,
				RecipientName:  row.RecipientName,
				RecipientEmail: row.RecipientEmail,
				SettingsURL:    notificationSettingsURL(appURL, row.WorkspaceSlug),
				Preferences:    preferences,
			})
			index = len(batches) - 1
			indexes[key] = index
		}

		identifier := ""
		issueTitle := ""
		issueURL := ""
		if row.IssueNumber.Valid {
			identifier = fmt.Sprintf("%s-%d", row.IssuePrefix, row.IssueNumber.Int32)
			issueURL = notificationIssueURL(appURL, row.WorkspaceSlug, identifier)
		}
		if row.IssueTitle.Valid {
			issueTitle = row.IssueTitle.String
		}
		details := map[string]string{}
		if len(row.Details) != 0 {
			var raw map[string]any
			if err := json.Unmarshal(row.Details, &raw); err == nil {
				for key, value := range raw {
					if text, ok := value.(string); ok {
						details[key] = text
					}
				}
			}
		}
		batches[index].Items = append(batches[index].Items, notificationDeliveryItem{
			Type:            row.Type,
			ActorName:       row.ActorName,
			Body:            row.Body.String,
			Details:         details,
			IssueIdentifier: identifier,
			IssueTitle:      issueTitle,
			IssueURL:        issueURL,
		})
	}

	return batches
}

func notificationIssueURL(appURL, workspaceSlug, identifier string) string {
	return strings.TrimRight(appURL, "/") + "/" +
		url.PathEscape(workspaceSlug) + "/issues/" + url.PathEscape(identifier)
}

func notificationSettingsURL(appURL, workspaceSlug string) string {
	return strings.TrimRight(appURL, "/") + "/" +
		url.PathEscape(workspaceSlug) + "/settings"
}

// notificationAction says who did what, e.g. "Alex assigned you". Google
// Chat and email both use it, after the issue label.
func notificationAction(item notificationDeliveryItem) string {
	actor := item.ActorName
	if actor == "" {
		actor = "Someone"
	}
	switch item.Type {
	case "issue_assigned":
		return actor + " assigned you"
	case "unassigned":
		return actor + " unassigned you"
	case "assignee_changed":
		return actor + " changed the assignee"
	case "mentioned":
		return actor + " mentioned you"
	case "new_comment":
		return actor + " commented"
	case "issue_created":
		return actor + " created an issue"
	case "issue_subscribed":
		return actor + " subscribed you"
	case "review_requested":
		return actor + " requested your review"
	case "status_changed", "priority_changed", "start_date_changed", "due_date_changed":
		field := strings.TrimSuffix(strings.ReplaceAll(item.Type, "_", " "), " changed")
		from := changeValue(item.Type, item.Details["from"])
		to := changeValue(item.Type, item.Details["to"])
		return fmt.Sprintf("%s changed %s: %s → %s", actor, field, from, to)
	case "reaction_added":
		return actor + " reacted " + item.Details["emoji"]
	case "task_failed", "quick_create_failed":
		return actor + "'s run failed"
	case "task_completed", "agent_completed", "quick_create_done":
		return actor + " completed the run"
	case "agent_blocked":
		return actor + " is blocked"
	case "app_action_requested", "app_action_reminder":
		return actor + " needs your approval"
	case "app_connect_requested":
		return actor + " needs you to connect an app"
	case "children_done":
		return "All sub-issues are done"
	case "quick_create_unconfirmed":
		return "Issue creation could not be confirmed"
	case "autopilot_paused":
		return "Autopilot paused"
	case "autopilot_quota_exceeded":
		return "Autopilot quota exceeded"
	case "app_action_result":
		return actor + " reported an app action result"
	default:
		return strings.ReplaceAll(item.Type, "_", " ")
	}
}

// changeValue labels a changed field's value. Status and priority are stored as
// keys (in_progress), so they read as titles (In Progress); dates stay as-is.
func changeValue(kind, value string) string {
	if value == "" {
		return "unset"
	}
	if kind != "status_changed" && kind != "priority_changed" {
		return value
	}
	words := strings.Fields(strings.ReplaceAll(value, "_", " "))
	for i, word := range words {
		first, size := utf8.DecodeRuneInString(word)
		words[i] = string(unicode.ToUpper(first)) + word[size:]
	}
	return strings.Join(words, " ")
}

// plainText reads a markdown excerpt as text: a mention becomes its "@Name"
// label and a link its text, since neither channel renders the markup.
func plainText(value string) string {
	return markdownLink.ReplaceAllString(value, "$1")
}

func compactText(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func truncateCompactText(value string) string {
	value = compactText(value)
	runes := []rune(value)
	if len(runes) <= notificationFieldRuneLimit {
		return value
	}
	return string(runes[:notificationFieldRuneLimit-1]) + "…"
}

// notificationExcerpt is the item's comment or message text, one line.
func notificationExcerpt(item notificationDeliveryItem) string {
	return truncateCompactText(plainText(item.Body))
}

func notificationItemLabel(item notificationDeliveryItem) string {
	switch {
	case item.IssueIdentifier != "" && item.IssueTitle != "":
		return item.IssueIdentifier + ": " + item.IssueTitle
	case item.IssueIdentifier != "":
		return item.IssueIdentifier
	case item.IssueTitle != "":
		return item.IssueTitle
	default:
		return "New inbox notification"
	}
}

func buildNotificationEmail(batch notificationDeliveryBatch) (string, string) {
	subject := notificationEmailSubject(batch)
	safeWorkspace := html.EscapeString(batch.WorkspaceName)

	var items strings.Builder
	for _, item := range batch.Items {
		safeLabel := html.EscapeString(truncateCompactText(notificationItemLabel(item)))
		label := "<strong>" + safeLabel + "</strong>"
		if item.IssueURL != "" {
			label = `<a href="` + html.EscapeString(item.IssueURL) +
				`" style="color: inherit;"><strong>` + safeLabel + "</strong></a>"
		}
		line := label + " — " + html.EscapeString(truncateCompactText(notificationAction(item)))
		if excerpt := notificationExcerpt(item); excerpt != "" {
			line += " — " + html.EscapeString(excerpt)
		}
		fmt.Fprintf(&items, `<li style="margin: 0 0 12px;">%s</li>`, line)
	}

	body := fmt.Sprintf(
		`<div style="font-family: sans-serif; max-width: 560px; margin: 0 auto;">
			<h2>New notifications in %s</h2>
			<ul style="padding-left: 20px;">%s</ul>
			<p style="margin-top: 24px; color: #666; font-size: 14px;">
				<a href="%s">Change notification settings</a>
			</p>
		</div>`,
		safeWorkspace,
		items.String(),
		html.EscapeString(batch.SettingsURL),
	)
	return subject, body
}

func notificationEmailSubject(batch notificationDeliveryBatch) string {
	if len(batch.Items) != 1 {
		return fmt.Sprintf(
			"%d new notifications in %s",
			len(batch.Items),
			sanitizeSubjectField(batch.WorkspaceName),
		)
	}
	item := batch.Items[0]
	return sanitizeSubjectField(notificationAction(item)) + " — " +
		sanitizeSubjectField(notificationItemLabel(item))
}

// buildGoogleChatNotification renders a batch as one message: the workspace
// name in bold, then one line per item with the issue link, who did what and
// the comment excerpt. Lines that would exceed Google Chat's budget are counted
// in a trailing "and N more" line.
func buildGoogleChatNotification(batch notificationDeliveryBatch) string {
	if len(batch.Items) == 0 {
		return ""
	}
	var message strings.Builder
	if len(batch.Items) != 1 {
		fmt.Fprintf(&message, "*%d new notifications in ", len(batch.Items))
	} else {
		message.WriteString("*")
	}
	fmt.Fprintf(&message, "%s*", EscapeGoogleChatText(truncateCompactText(batch.WorkspaceName)))

	for index, item := range batch.Items {
		line := googleChatNotificationLine(item)
		remainingAfter := len(batch.Items) - index - 1
		candidateRunes := utf8.RuneCountInString(message.String()) + utf8.RuneCountInString(line)
		if candidateRunes <= googleChatMessageRuneBudget &&
			(remainingAfter == 0 ||
				candidateRunes+utf8.RuneCountInString(omittedItemsLine(remainingAfter)) <= googleChatMessageRuneBudget) {
			message.WriteString(line)
			continue
		}
		message.WriteString(omittedItemsLine(len(batch.Items) - index))
		break
	}
	return message.String()
}

func googleChatNotificationLine(item notificationDeliveryItem) string {
	label := EscapeGoogleChatText(truncateCompactText(notificationItemLabel(item)))
	if item.IssueURL != "" {
		label = "<" + item.IssueURL + "|" + label + ">"
	}
	line := "\n• " + label + " — " + EscapeGoogleChatText(truncateCompactText(notificationAction(item)))
	if excerpt := notificationExcerpt(item); excerpt != "" {
		line += " — " + EscapeGoogleChatText(excerpt)
	}
	return line
}

func omittedItemsLine(count int) string {
	if count <= 0 {
		return ""
	}
	return fmt.Sprintf("\n…and %d more in Multica", count)
}

func EscapeGoogleChatText(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	return strings.ReplaceAll(value, ">", "&gt;")
}

func BuildGoogleChatConfirmation(workspaceName string) string {
	return fmt.Sprintf(
		"Multica notifications are connected for *%s*.",
		EscapeGoogleChatText(workspaceName),
	)
}

var (
	ErrGoogleChatInvalidWebhookURL  = errors.New("Google Chat webhook must use https://chat.googleapis.com/v1/spaces/{space}/messages without a port, userinfo, or fragment")
	ErrGoogleChatMissingCredentials = errors.New("Google Chat webhook must include non-empty key and token parameters")
)

func ValidateGoogleChatWebhookURL(raw string) (*url.URL, error) {
	endpoint, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, ErrGoogleChatInvalidWebhookURL
	}
	if endpoint.Scheme != "https" ||
		!strings.EqualFold(endpoint.Hostname(), "chat.googleapis.com") ||
		endpoint.Host != endpoint.Hostname() ||
		endpoint.User != nil ||
		endpoint.Port() != "" ||
		endpoint.Fragment != "" {
		return nil, ErrGoogleChatInvalidWebhookURL
	}
	if !googleChatWebhookPathPattern.MatchString(endpoint.EscapedPath()) {
		return nil, ErrGoogleChatInvalidWebhookURL
	}
	query := endpoint.Query()
	if strings.TrimSpace(query.Get("key")) == "" || strings.TrimSpace(query.Get("token")) == "" {
		return nil, ErrGoogleChatMissingCredentials
	}
	return endpoint, nil
}

func MaskGoogleChatWebhookURL(raw string) string {
	endpoint, err := ValidateGoogleChatWebhookURL(raw)
	if err != nil {
		return ""
	}
	matches := googleChatWebhookPathPattern.FindStringSubmatch(endpoint.EscapedPath())
	if len(matches) != 2 {
		return ""
	}
	runes := []rune(matches[1])
	if len(runes) > 4 {
		runes = runes[:4]
	}
	return "https://chat.googleapis.com/v1/spaces/" + string(runes) + "…/messages?••••"
}

func GoogleChatSpaceID(raw string) string {
	endpoint, err := ValidateGoogleChatWebhookURL(raw)
	if err != nil {
		return ""
	}
	matches := googleChatWebhookPathPattern.FindStringSubmatch(endpoint.EscapedPath())
	if len(matches) != 2 {
		return ""
	}
	return matches[1]
}

// googleChatRequestURL rebuilds a validated webhook from a constant origin so
// the request host can never come from stored user input.
func googleChatRequestURL(endpoint *url.URL) (string, error) {
	matches := googleChatWebhookPathPattern.FindStringSubmatch(endpoint.EscapedPath())
	if len(matches) != 2 {
		return "", errors.New("Google Chat webhook path must match /v1/spaces/{space}/messages")
	}
	return "https://chat.googleapis.com/v1/spaces/" + url.PathEscape(matches[1]) +
		"/messages?" + endpoint.Query().Encode(), nil
}

type restrictedGoogleChatSender struct {
	newClient func(*url.URL, time.Duration, bool) (*http.Client, error)
}

func NewGoogleChatSender() NotificationChatSender {
	return &restrictedGoogleChatSender{newClient: httptransport.NewRestrictedClient}
}

func (s *restrictedGoogleChatSender) Send(ctx context.Context, webhookURL, text string) error {
	endpoint, err := ValidateGoogleChatWebhookURL(webhookURL)
	if err != nil {
		return err
	}
	client, err := s.newClient(endpoint, googleChatRequestTimeout, false)
	if err != nil {
		return errors.New("could not initialize Google Chat delivery")
	}

	requestURL, err := googleChatRequestURL(endpoint)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"text": text})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(payload))
	if err != nil {
		return errors.New("could not create Google Chat request")
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := client.Do(request)
	if err != nil {
		return errors.New("Google Chat webhook request failed")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("Google Chat webhook returned HTTP %d", response.StatusCode)
	}
	return nil
}
