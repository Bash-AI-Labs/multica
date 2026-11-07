package service

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type notificationEmailCall struct {
	to      string
	subject string
	body    string
}

type fakeNotificationEmailSender struct {
	configured bool
	calls      []notificationEmailCall
	err        error
}

func (f *fakeNotificationEmailSender) Configured() bool {
	return f.configured
}

func (f *fakeNotificationEmailSender) SendHTML(to, subject, body string) error {
	f.calls = append(f.calls, notificationEmailCall{to: to, subject: subject, body: body})
	return f.err
}

type notificationChatCall struct {
	webhook string
	text    string
}

type fakeNotificationChatSender struct {
	calls []notificationChatCall
	err   error
}

func (f *fakeNotificationChatSender) Send(_ context.Context, webhookURL, text string) error {
	f.calls = append(f.calls, notificationChatCall{webhook: webhookURL, text: text})
	return f.err
}

func TestNotificationMessageBuildersEscapeAndDescribeItems(t *testing.T) {
	batch := notificationDeliveryBatch{
		WorkspaceName: "Bash <POS> & Co",
		SettingsURL:   "https://app.example.test/bash/settings?a=1&b=2",
		Items: []notificationDeliveryItem{{
			Type:            "mentioned",
			ActorName:       "Dillon <script>\r\n",
			IssueIdentifier: "BAS-12",
			IssueTitle:      "Fix <login> & logout",
			IssueURL:        "https://app.example.test/bash/issues/BAS-12",
		}},
	}

	subject, body := buildNotificationEmail(batch)
	if strings.ContainsAny(subject, "\r\n") {
		t.Fatalf("subject contains a line break: %q", subject)
	}
	if subject != "Dillon <script> mentioned you in BAS-12: Fix <login> & logout" {
		t.Fatalf("subject = %q", subject)
	}
	for _, want := range []string{
		"Dillon &lt;script&gt;",
		"BAS-12: Fix &lt;login&gt; &amp; logout",
		"Bash &lt;POS&gt; &amp; Co",
		"a=1&amp;b=2",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("email body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "<script>") {
		t.Fatalf("email body contains unescaped user HTML:\n%s", body)
	}

	chat := buildGoogleChatNotification(batch)
	for _, want := range []string{
		"Bash &lt;POS&gt; &amp; Co",
		"Dillon &lt;script&gt;",
		"BAS-12: Fix &lt;login&gt; &amp; logout",
		"<https://app.example.test/bash/issues/BAS-12|",
	} {
		if !strings.Contains(chat, want) {
			t.Fatalf("Google Chat message missing %q:\n%s", want, chat)
		}
	}
}

func TestNotificationDeliveryConfigUsesEmptyDefaultsAndAcceptsZeroDelay(t *testing.T) {
	t.Setenv("NOTIFICATION_DELIVERY_ENABLED", "")
	t.Setenv("NOTIFICATION_DELIVERY_DELAY", "")
	t.Setenv("NOTIFICATION_DELIVERY_TYPES", "")
	t.Setenv("MULTICA_APP_URL", "")
	t.Setenv("FRONTEND_ORIGIN", "https://frontend.example.test/")

	config := NotificationDeliveryConfigFromEnv()
	if !config.Enabled {
		t.Fatal("empty enabled value should use the true default")
	}
	if config.Delay != 10*time.Minute {
		t.Fatalf("delay = %s", config.Delay)
	}
	if strings.Join(config.Types, ",") != "mentioned,issue_assigned,agent_blocked" {
		t.Fatalf("types = %#v", config.Types)
	}
	if config.AppURL != "https://frontend.example.test" {
		t.Fatalf("app URL = %q", config.AppURL)
	}

	t.Setenv("NOTIFICATION_DELIVERY_DELAY", "0")
	if zero := NotificationDeliveryConfigFromEnv().Delay; zero != 0 {
		t.Fatalf("zero delay = %s", zero)
	}
}

func TestNotificationEmailSubjectsByTypeAndBatchSize(t *testing.T) {
	tests := []struct {
		kind string
		want string
	}{
		{"mentioned", "Dillon mentioned you in BAS-12: Fix login"},
		{"issue_assigned", "Dillon assigned you BAS-12: Fix login"},
		{"agent_blocked", "Dillon needs your input on BAS-12: Fix login"},
	}
	for _, test := range tests {
		t.Run(test.kind, func(t *testing.T) {
			got := notificationEmailSubject(notificationDeliveryBatch{
				WorkspaceName: "BashPOS",
				Items: []notificationDeliveryItem{{
					Type:            test.kind,
					ActorName:       "Dillon",
					IssueIdentifier: "BAS-12",
					IssueTitle:      "Fix login",
				}},
			})
			if got != test.want {
				t.Fatalf("subject = %q, want %q", got, test.want)
			}
		})
	}

	got := notificationEmailSubject(notificationDeliveryBatch{
		WorkspaceName: "BashPOS",
		Items:         []notificationDeliveryItem{{}, {}, {}},
	})
	if got != "3 new notifications in BashPOS" {
		t.Fatalf("batch subject = %q", got)
	}
}

func TestGroupNotificationDeliveriesUsesWorkspaceAndRecipient(t *testing.T) {
	workspaceA := pgUUID(1)
	workspaceB := pgUUID(2)
	recipientA := pgUUID(3)
	recipientB := pgUUID(4)
	rows := []db.ListPendingNotificationDeliveriesRow{
		deliveryRow(workspaceA, recipientA, "mentioned", 12),
		deliveryRow(workspaceA, recipientA, "issue_assigned", 13),
		deliveryRow(workspaceA, recipientB, "agent_blocked", 14),
		deliveryRow(workspaceB, recipientA, "mentioned", 15),
	}

	batches := groupNotificationDeliveries(rows, "https://app.example.test/")
	if len(batches) != 3 {
		t.Fatalf("group count = %d, want 3", len(batches))
	}
	if len(batches[0].Items) != 2 {
		t.Fatalf("first group item count = %d, want 2", len(batches[0].Items))
	}
	if batches[0].Items[0].IssueURL != "https://app.example.test/bash/issues/BAS-12" {
		t.Fatalf("issue URL = %q", batches[0].Items[0].IssueURL)
	}
	if batches[0].SettingsURL != "https://app.example.test/bash/settings" {
		t.Fatalf("settings URL = %q", batches[0].SettingsURL)
	}
}

func TestNotificationDeliveryWorkerFiltersAndBatches(t *testing.T) {
	pool := sharedTestPool(t)
	fx := testutil.New(pool, "", "")
	suffix := uuid.NewString()
	recipientA := fx.User(t, "Recipient A", "recipient-a-"+suffix+"@example.test")
	recipientB := fx.User(t, "Recipient B", "recipient-b-"+suffix+"@example.test")
	nonMember := fx.User(t, "Former Member", "former-"+suffix+"@example.test")
	actor := fx.User(t, "Dillon", "actor-"+suffix+"@example.test")
	workspaceID := fx.Workspace(t, "BashPOS", "bash-"+suffix, testutil.Cols{"issue_prefix": "BAS"})
	fx.WorkspaceID = workspaceID
	fx.UserID = actor
	fx.Member(t, workspaceID, recipientA, "member")
	fx.Member(t, workspaceID, recipientB, "member")
	fx.Member(t, workspaceID, actor, "member")
	issueID := fx.Issue(t, "Fix login", testutil.Cols{"number": 12})
	runtimeID := fx.Runtime(t, "Notification agent runtime")
	agentID := fx.Agent(t, "Build Agent", runtimeID)

	webhookA := "https://chat.googleapis.com/v1/spaces/AAAA/messages?key=test-key&token=test-token"
	webhookB := "https://chat.googleapis.com/v1/spaces/BBBB/messages?key=test-key&token=test-token"
	fx.Insert(t, "notification_preference", testutil.Cols{
		"workspace_id": workspaceID,
		"user_id":      recipientA,
		"preferences":  testutil.Raw(`'{"google_chat_webhook":"` + webhookA + `"}'::jsonb`),
	})
	fx.Insert(t, "notification_preference", testutil.Cols{
		"workspace_id": workspaceID,
		"user_id":      recipientB,
		"preferences": testutil.Raw(`'{"email_notifications":"muted","google_chat_webhook":"` +
			webhookB + `"}'::jsonb`),
	})

	base := time.Date(2099, 1, 1, 12, 0, 0, 0, time.UTC)
	insertInbox := func(recipientID, kind string, createdAt time.Time, over testutil.Cols) {
		t.Helper()
		cols := testutil.Cols{
			"workspace_id":   workspaceID,
			"recipient_type": "member",
			"recipient_id":   recipientID,
			"type":           kind,
			"severity":       "info",
			"issue_id":       issueID,
			"title":          "notification",
			"body":           "",
			"actor_type":     "member",
			"actor_id":       actor,
			"details":        testutil.Raw(`'{}'::jsonb`),
			"created_at":     createdAt,
		}
		for key, value := range over {
			cols[key] = value
		}
		fx.Insert(t, "inbox_item", cols)
	}

	eligibleAt := base.Add(-9 * time.Minute)
	insertInbox(recipientA, "mentioned", eligibleAt, nil)
	insertInbox(recipientA, "issue_assigned", eligibleAt.Add(time.Second), nil)
	insertInbox(recipientB, "agent_blocked", eligibleAt, testutil.Cols{
		"actor_type": "agent",
		"actor_id":   agentID,
	})
	insertInbox(recipientA, "mentioned", eligibleAt, testutil.Cols{"read": true})
	insertInbox(recipientA, "mentioned", eligibleAt, testutil.Cols{"archived": true})
	insertInbox(recipientA, "new_comment", eligibleAt, nil)
	insertInbox(recipientA, "mentioned", base.Add(-11*time.Minute), nil)
	insertInbox(recipientA, "mentioned", base.Add(-6*time.Minute), nil)
	insertInbox(nonMember, "mentioned", eligibleAt, nil)

	email := &fakeNotificationEmailSender{configured: true}
	chat := &fakeNotificationChatSender{}
	worker := NewNotificationDeliveryService(
		db.New(pool),
		email,
		chat,
		NotificationDeliveryConfig{
			Enabled:      true,
			Delay:        10 * time.Minute,
			Types:        append([]string(nil), defaultNotificationDeliveryTypes...),
			TickInterval: time.Minute,
			AppURL:       "https://app.example.test",
		},
	)
	worker.Initialize(base)
	if err := worker.DeliverAt(context.Background(), base.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	if len(email.calls) != 1 {
		t.Fatalf("email call count = %d, want 1", len(email.calls))
	}
	if email.calls[0].to != "recipient-a-"+suffix+"@example.test" {
		t.Fatalf("email recipient = %q", email.calls[0].to)
	}
	if email.calls[0].subject != "2 new notifications in BashPOS" {
		t.Fatalf("email subject = %q", email.calls[0].subject)
	}
	if len(chat.calls) != 2 {
		t.Fatalf("chat call count = %d, want 2", len(chat.calls))
	}
	deliveredWebhooks := map[string]bool{}
	for _, call := range chat.calls {
		deliveredWebhooks[call.webhook] = true
	}
	if !deliveredWebhooks[webhookA] || !deliveredWebhooks[webhookB] {
		t.Fatalf("chat webhooks = %#v", deliveredWebhooks)
	}

	if err := worker.DeliverAt(context.Background(), base.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(email.calls) != 1 || len(chat.calls) != 2 {
		t.Fatalf("a later tick redelivered old rows: email=%d chat=%d", len(email.calls), len(chat.calls))
	}
}

func TestGoogleChatWebhookValidationAndMasking(t *testing.T) {
	valid := "https://chat.googleapis.com/v1/spaces/AAAA_123/messages?key=test-key&token=test-token&threadKey=x"
	if _, err := ValidateGoogleChatWebhookURL(valid); err != nil {
		t.Fatalf("valid webhook rejected: %v", err)
	}
	if got := MaskGoogleChatWebhookURL(valid); got != "https://chat.googleapis.com/v1/spaces/AAAA…/messages?••••" {
		t.Fatalf("masked webhook = %q", got)
	}
	if got := GoogleChatSpaceID(valid); got != "AAAA_123" {
		t.Fatalf("space id = %q", got)
	}
	if _, err := ValidateGoogleChatWebhookURL("https://example.com/v1/spaces/AAAA/messages?key=x&token=y"); err == nil {
		t.Fatal("bad host accepted")
	}
}

func TestGoogleChatSenderUsesInjectedRestrictedClient(t *testing.T) {
	webhook := "https://chat.googleapis.com/v1/spaces/AAAA/messages?key=secret-key&token=secret-token"
	var requestBody string
	sender := &restrictedGoogleChatSender{
		newClient: func(endpoint *url.URL, timeout time.Duration, allowPrivate bool) (*http.Client, error) {
			if endpoint.Host != "chat.googleapis.com" || timeout != 10*time.Second || allowPrivate {
				t.Fatalf("client config: endpoint=%s timeout=%s allowPrivate=%v", endpoint.Host, timeout, allowPrivate)
			}
			return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.Method != http.MethodPost || request.Header.Get("Content-Type") != "application/json" {
					t.Fatalf("request = %s content-type=%q", request.Method, request.Header.Get("Content-Type"))
				}
				if got := request.URL.String(); got != webhook {
					t.Fatalf("request URL = %s, want %s", got, webhook)
				}
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Fatal(err)
				}
				requestBody = string(body)
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("ok")),
					Header:     make(http.Header),
				}, nil
			})}, nil
		},
	}

	if err := sender.Send(context.Background(), webhook, "hello <team>"); err != nil {
		t.Fatal(err)
	}
	if requestBody != `{"text":"hello \u003cteam\u003e"}` {
		t.Fatalf("request body = %s", requestBody)
	}
}

func TestGoogleChatRequestURLUsesConstantOrigin(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{
			raw:  "https://chat.googleapis.com/v1/spaces/AAAA/messages?key=k&token=t",
			want: "https://chat.googleapis.com/v1/spaces/AAAA/messages?key=k&token=t",
		},
		{
			raw:  "https://CHAT.googleapis.com/v1/spaces/AA_b-9/messages?token=t%2Bx&key=k&threadKey=x",
			want: "https://chat.googleapis.com/v1/spaces/AA_b-9/messages?key=k&threadKey=x&token=t%2Bx",
		},
	} {
		endpoint, err := ValidateGoogleChatWebhookURL(tc.raw)
		if err != nil {
			t.Fatalf("validate %s: %v", tc.raw, err)
		}
		got, err := googleChatRequestURL(endpoint)
		if err != nil || got != tc.want {
			t.Fatalf("googleChatRequestURL(%s) = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func pgUUID(last byte) pgtype.UUID {
	var bytes [16]byte
	bytes[15] = last
	return pgtype.UUID{Bytes: bytes, Valid: true}
}

func deliveryRow(
	workspaceID pgtype.UUID,
	recipientID pgtype.UUID,
	kind string,
	number int32,
) db.ListPendingNotificationDeliveriesRow {
	slug := "bash"
	name := "BashPOS"
	prefix := "BAS"
	if workspaceID.Bytes[15] == 2 {
		slug = "other"
		name = "Other"
		prefix = "OTH"
	}
	return db.ListPendingNotificationDeliveriesRow{
		WorkspaceID:    workspaceID,
		RecipientID:    recipientID,
		Type:           kind,
		RecipientEmail: "person@example.test",
		RecipientName:  "Person",
		WorkspaceSlug:  slug,
		WorkspaceName:  name,
		IssuePrefix:    prefix,
		IssueNumber:    pgtype.Int4{Int32: number, Valid: true},
		IssueTitle:     pgtype.Text{String: "Fix login", Valid: true},
		ActorName:      "Dillon",
		Preferences:    []byte(`{}`),
	}
}
