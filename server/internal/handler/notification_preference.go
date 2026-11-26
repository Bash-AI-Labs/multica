package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/inbox"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// validNotifGroups is the set of notification preference group keys that the
// API accepts. Keys not in this set are rejected. `system_notifications` is
// not an inbox event group — it's a delivery-channel toggle controlling
// whether native OS notification banners fire — but it shares the same
// preferences map so a single endpoint covers all user notification
// preferences. The `auto_archive_*` keys are not event groups either: each
// says whether one action you take on an issue archives your notifications on
// it. Completing and triggering an agent archive unless "muted"; commenting,
// status changes, reassigning, priority, project, date and label edits archive
// only with "all". `squad_handoffs` narrows `comments` to leave out the
// comments a squad's agents hand work to each other with (MUL-10008).
// `after_auto_archive` is a legacy preference kept accepted for older
// clients; current clients always stay on an auto-archived issue.
var validNotifGroups = map[string]bool{
	"assignments":                   true,
	"status_changes":                true,
	"comments":                      true,
	"squad_handoffs":                true,
	"chat_replies":                  true,
	"mentions":                      true,
	"reactions":                     true,
	"updates":                       true,
	"agent_activity":                true,
	"new_issues":                    true,
	"system_notifications":          true,
	"email_notifications":           true,
	"auto_archive_completed":        true,
	"auto_archive_on_agent_trigger": true,
	"auto_archive_on_comment":       true,
	"auto_archive_on_status":        true,
	"auto_archive_on_assignee":      true,
	"auto_archive_on_priority":      true,
	"auto_archive_on_project":       true,
	"auto_archive_on_dates":         true,
	"auto_archive_on_labels":        true,
	"after_auto_archive":            true,
	"others_issues":                 true,
}

// validNotifValues is the set of allowed preference values for a group not
// listed in notifValuesByGroup.
var validNotifValues = map[string]bool{
	"all":   true,
	"muted": true,
}

// notifValuesByGroup lists the groups whose values are not on/off.
// `others_issues` is how much of someone else's issue a passive subscriber
// receives: absent or "key_updates" for mentions, thread replies and handoffs,
// "all" for every event. Muting it would only hide what still needs you.
// `after_auto_archive` is absent or "stay" to keep the archived notification
// open, "next" to open the next one.
var notifValuesByGroup = map[string]map[string]bool{
	"others_issues":      {"all": true, "key_updates": true},
	"after_auto_archive": {"stay": true, "next": true},
}

// autoArchivePromptKey marks a workspace where the member has not yet answered
// the one-time question of which actions archive their notifications. Migration
// 718 writes it for members who archived notifications before replying,
// changing status and reassigning did so by default. It is stored with the
// preferences but answered as the response's own `auto_archive_prompt` flag,
// so no client ever echoes it back in a write.
const autoArchivePromptKey = "auto_archive_prompt"

func validNotifValue(group, value string) bool {
	if values, ok := notifValuesByGroup[group]; ok {
		return values[value]
	}
	return validNotifValues[value]
}

func (h *Handler) GetNotificationPreferences(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())

	prefs, err := h.loadNotificationPreferences(r, workspaceID, userID)
	if err != nil {
		slog.Warn("GetNotificationPreference failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to get notification preferences")
		return
	}
	writeNotificationPreferences(w, workspaceID, prefs)
}

type updateNotifPrefRequest struct {
	Preferences map[string]string `json:"preferences"`
}

// decodeNotificationPreferenceRequest reads and validates the preferences in
// a write request.
func decodeNotificationPreferenceRequest(w http.ResponseWriter, r *http.Request) (map[string]string, bool) {
	var req updateNotifPrefRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return nil, false
	}

	if req.Preferences == nil {
		writeError(w, http.StatusBadRequest, "preferences field is required")
		return nil, false
	}

	for k, v := range req.Preferences {
		if k == "google_chat_webhook" {
			continue
		}
		if !validNotifGroups[k] {
			writeError(w, http.StatusBadRequest, "invalid preference group: "+k)
			return nil, false
		}
		if !validNotifValue(k, v) {
			writeError(w, http.StatusBadRequest, "invalid preference value: "+v)
			return nil, false
		}
	}

	return req.Preferences, true
}

func marshalNotificationPreferences(w http.ResponseWriter, prefs map[string]string) ([]byte, bool) {
	prefsJSON, err := json.Marshal(prefs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to marshal preferences")
		return nil, false
	}
	return prefsJSON, true
}

// writeNotificationPreferenceResponse answers a write with the row it stored.
func writeNotificationPreferenceResponse(
	w http.ResponseWriter,
	workspaceID string,
	pref db.NotificationPreference,
) {
	var prefs map[string]string
	if err := json.Unmarshal(pref.Preferences, &prefs); err != nil {
		prefs = map[string]string{}
	}
	writeNotificationPreferences(w, workspaceID, prefs)
}

// writeNotificationPreferences answers with the stored preferences plus the
// groups muted by default (inbox.WithDefaults), so every client renders those
// switches as off whichever defaults it was built with. The Google Chat
// webhook is masked, and the stored auto-archive prompt marker is answered as
// its own flag.
func writeNotificationPreferences(w http.ResponseWriter, workspaceID string, prefs map[string]string) {
	_, prompt := prefs[autoArchivePromptKey]
	delete(prefs, autoArchivePromptKey)

	writeJSON(w, http.StatusOK, map[string]any{
		"workspace_id":        workspaceID,
		"preferences":         inbox.WithDefaults(maskedNotificationPreferences(prefs)),
		"auto_archive_prompt": prompt,
	})
}

func (h *Handler) loadNotificationPreferences(
	r *http.Request,
	workspaceID string,
	userID string,
) (map[string]string, error) {
	pref, err := h.Queries.GetNotificationPreference(r.Context(), db.GetNotificationPreferenceParams{
		WorkspaceID: parseUUID(workspaceID),
		UserID:      parseUUID(userID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var prefs map[string]string
	if err := json.Unmarshal(pref.Preferences, &prefs); err != nil {
		return map[string]string{}, nil
	}
	return prefs, nil
}

func maskedNotificationPreferences(prefs map[string]string) map[string]string {
	masked := make(map[string]string, len(prefs))
	for key, value := range prefs {
		if key == "google_chat_webhook" && value != "" {
			value = service.MaskGoogleChatWebhookURL(value)
		}
		masked[key] = value
	}
	return masked
}

func (h *Handler) prepareNotificationPreferenceUpdate(
	w http.ResponseWriter,
	r *http.Request,
	workspaceID string,
	userID string,
) (map[string]string, bool) {
	current, err := h.loadNotificationPreferences(r, workspaceID, userID)
	if err != nil {
		slog.Warn("GetNotificationPreference before update failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to load notification preferences")
		return nil, false
	}

	next, ok := decodeNotificationPreferenceRequest(w, r)
	if !ok {
		return nil, false
	}

	incomingWebhook, webhookIncluded := next["google_chat_webhook"]
	currentWebhook := current["google_chat_webhook"]
	if !webhookIncluded && currentWebhook != "" {
		// Clients that predate Google Chat delivery replace-all PUT without
		// this key; keep the credential instead of silently disconnecting.
		next["google_chat_webhook"] = currentWebhook
	}
	if webhookIncluded && incomingWebhook != "" {
		currentMasked := service.MaskGoogleChatWebhookURL(currentWebhook)
		if currentMasked != "" && incomingWebhook == currentMasked {
			next["google_chat_webhook"] = currentWebhook
			incomingWebhook = currentWebhook
		} else if _, err := service.ValidateGoogleChatWebhookURL(incomingWebhook); err != nil {
			code := "google_chat_invalid_url"
			if errors.Is(err, service.ErrGoogleChatMissingCredentials) {
				code = "google_chat_missing_credentials"
			}
			writeErrorCode(w, http.StatusBadRequest, code, err.Error())
			return nil, false
		}
	}

	if webhookIncluded && incomingWebhook != "" && incomingWebhook != currentWebhook {
		workspace, err := h.Queries.GetWorkspace(r.Context(), parseUUID(workspaceID))
		if err != nil {
			slog.Warn("GetWorkspace for Google Chat confirmation failed", append(logger.RequestAttrs(r), "error", err)...)
			writeError(w, http.StatusInternalServerError, "failed to load workspace")
			return nil, false
		}
		if h.NotificationChatSender == nil {
			writeErrorCode(w, http.StatusBadRequest, "google_chat_connection_failed", "Google Chat delivery is unavailable")
			return nil, false
		}
		if err := h.NotificationChatSender.Send(
			r.Context(),
			incomingWebhook,
			service.BuildGoogleChatConfirmation(workspace.Name),
		); err != nil {
			writeErrorCode(w, http.StatusBadRequest, "google_chat_connection_failed", "failed to connect Google Chat webhook; verify the URL and try again")
			return nil, false
		}
	}

	return next, true
}

// UpdateNotificationPreferences preserves the original replace-all PUT
// contract for compatibility with installed clients.
func (h *Handler) UpdateNotificationPreferences(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())
	prefs, ok := h.prepareNotificationPreferenceUpdate(w, r, workspaceID, userID)
	if !ok {
		return
	}
	// Only clients built before PATCH existed send this full replacement, and
	// they leave a group out to mean "on".
	prefsJSON, ok := marshalNotificationPreferences(w, inbox.WithLegacyDefaults(prefs))
	if !ok {
		return
	}

	pref, err := h.Queries.UpsertNotificationPreference(r.Context(), db.UpsertNotificationPreferenceParams{
		WorkspaceID: parseUUID(workspaceID),
		UserID:      parseUUID(userID),
		Preferences: prefsJSON,
	})
	if err != nil {
		slog.Warn("UpsertNotificationPreference failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to update notification preferences")
		return
	}
	writeNotificationPreferenceResponse(w, workspaceID, pref)
}

// PatchNotificationPreferences atomically merges only the supplied keys. This
// prevents stale tabs or devices from replacing unrelated mute settings.
func (h *Handler) PatchNotificationPreferences(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())
	prefs, ok := h.prepareNotificationPreferenceUpdate(w, r, workspaceID, userID)
	if !ok {
		return
	}
	prefsJSON, ok := marshalNotificationPreferences(w, prefs)
	if !ok {
		return
	}

	pref, err := h.Queries.PatchNotificationPreference(r.Context(), db.PatchNotificationPreferenceParams{
		WorkspaceID: parseUUID(workspaceID),
		UserID:      parseUUID(userID),
		Preferences: prefsJSON,
	})
	if err != nil {
		slog.Warn("PatchNotificationPreference failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to update notification preferences")
		return
	}
	writeNotificationPreferenceResponse(w, workspaceID, pref)
}

// ResolveAutoArchivePrompt answers the one-time auto-archive question once for
// all of the caller's workspaces: the supplied `auto_archive_*` values are
// merged into every one of their preference rows still marked with
// auto_archive_prompt, and the marker is dropped. An empty map keeps what each
// workspace has. Answers with the current workspace's preferences.
func (h *Handler) ResolveAutoArchivePrompt(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	prefs, ok := decodeNotificationPreferenceRequest(w, r)
	if !ok {
		return
	}
	for k := range prefs {
		if !strings.HasPrefix(k, "auto_archive_") {
			writeError(w, http.StatusBadRequest, "invalid preference group: "+k)
			return
		}
	}
	prefsJSON, ok := marshalNotificationPreferences(w, prefs)
	if !ok {
		return
	}

	if err := h.Queries.ResolveAutoArchivePrompt(r.Context(), db.ResolveAutoArchivePromptParams{
		UserID:      parseUUID(userID),
		Preferences: prefsJSON,
	}); err != nil {
		slog.Warn("ResolveAutoArchivePrompt failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to update notification preferences")
		return
	}
	h.GetNotificationPreferences(w, r)
}

func (h *Handler) archiveActorInboxOnAgentTrigger(ctx context.Context, workspaceID, issueID, actorType, actorID string) {
	inbox.ArchiveActorIssueInbox(ctx, h.Queries, h.Bus, workspaceID, issueID, actorType, actorID, inbox.AutoArchiveAgentTriggerPref)
}

// issueRunTriggeredAgent reports whether an issue write actually handed work to
// an agent (queued a fresh run or coalesced into a pending one). Deferred
// (backlog parking, suppress_run, self-loop) and blocked outcomes do not count.
func issueRunTriggeredAgent(outcome *IssueRunOutcome) bool {
	return outcome != nil && (outcome.Status == DispatchQueued || outcome.Status == DispatchCoalesced)
}

// commentResultsTriggeredAgent reports whether a comment actually reached at
// least one agent (queued, coalesced into a pending run, deferred to an active
// run's completion reconcile, or steered into a running turn). Blocked targets
// do not count.
func commentResultsTriggeredAgent(results map[string]commentEnqueueResult) bool {
	for _, res := range results {
		switch res.status {
		case DispatchQueued, DispatchCoalesced, DispatchDeferred, DispatchSteered:
			return true
		}
	}
	return false
}
