package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func notificationPreferenceRequest(
	t *testing.T,
	method string,
	preferences map[string]string,
) *http.Request {
	t.Helper()

	member, err := testHandler.Queries.GetMemberByUserAndWorkspace(
		context.Background(),
		db.GetMemberByUserAndWorkspaceParams{
			UserID:      parseUUID(testUserID),
			WorkspaceID: parseUUID(testWorkspaceID),
		},
	)
	if err != nil {
		t.Fatalf("load test member: %v", err)
	}

	req := newRequest(method, "/api/notification-preferences", map[string]any{
		"preferences": preferences,
	})
	return req.WithContext(
		middleware.SetMemberContext(req.Context(), testWorkspaceID, member),
	)
}

func TestPatchNotificationPreferencesMergesWithoutReplacing(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `
		DELETE FROM notification_preference
		WHERE workspace_id = $1 AND user_id = $2
	`, testWorkspaceID, testUserID); err != nil {
		t.Fatalf("reset notification preference: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `
			DELETE FROM notification_preference
			WHERE workspace_id = $1 AND user_id = $2
		`, testWorkspaceID, testUserID)
	})

	testutil.Call(t, testHandler.UpdateNotificationPreferences,
		notificationPreferenceRequest(t, http.MethodPut, map[string]string{
			"status_changes": "muted",
		}),
	).Want(http.StatusOK)

	patchRecorder := testutil.Call(t, testHandler.PatchNotificationPreferences,
		notificationPreferenceRequest(t, http.MethodPatch, map[string]string{
			"comments": "muted",
		}),
	).Want(http.StatusOK)

	var response struct {
		WorkspaceID string            `json:"workspace_id"`
		Preferences map[string]string `json:"preferences"`
	}
	if err := json.NewDecoder(patchRecorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode patch response: %v", err)
	}
	if response.WorkspaceID != testWorkspaceID {
		t.Fatalf("workspace_id = %q, want %q", response.WorkspaceID, testWorkspaceID)
	}
	if response.Preferences["status_changes"] != "muted" {
		t.Fatalf("status_changes was replaced: %#v", response.Preferences)
	}
	if response.Preferences["comments"] != "muted" {
		t.Fatalf("comments patch missing: %#v", response.Preferences)
	}

	testutil.Call(t, testHandler.PatchNotificationPreferences,
		notificationPreferenceRequest(t, http.MethodPatch, map[string]string{
			"status_changes": "all",
		}),
	).Want(http.StatusOK)

	var persisted []byte
	if err := testPool.QueryRow(ctx, `
		SELECT preferences
		FROM notification_preference
		WHERE workspace_id = $1 AND user_id = $2
	`, testWorkspaceID, testUserID).Scan(&persisted); err != nil {
		t.Fatalf("load persisted preference: %v", err)
	}
	var preferences map[string]string
	if err := json.Unmarshal(persisted, &preferences); err != nil {
		t.Fatalf("decode persisted preference: %v", err)
	}
	if preferences["status_changes"] != "all" || preferences["comments"] != "muted" {
		t.Fatalf("unexpected persisted preferences: %#v", preferences)
	}
}

// TestPatchNotificationPreferencesAcceptsMentions covers the group added by
// #6468. The whitelist rejects unknown keys, so a client shipped ahead of the
// server would 400 on this key — the reason the server must deploy first.
func TestPatchNotificationPreferencesAcceptsMentions(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `
			DELETE FROM notification_preference
			WHERE workspace_id = $1 AND user_id = $2
		`, testWorkspaceID, testUserID)
	})

	recorder := testutil.Call(t, testHandler.PatchNotificationPreferences,
		notificationPreferenceRequest(t, http.MethodPatch, map[string]string{
			"mentions": "muted",
		}),
	).Want(http.StatusOK)

	var response struct {
		Preferences map[string]string `json:"preferences"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Preferences["mentions"] != "muted" {
		t.Fatalf("mentions not persisted: %#v", response.Preferences)
	}
}

func TestPatchNotificationPreferencesRejectsUnknownGroups(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	testutil.Call(t, testHandler.PatchNotificationPreferences,
		notificationPreferenceRequest(t, http.MethodPatch, map[string]string{
			"unknown_group": "muted",
		}),
	).Want(http.StatusBadRequest)
}

type fakePreferenceChatSender struct {
	webhook string
	text    string
	calls   int
	err     error
}

func (f *fakePreferenceChatSender) Send(_ context.Context, webhookURL, text string) error {
	f.calls++
	f.webhook = webhookURL
	f.text = text
	return f.err
}

func usePreferenceChatSender(t *testing.T, sender service.NotificationChatSender) {
	t.Helper()
	previous := testHandler.NotificationChatSender
	testHandler.NotificationChatSender = sender
	t.Cleanup(func() {
		testHandler.NotificationChatSender = previous
	})
}

func resetNotificationPreference(t *testing.T) {
	t.Helper()
	dbfx.Exec(t, `
		DELETE FROM notification_preference
		WHERE workspace_id = $1 AND user_id = $2
	`, testWorkspaceID, testUserID)
	dbfx.Cleanup(t, `
		DELETE FROM notification_preference
		WHERE workspace_id = $1 AND user_id = $2
	`, testWorkspaceID, testUserID)
}

func seedNotificationPreference(t *testing.T, preferences string) {
	t.Helper()
	resetNotificationPreference(t)
	dbfx.Insert(t, "notification_preference", testutil.Cols{
		"workspace_id": testWorkspaceID,
		"user_id":      testUserID,
		"preferences":  testutil.Raw("'" + preferences + "'::jsonb"),
	})
}

func readStoredNotificationPreferences(t *testing.T) map[string]string {
	t.Helper()
	var raw []byte
	dbfx.QueryRow(t, `
		SELECT preferences
		FROM notification_preference
		WHERE workspace_id = $1 AND user_id = $2
	`, testWorkspaceID, testUserID).Scan(&raw)
	var preferences map[string]string
	if err := json.Unmarshal(raw, &preferences); err != nil {
		t.Fatal(err)
	}
	return preferences
}

func TestNotificationPreferenceGoogleChatValidation(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	tests := []struct {
		name string
		url  string
	}{
		{"scheme", "http://chat.googleapis.com/v1/spaces/AAAA/messages?key=x&token=y"},
		{"host", "https://example.com/v1/spaces/AAAA/messages?key=x&token=y"},
		{"port", "https://chat.googleapis.com:443/v1/spaces/AAAA/messages?key=x&token=y"},
		{"path", "https://chat.googleapis.com/v1/spaces/AAAA?key=x&token=y"},
		{"missing token", "https://chat.googleapis.com/v1/spaces/AAAA/messages?key=x"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resetNotificationPreference(t)
			sender := &fakePreferenceChatSender{}
			usePreferenceChatSender(t, sender)

			testutil.Call(t, testHandler.PatchNotificationPreferences,
				notificationPreferenceRequest(t, http.MethodPatch, map[string]string{
					"google_chat_webhook": test.url,
				}),
			).Want(http.StatusBadRequest)
			if sender.calls != 0 {
				t.Fatalf("confirmation calls = %d, want 0", sender.calls)
			}
		})
	}
}

func TestNotificationPreferenceGoogleChatAcceptedAndMasked(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	resetNotificationPreference(t)
	sender := &fakePreferenceChatSender{}
	usePreferenceChatSender(t, sender)
	webhook := "https://chat.googleapis.com/v1/spaces/AAAA_123/messages?key=test-key&token=test-token"
	masked := "https://chat.googleapis.com/v1/spaces/AAAA…/messages?••••"

	var patched struct {
		Preferences map[string]string `json:"preferences"`
	}
	testutil.Call(t, testHandler.PatchNotificationPreferences,
		notificationPreferenceRequest(t, http.MethodPatch, map[string]string{
			"google_chat_webhook": webhook,
			"email_notifications": "muted",
		}),
	).Want(http.StatusOK).JSON(&patched)
	if patched.Preferences["google_chat_webhook"] != masked {
		t.Fatalf("PATCH webhook = %q", patched.Preferences["google_chat_webhook"])
	}
	if sender.calls != 1 || sender.webhook != webhook {
		t.Fatalf("confirmation = %#v", sender)
	}
	if patched.Preferences["email_notifications"] != "muted" {
		t.Fatalf("email_notifications = %q", patched.Preferences["email_notifications"])
	}
	if !strings.Contains(sender.text, "Handler Tests") {
		t.Fatalf("confirmation text = %q", sender.text)
	}
	if stored := readStoredNotificationPreferences(t)["google_chat_webhook"]; stored != webhook {
		t.Fatalf("stored webhook = %q", stored)
	}

	var fetched struct {
		Preferences map[string]string `json:"preferences"`
	}
	testutil.Call(t, testHandler.GetNotificationPreferences,
		notificationPreferenceRequest(t, http.MethodGet, map[string]string{}),
	).Want(http.StatusOK).JSON(&fetched)
	if fetched.Preferences["google_chat_webhook"] != masked {
		t.Fatalf("GET webhook = %q", fetched.Preferences["google_chat_webhook"])
	}
}

func TestNotificationPreferenceMaskedEchoPreservesWebhookOnPut(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	webhook := "https://chat.googleapis.com/v1/spaces/AAAA_123/messages?key=test-key&token=test-token"
	seedNotificationPreference(t, `{"google_chat_webhook":"`+webhook+`","mentions":"muted"}`)
	sender := &fakePreferenceChatSender{}
	usePreferenceChatSender(t, sender)

	var response struct {
		Preferences map[string]string `json:"preferences"`
	}
	testutil.Call(t, testHandler.UpdateNotificationPreferences,
		notificationPreferenceRequest(t, http.MethodPut, map[string]string{
			"google_chat_webhook": "https://chat.googleapis.com/v1/spaces/AAAA…/messages?••••",
			"comments":            "muted",
		}),
	).Want(http.StatusOK).JSON(&response)

	if sender.calls != 0 {
		t.Fatalf("masked echo sent %d confirmations", sender.calls)
	}
	if response.Preferences["google_chat_webhook"] != "https://chat.googleapis.com/v1/spaces/AAAA…/messages?••••" {
		t.Fatalf("response webhook = %q", response.Preferences["google_chat_webhook"])
	}
	stored := readStoredNotificationPreferences(t)
	if stored["google_chat_webhook"] != webhook || stored["comments"] != "muted" {
		t.Fatalf("stored preferences = %#v", stored)
	}
	if _, ok := stored["mentions"]; ok {
		t.Fatalf("replace-all PUT retained omitted mentions: %#v", stored)
	}
}

func TestNotificationPreferencePutWithoutWebhookKeepsIt(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	webhook := "https://chat.googleapis.com/v1/spaces/AAAA_123/messages?key=test-key&token=test-token"
	seedNotificationPreference(t, `{"google_chat_webhook":"`+webhook+`","mentions":"muted"}`)
	sender := &fakePreferenceChatSender{}
	usePreferenceChatSender(t, sender)

	// An installed client that predates the webhook key replaces all prefs.
	testutil.Call(t, testHandler.UpdateNotificationPreferences,
		notificationPreferenceRequest(t, http.MethodPut, map[string]string{"comments": "muted"}),
	).Want(http.StatusOK)

	if sender.calls != 0 {
		t.Fatalf("unchanged webhook sent %d confirmations", sender.calls)
	}
	stored := readStoredNotificationPreferences(t)
	if stored["google_chat_webhook"] != webhook || stored["comments"] != "muted" {
		t.Fatalf("stored preferences = %#v", stored)
	}
	if _, ok := stored["mentions"]; ok {
		t.Fatalf("replace-all PUT retained omitted mentions: %#v", stored)
	}
}

func TestNotificationPreferenceGoogleChatClearing(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	webhook := "https://chat.googleapis.com/v1/spaces/AAAA/messages?key=test-key&token=test-token"
	seedNotificationPreference(t, `{"google_chat_webhook":"`+webhook+`"}`)
	sender := &fakePreferenceChatSender{}
	usePreferenceChatSender(t, sender)

	testutil.Call(t, testHandler.PatchNotificationPreferences,
		notificationPreferenceRequest(t, http.MethodPatch, map[string]string{
			"google_chat_webhook": "",
		}),
	).Want(http.StatusOK)
	if sender.calls != 0 {
		t.Fatalf("clearing sent %d confirmations", sender.calls)
	}
	if stored := readStoredNotificationPreferences(t)["google_chat_webhook"]; stored != "" {
		t.Fatalf("stored webhook after clear = %q", stored)
	}
}

func TestNotificationPreferenceGoogleChatConfirmationFailureDoesNotPersist(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	resetNotificationPreference(t)
	sender := &fakePreferenceChatSender{err: errors.New("confirmation failed")}
	usePreferenceChatSender(t, sender)
	webhook := "https://chat.googleapis.com/v1/spaces/AAAA/messages?key=test-key&token=test-token"

	response := testutil.Call(t, testHandler.PatchNotificationPreferences,
		notificationPreferenceRequest(t, http.MethodPatch, map[string]string{
			"google_chat_webhook": webhook,
		}),
	).Want(http.StatusBadRequest)
	if !strings.Contains(response.Text(), "failed to connect Google Chat webhook") {
		t.Fatalf("response = %s", response.Text())
	}
	_, err := testHandler.Queries.GetNotificationPreference(context.Background(), db.GetNotificationPreferenceParams{
		WorkspaceID: parseUUID(testWorkspaceID),
		UserID:      parseUUID(testUserID),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("preference was stored after failed confirmation: %v", err)
	}
}
