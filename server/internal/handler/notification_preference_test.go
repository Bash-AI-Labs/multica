package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/inbox"
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
			"updates": "muted",
		}),
	).Want(http.StatusOK)

	patchRecorder := testutil.Call(t, testHandler.PatchNotificationPreferences,
		notificationPreferenceRequest(t, http.MethodPatch, map[string]string{
			"comments":     "muted",
			"chat_replies": "muted",
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
	if response.Preferences["updates"] != "muted" {
		t.Fatalf("updates was replaced: %#v", response.Preferences)
	}
	if response.Preferences["comments"] != "muted" {
		t.Fatalf("comments patch missing: %#v", response.Preferences)
	}
	if response.Preferences["chat_replies"] != "muted" {
		t.Fatalf("chat_replies patch missing: %#v", response.Preferences)
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
	if preferences["status_changes"] != "all" || preferences["comments"] != "muted" || preferences["updates"] != "muted" {
		t.Fatalf("unexpected persisted preferences: %#v", preferences)
	}
}

// Status changes are muted by default (MUL-9449). Responses say so explicitly,
// because installed clients read a missing key as "all" and would show the
// switch on; storage stays sparse.
func TestNotificationPreferencesReportStatusChangesMutedByDefault(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	reset := func() {
		_, _ = testPool.Exec(ctx, `
			DELETE FROM notification_preference
			WHERE workspace_id = $1 AND user_id = $2
		`, testWorkspaceID, testUserID)
	}
	reset()
	t.Cleanup(reset)

	call := func(method string, prefs map[string]string) map[string]string {
		t.Helper()
		recorder := httptest.NewRecorder()
		switch method {
		case http.MethodGet:
			testHandler.GetNotificationPreferences(recorder, notificationPreferenceRequest(t, method, nil))
		case http.MethodPatch:
			testHandler.PatchNotificationPreferences(recorder, notificationPreferenceRequest(t, method, prefs))
		}
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", method, recorder.Code, recorder.Body.String())
		}
		var response struct {
			Preferences map[string]string `json:"preferences"`
		}
		if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
			t.Fatalf("%s: decode response: %v", method, err)
		}
		return response.Preferences
	}

	if got := call(http.MethodGet, nil); len(got) != 1 || got["status_changes"] != "muted" {
		t.Fatalf("GET without a row = %#v, want status_changes muted", got)
	}

	if got := call(http.MethodPatch, map[string]string{"comments": "muted"}); got["status_changes"] != "muted" || got["comments"] != "muted" {
		t.Fatalf("PATCH comments = %#v, want comments and the default status_changes muted", got)
	}
	var stored map[string]string
	var raw []byte
	if err := testPool.QueryRow(ctx, `
		SELECT preferences FROM notification_preference
		WHERE workspace_id = $1 AND user_id = $2
	`, testWorkspaceID, testUserID).Scan(&raw); err != nil {
		t.Fatalf("load persisted preference: %v", err)
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("decode persisted preference: %v", err)
	}
	if _, ok := stored["status_changes"]; ok {
		t.Fatalf("the default was persisted: %#v", stored)
	}

	call(http.MethodPatch, map[string]string{"status_changes": "all"})
	if got := call(http.MethodGet, nil); got["status_changes"] != "all" || got["comments"] != "muted" {
		t.Fatalf("GET after turning status changes on = %#v", got)
	}
}

// A client built before PATCH (MUL-4798) replaces the whole object and leaves
// a group out to mean "on". Its status changes stay on through persistence,
// read-back and delivery, while an explicit "muted" still turns them off
// (MUL-9449).
func TestLegacyPutKeepsOmittedStatusChangesOn(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	reset := func() {
		_, _ = testPool.Exec(ctx, `
			DELETE FROM notification_preference
			WHERE workspace_id = $1 AND user_id = $2
		`, testWorkspaceID, testUserID)
	}
	t.Cleanup(reset)

	for _, tc := range []struct {
		name      string
		sent      map[string]string
		stored    string
		delivered bool
	}{
		{"nothing muted", map[string]string{}, "all", true},
		{"another group muted", map[string]string{"comments": "muted"}, "all", true},
		{"status changes muted", map[string]string{"status_changes": "muted"}, "muted", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reset()

			recorder := httptest.NewRecorder()
			testHandler.UpdateNotificationPreferences(recorder, notificationPreferenceRequest(t, http.MethodPut, tc.sent))
			if recorder.Code != http.StatusOK {
				t.Fatalf("PUT: status=%d body=%s", recorder.Code, recorder.Body.String())
			}

			var raw []byte
			if err := testPool.QueryRow(ctx, `
				SELECT preferences FROM notification_preference
				WHERE workspace_id = $1 AND user_id = $2
			`, testWorkspaceID, testUserID).Scan(&raw); err != nil {
				t.Fatalf("load persisted preference: %v", err)
			}
			var stored map[string]string
			if err := json.Unmarshal(raw, &stored); err != nil {
				t.Fatalf("decode persisted preference: %v", err)
			}
			if stored["status_changes"] != tc.stored {
				t.Fatalf("persisted %#v, want status_changes %q", stored, tc.stored)
			}
			for group, value := range tc.sent {
				if stored[group] != value {
					t.Fatalf("persisted %#v, want the sent %s=%q kept", stored, group, value)
				}
			}

			get := httptest.NewRecorder()
			testHandler.GetNotificationPreferences(get, notificationPreferenceRequest(t, http.MethodGet, nil))
			var response struct {
				Preferences map[string]string `json:"preferences"`
			}
			if err := json.NewDecoder(get.Body).Decode(&response); err != nil {
				t.Fatalf("decode GET response: %v", err)
			}
			if response.Preferences["status_changes"] != tc.stored {
				t.Fatalf("GET = %#v, want status_changes %q", response.Preferences, tc.stored)
			}

			issueID := dbfx.Issue(t, "Legacy PUT status change", testutil.Cols{"workspace_id": testWorkspaceID})
			dbfx.Cleanup(t, `DELETE FROM inbox_item WHERE issue_id = $1`, issueID)
			_, inserted, err := inbox.Deliver(ctx, testHandler.Queries, nil, inbox.Notification{Params: db.CreateInboxItemParams{
				WorkspaceID:   parseUUID(testWorkspaceID),
				RecipientType: "member",
				RecipientID:   parseUUID(testUserID),
				Type:          "status_changed",
				Severity:      "info",
				IssueID:       parseUUID(issueID),
				Title:         "Legacy PUT status change",
				Details:       []byte(`{}`),
			}})
			if err != nil {
				t.Fatalf("deliver status_changed: %v", err)
			}
			if inserted != tc.delivered {
				t.Fatalf("status_changed delivered = %v, want %v", inserted, tc.delivered)
			}
		})
	}
}

// TestPatchNotificationPreferencesAcceptsAddedGroups covers groups added after
// the original set: mentions (#6468), reactions (MUL-9328),
// auto_archive_completed (MUL-9336) and the per-action auto_archive_on_*
// keys (MUL-9788).
// The whitelist rejects unknown keys, so a
// client shipped ahead of the server would 400 on these keys — the reason the
// server must deploy first.
func TestPatchNotificationPreferencesAcceptsAddedGroups(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	for _, groups := range [][]string{
		{"mentions"},
		{"reactions"},
		{"auto_archive_completed"},
		{"reactions", "auto_archive_completed"},
		{"auto_archive_on_comment", "auto_archive_on_status", "auto_archive_on_assignee"},
		{"auto_archive_on_priority", "auto_archive_on_project", "auto_archive_on_dates", "auto_archive_on_labels"},
	} {
		t.Run(strings.Join(groups, "+"), func(t *testing.T) {
			t.Cleanup(func() {
				_, _ = testPool.Exec(context.Background(), `
					DELETE FROM notification_preference
					WHERE workspace_id = $1 AND user_id = $2
				`, testWorkspaceID, testUserID)
			})

			patch := map[string]string{}
			for _, group := range groups {
				patch[group] = "muted"
			}
			recorder := httptest.NewRecorder()
			testHandler.PatchNotificationPreferences(
				recorder,
				notificationPreferenceRequest(t, http.MethodPatch, patch),
			)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}

			var response struct {
				Preferences map[string]string `json:"preferences"`
			}
			if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			for _, group := range groups {
				if response.Preferences[group] != "muted" {
					t.Fatalf("%s not persisted: %#v", group, response.Preferences)
				}
			}
		})
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
		code string
	}{
		{"scheme", "http://chat.googleapis.com/v1/spaces/AAAA/messages?key=x&token=y", "google_chat_invalid_url"},
		{"host", "https://example.com/v1/spaces/AAAA/messages?key=x&token=y", "google_chat_invalid_url"},
		{"port", "https://chat.googleapis.com:443/v1/spaces/AAAA/messages?key=x&token=y", "google_chat_invalid_url"},
		{"path", "https://chat.googleapis.com/v1/spaces/AAAA?key=x&token=y", "google_chat_invalid_url"},
		{"missing token", "https://chat.googleapis.com/v1/spaces/AAAA/messages?key=x", "google_chat_missing_credentials"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resetNotificationPreference(t)
			sender := &fakePreferenceChatSender{}
			usePreferenceChatSender(t, sender)

			var response struct {
				Code string `json:"code"`
			}
			testutil.Call(t, testHandler.PatchNotificationPreferences,
				notificationPreferenceRequest(t, http.MethodPatch, map[string]string{
					"google_chat_webhook": test.url,
				}),
			).Want(http.StatusBadRequest).JSON(&response)
			if response.Code != test.code {
				t.Fatalf("error code = %q, want %q", response.Code, test.code)
			}
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
	if !strings.Contains(response.Text(), `"code":"google_chat_connection_failed"`) {
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

func TestPatchNotificationPreferencesValidatesOthersIssuesValues(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `
			DELETE FROM notification_preference
			WHERE workspace_id = $1 AND user_id = $2
		`, testWorkspaceID, testUserID)
	})

	for value, want := range map[string]int{
		"all":         http.StatusOK,
		"key_updates": http.StatusOK,
		"muted":       http.StatusBadRequest,
	} {
		recorder := httptest.NewRecorder()
		testHandler.PatchNotificationPreferences(
			recorder,
			notificationPreferenceRequest(t, http.MethodPatch, map[string]string{
				"others_issues": value,
			}),
		)
		if recorder.Code != want {
			t.Errorf("others_issues=%q: status=%d want %d body=%s",
				value, recorder.Code, want, recorder.Body.String())
		}
	}

	// key_updates stays exclusive to others_issues.
	recorder := httptest.NewRecorder()
	testHandler.PatchNotificationPreferences(
		recorder,
		notificationPreferenceRequest(t, http.MethodPatch, map[string]string{
			"comments": "key_updates",
		}),
	)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("comments=key_updates: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestPatchNotificationPreferencesValidatesAfterAutoArchiveValues(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `
			DELETE FROM notification_preference
			WHERE workspace_id = $1 AND user_id = $2
		`, testWorkspaceID, testUserID)
	})

	for value, want := range map[string]int{
		"stay":  http.StatusOK,
		"next":  http.StatusOK,
		"all":   http.StatusBadRequest,
		"muted": http.StatusBadRequest,
	} {
		recorder := httptest.NewRecorder()
		testHandler.PatchNotificationPreferences(
			recorder,
			notificationPreferenceRequest(t, http.MethodPatch, map[string]string{
				"after_auto_archive": value,
			}),
		)
		if recorder.Code != want {
			t.Errorf("after_auto_archive=%q: status=%d want %d body=%s",
				value, recorder.Code, want, recorder.Body.String())
		}
	}
}

// Members asked once which actions archive their notifications (migration 718)
// answer for every workspace at once; the marker never reaches a client as a
// preference it could write back.
func TestAutoArchivePromptResolvesEveryPendingWorkspace(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	suffix := strings.ReplaceAll(t.Name(), "/", "-")
	otherWorkspaceID := dbfx.Workspace(t, "Auto-archive prompt", strings.ToLower(suffix))
	dbfx.Member(t, otherWorkspaceID, testUserID, "member")
	otherUserID := dbfx.User(t, "Auto-archive teammate", strings.ToLower(suffix)+"@multica.test")
	dbfx.Member(t, testWorkspaceID, otherUserID, "member")

	pending := `{"auto_archive_on_agent_trigger": "muted", "auto_archive_on_comment": "muted", "auto_archive_on_status": "muted",
		"auto_archive_on_assignee": "muted", "auto_archive_prompt": "pending", "updates": "muted"}`
	rows := [][2]string{{testWorkspaceID, testUserID}, {otherWorkspaceID, testUserID}, {testWorkspaceID, otherUserID}}
	for _, row := range rows {
		if _, err := testPool.Exec(ctx, `
			INSERT INTO notification_preference (workspace_id, user_id, preferences)
			VALUES ($1, $2, $3::jsonb)
			ON CONFLICT (workspace_id, user_id) DO UPDATE SET preferences = EXCLUDED.preferences
		`, row[0], row[1], pending); err != nil {
			t.Fatalf("seed preference: %v", err)
		}
	}
	t.Cleanup(func() {
		for _, row := range rows {
			_, _ = testPool.Exec(context.Background(), `
				DELETE FROM notification_preference WHERE workspace_id = $1 AND user_id = $2
			`, row[0], row[1])
		}
	})

	type response struct {
		Preferences       map[string]string `json:"preferences"`
		AutoArchivePrompt bool              `json:"auto_archive_prompt"`
	}
	decode := func(recorder *httptest.ResponseRecorder) response {
		t.Helper()
		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		var got response
		if err := json.NewDecoder(recorder.Body).Decode(&got); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return got
	}
	stored := func(workspaceID, userID string) map[string]string {
		t.Helper()
		var raw []byte
		if err := testPool.QueryRow(ctx, `
			SELECT preferences FROM notification_preference WHERE workspace_id = $1 AND user_id = $2
		`, workspaceID, userID).Scan(&raw); err != nil {
			t.Fatalf("load preference: %v", err)
		}
		var prefs map[string]string
		if err := json.Unmarshal(raw, &prefs); err != nil {
			t.Fatalf("decode preference: %v", err)
		}
		return prefs
	}

	get := httptest.NewRecorder()
	testHandler.GetNotificationPreferences(get, notificationPreferenceRequest(t, http.MethodGet, nil))
	before := decode(get)
	if !before.AutoArchivePrompt {
		t.Fatal("auto_archive_prompt = false, want true while pending")
	}
	if _, ok := before.Preferences[autoArchivePromptKey]; ok {
		t.Fatalf("preferences expose the prompt marker: %#v", before.Preferences)
	}

	for _, invalid := range []map[string]string{
		{"updates": "all"},
		{"after_auto_archive": "next"},
		{"auto_archive_prompt": "pending"},
	} {
		recorder := httptest.NewRecorder()
		testHandler.ResolveAutoArchivePrompt(recorder, notificationPreferenceRequest(t, http.MethodPost, invalid))
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("resolve with %v: status=%d want 400", invalid, recorder.Code)
		}
	}

	resolve := httptest.NewRecorder()
	testHandler.ResolveAutoArchivePrompt(resolve, notificationPreferenceRequest(t, http.MethodPost, map[string]string{
		"auto_archive_completed":        "all",
		"auto_archive_on_agent_trigger": "all",
		"auto_archive_on_comment":       "muted",
		"auto_archive_on_status":        "muted",
		"auto_archive_on_assignee":      "muted",
	}))
	after := decode(resolve)
	if after.AutoArchivePrompt {
		t.Fatal("auto_archive_prompt still true after resolving")
	}
	if after.Preferences["auto_archive_on_agent_trigger"] != "all" || after.Preferences["updates"] != "muted" {
		t.Fatalf("resolved preferences = %#v", after.Preferences)
	}

	for _, workspaceID := range []string{testWorkspaceID, otherWorkspaceID} {
		prefs := stored(workspaceID, testUserID)
		if _, ok := prefs[autoArchivePromptKey]; ok || prefs["auto_archive_on_agent_trigger"] != "all" {
			t.Errorf("workspace %s not resolved: %#v", workspaceID, prefs)
		}
	}
	if prefs := stored(testWorkspaceID, otherUserID); prefs[autoArchivePromptKey] != "pending" ||
		prefs["auto_archive_on_agent_trigger"] != "muted" {
		t.Errorf("another member's prompt changed: %#v", prefs)
	}
}
