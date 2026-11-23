package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
	publicapiv1 "github.com/multica-ai/multica/server/pkg/publicapi/v1"
)

// These are the Action API's security properties, not its happy path. Each one
// is a way a plugin could exceed what the person using it can do, and none of
// them is visible by reading the handler — they only hold if the three checks
// run in the right order against the right inputs.

// installPluginForAction installs the reference plugin and returns its id,
// leaving the caller free to vary scopes.
func installPluginForAction(t *testing.T, scopes []string) string {
	t.Helper()
	withPluginsV1Flag(t, testHandler, true)
	cleanupPluginInstallations(t)

	manifest := handlerTestManifest
	if scopes != nil {
		encoded, err := json.Marshal(scopes)
		if err != nil {
			t.Fatalf("encode scopes: %v", err)
		}
		manifest = strings.Replace(manifest,
			`"scopes": ["issues:read", "comments:write", "storage:user"]`,
			`"scopes": `+string(encoded), 1)
	}
	versionID := withLocalPluginSource(t, manifest)

	body, _ := json.Marshal(map[string]any{"version_id": versionID, "granted_scopes": scopes})
	recorder := httptest.NewRecorder()
	testHandler.InstallPlugin(recorder, pluginHandlerRequest(http.MethodPost, "/plugins", body, map[string]string{"id": testWorkspaceID}))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("install status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var installed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &installed); err != nil {
		t.Fatalf("decode installation: %v", err)
	}
	return installed.ID
}

func pluginActionRequest(method, path, installationID string, body any, params map[string]string) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	request := httptest.NewRequest(method, path, &buf)
	request.Header.Set("X-User-ID", testUserID)
	request.Header.Set("Content-Type", "application/json")
	if installationID != "" {
		request.Header.Set(pluginInstallationHeader, installationID)
	}
	routeContext := chi.NewRouteContext()
	for key, value := range params {
		routeContext.URLParams.Add(key, value)
	}
	return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
}

func pluginInstallTokenRequest(method, path, token string, body any, params map[string]string) *http.Request {
	request := pluginActionRequest(method, path, "", body, params)
	request.Header.Del("X-User-ID")
	request.Header.Set("Authorization", "Bearer "+token)
	return request
}

func TestPluginActionRequiresTheFeatureFlag(t *testing.T) {
	withPluginsV1Flag(t, testHandler, false)
	recorder := httptest.NewRecorder()

	testHandler.GetPluginContext(recorder, pluginInstallTokenRequest(http.MethodGet, "/v1/context", "mpi_invalid", nil, nil))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s, want 403", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After=%q, want empty for a non-retryable feature gate", got)
	}
	var problem publicapiv1.Problem
	if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode feature flag problem: %v", err)
	}
	if problem.Status != http.StatusForbidden || problem.Code != "plugin_api_disabled" || problem.Title != "Forbidden" {
		t.Fatalf("unexpected feature flag problem: %+v", problem)
	}
}

func TestPluginInstallTokenRunsIssueCommentWorkflow(t *testing.T) {
	installationID := installPluginForAction(t, []string{"issues:read", "issues:write", "comments:read", "comments:write"})
	token, err := testHandler.PluginService.IssueInstallToken(context.Background(), parseUUID(installationID))
	if err != nil {
		t.Fatalf("issue install token: %v", err)
	}
	issueID := createTestIssue(t, "Install token workflow", "todo", "none")

	get := httptest.NewRecorder()
	testHandler.GetPluginIssue(get, pluginInstallTokenRequest(http.MethodGet, "/v1/issues/"+issueID, token, nil,
		map[string]string{"issue_ref": issueID}))
	if get.Code != http.StatusOK {
		t.Fatalf("install-token issue read status=%d body=%s", get.Code, get.Body.String())
	}

	patchRequest := pluginInstallTokenRequest(http.MethodPatch, "/v1/issues/"+issueID, token,
		map[string]any{"title": "Updated by install token"}, map[string]string{"issue_ref": issueID})
	patchRequest.Header.Set("If-Match", get.Header().Get("ETag"))
	patch := httptest.NewRecorder()
	testHandler.PatchPluginIssue(patch, patchRequest)
	if patch.Code != http.StatusOK {
		t.Fatalf("install-token issue patch status=%d body=%s", patch.Code, patch.Body.String())
	}

	create := httptest.NewRecorder()
	testHandler.CreatePluginComment(create, pluginInstallTokenRequest(http.MethodPost, "/v1/issues/"+issueID+"/comments", token,
		map[string]any{"content": "created with a real mpi token"}, map[string]string{"issue_ref": issueID}))
	if create.Code != http.StatusCreated {
		t.Fatalf("install-token comment create status=%d body=%s", create.Code, create.Body.String())
	}
	var comment publicapiv1.Comment
	if err := json.Unmarshal(create.Body.Bytes(), &comment); err != nil {
		t.Fatalf("decode install-token comment: %v", err)
	}
	if comment.AuthorType != "plugin" || comment.AuthorID != installationID {
		t.Fatalf("install-token comment attribution = %+v", comment)
	}

	list := httptest.NewRecorder()
	testHandler.ListPluginComments(list, pluginInstallTokenRequest(http.MethodGet, "/v1/issues/"+issueID+"/comments", token, nil,
		map[string]string{"issue_ref": issueID}))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), comment.ID) {
		t.Fatalf("install-token comment list status=%d body=%s", list.Code, list.Body.String())
	}
}

func TestPluginInstallTokenEnforcesGrantedScope(t *testing.T) {
	installationID := installPluginForAction(t, []string{"issues:read"})
	token, err := testHandler.PluginService.IssueInstallToken(context.Background(), parseUUID(installationID))
	if err != nil {
		t.Fatalf("issue install token: %v", err)
	}
	issueID := createTestIssue(t, "Install token scope", "todo", "none")

	request := pluginInstallTokenRequest(http.MethodPatch, "/v1/issues/"+issueID, token,
		map[string]any{"title": "must not update"}, map[string]string{"issue_ref": issueID})
	response := httptest.NewRecorder()
	testHandler.PatchPluginIssue(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("ungranted install-token patch status=%d body=%s", response.Code, response.Body.String())
	}
	var problem publicapiv1.Problem
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode scope problem: %v", err)
	}
	if problem.Code != "forbidden" || !strings.Contains(problem.Detail, "issues:write") {
		t.Fatalf("unexpected scope problem: %+v", problem)
	}
}

func TestPluginActionRequiresAGrantedScope(t *testing.T) {
	// Installed with issues:read only. Everything else must be refused, and the
	// refusal must name the missing scope so an admin can act on it.
	installationID := installPluginForAction(t, []string{"issues:read"})
	issueID := createTestIssue(t, "Plugin action scope test", "todo", "none")

	recorder := httptest.NewRecorder()
	testHandler.GetPluginIssue(recorder, pluginActionRequest(http.MethodGet, "/issues", installationID, nil,
		map[string]string{"issue_ref": issueID}))
	if recorder.Code != http.StatusOK {
		t.Fatalf("granted scope was refused: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	testHandler.CreatePluginComment(recorder, pluginActionRequest(http.MethodPost, "/comments", installationID,
		map[string]any{"content": "hello"}, map[string]string{"issue_ref": issueID}))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("ungranted scope status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "comments:write") {
		t.Fatalf("refusal does not name the missing scope: %s", recorder.Body.String())
	}
	var problem publicapiv1.Problem
	if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode stable problem: %v", err)
	}
	if problem.Code != "forbidden" || problem.Error == "" || problem.RequestID == "" {
		t.Fatalf("unexpected stable problem: %+v", problem)
	}
}

func TestPluginIssueUsesStableDTOAndRevisionETag(t *testing.T) {
	installationID := installPluginForAction(t, []string{"issues:read", "issues:write"})
	issueID := createTestIssue(t, "Plugin public DTO test", "todo", "none")

	get := httptest.NewRecorder()
	testHandler.GetPluginIssue(get, pluginActionRequest(http.MethodGet, "/v1/issues/"+issueID, installationID, nil,
		map[string]string{"issue_ref": issueID}))
	if get.Code != http.StatusOK {
		t.Fatalf("get issue status=%d body=%s", get.Code, get.Body.String())
	}
	var issue publicapiv1.Issue
	if err := json.Unmarshal(get.Body.Bytes(), &issue); err != nil {
		t.Fatalf("decode public issue: %v", err)
	}
	if issue.ID != issueID || issue.Identifier == "" || issue.Revision < 1 || issue.Metadata == nil || issue.Properties == nil {
		t.Fatalf("unexpected public issue: %+v", issue)
	}
	wantETag := fmt.Sprintf(`W/"%d"`, issue.Revision)
	if got := get.Header().Get("ETag"); got != wantETag {
		t.Fatalf("ETag = %q, want %q", got, wantETag)
	}
	var raw map[string]any
	if err := json.Unmarshal(get.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode issue map: %v", err)
	}
	for _, appOnly := range []string{"labels", "attachments", "reactions"} {
		if _, leaked := raw[appOnly]; leaked {
			t.Fatalf("App-only field %q leaked into Public API: %s", appOnly, get.Body.String())
		}
	}

	patchRequest := pluginActionRequest(http.MethodPatch, "/v1/issues/"+issueID, installationID,
		map[string]any{"title": "Updated through Public API"}, map[string]string{"issue_ref": issueID})
	patchRequest.Header.Set("If-Match", wantETag)
	patch := httptest.NewRecorder()
	testHandler.PatchPluginIssue(patch, patchRequest)
	if patch.Code != http.StatusOK {
		t.Fatalf("patch issue status=%d body=%s", patch.Code, patch.Body.String())
	}
	var updated publicapiv1.Issue
	if err := json.Unmarshal(patch.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode updated issue: %v", err)
	}
	if updated.Title != "Updated through Public API" || updated.Revision <= issue.Revision {
		t.Fatalf("unexpected updated issue: %+v", updated)
	}
	if got := patch.Header().Get("ETag"); got != fmt.Sprintf(`W/"%d"`, updated.Revision) {
		t.Fatalf("updated ETag = %q", got)
	}

	staleRequest := pluginActionRequest(http.MethodPatch, "/v1/issues/"+issueID, installationID,
		map[string]any{"description": "stale"}, map[string]string{"issue_ref": issueID})
	staleRequest.Header.Set("If-Match", wantETag)
	stale := httptest.NewRecorder()
	testHandler.PatchPluginIssue(stale, staleRequest)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale patch status=%d body=%s", stale.Code, stale.Body.String())
	}
	var conflict publicapiv1.Problem
	if err := json.Unmarshal(stale.Body.Bytes(), &conflict); err != nil {
		t.Fatalf("decode revision conflict: %v", err)
	}
	if conflict.Code != "revision_conflict" {
		t.Fatalf("conflict code = %q", conflict.Code)
	}
}

func TestPluginIssueConditionalPatchHasSingleWinner(t *testing.T) {
	installationID := installPluginForAction(t, []string{"issues:read", "issues:write"})
	issueID := createTestIssue(t, "Plugin conditional write test", "todo", "none")

	get := httptest.NewRecorder()
	testHandler.GetPluginIssue(get, pluginActionRequest(http.MethodGet, "/v1/issues/"+issueID, installationID, nil,
		map[string]string{"issue_ref": issueID}))
	if get.Code != http.StatusOK {
		t.Fatalf("get issue status=%d body=%s", get.Code, get.Body.String())
	}
	etag := get.Header().Get("ETag")

	start := make(chan struct{})
	results := make(chan int, 2)
	var wg sync.WaitGroup
	for _, title := range []string{"concurrent winner A", "concurrent winner B"} {
		wg.Add(1)
		go func(title string) {
			defer wg.Done()
			request := pluginActionRequest(http.MethodPatch, "/v1/issues/"+issueID, installationID,
				map[string]any{"title": title}, map[string]string{"issue_ref": issueID})
			request.Header.Set("If-Match", etag)
			<-start
			response := httptest.NewRecorder()
			testHandler.PatchPluginIssue(response, request)
			results <- response.Code
		}(title)
	}
	close(start)
	wg.Wait()
	close(results)

	counts := map[int]int{}
	for status := range results {
		counts[status]++
	}
	if counts[http.StatusOK] != 1 || counts[http.StatusConflict] != 1 {
		t.Fatalf("conditional write statuses = %v, want one 200 and one 409", counts)
	}
}

func TestPluginActionRefusedWithoutAnInstallation(t *testing.T) {
	installPluginForAction(t, []string{"issues:read"})
	issueID := createTestIssue(t, "Plugin action installation test", "todo", "none")

	// No header at all: the endpoint must not fall back to "any installed
	// plugin" or to an unscoped call.
	recorder := httptest.NewRecorder()
	testHandler.GetPluginIssue(recorder, pluginActionRequest(http.MethodGet, "/issues", "", nil,
		map[string]string{"issue_ref": issueID}))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("missing installation header status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	// An installation id that does not exist is a 404, never a pass-through.
	recorder = httptest.NewRecorder()
	testHandler.GetPluginIssue(recorder, pluginActionRequest(http.MethodGet, "/issues",
		"11111111-1111-1111-1111-111111111111", nil, map[string]string{"issue_ref": issueID}))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown installation status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestPluginActionStopsWhenTheInstallationIsDisabled(t *testing.T) {
	installationID := installPluginForAction(t, []string{"issues:read"})
	issueID := createTestIssue(t, "Plugin action disable test", "todo", "none")

	recorder := httptest.NewRecorder()
	testHandler.DisablePlugin(recorder, pluginHandlerRequest(http.MethodPost, "/plugins/disable", nil,
		map[string]string{"id": testWorkspaceID, "installationId": installationID}))
	if recorder.Code != http.StatusOK {
		t.Fatalf("disable status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	// A surface left open in a stale tab keeps its bridge; disabling has to cut
	// it off server-side or "disabled" only means "hidden".
	recorder = httptest.NewRecorder()
	testHandler.GetPluginIssue(recorder, pluginActionRequest(http.MethodGet, "/issues", installationID, nil,
		map[string]string{"issue_ref": issueID}))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("disabled installation status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestPluginCommentIsAuthoredByTheUserAndMarkedWithThePlugin(t *testing.T) {
	installationID := installPluginForAction(t, []string{"issues:read", "comments:write"})
	issueID := createTestIssue(t, "Plugin action attribution test", "todo", "none")

	recorder := httptest.NewRecorder()
	testHandler.CreatePluginComment(recorder, pluginActionRequest(http.MethodPost, "/comments", installationID,
		map[string]any{"content": "posted through a plugin"}, map[string]string{"issue_ref": issueID}))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create comment status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var created struct {
		ID         string `json:"id"`
		AuthorType string `json:"author_type"`
		AuthorID   string `json:"author_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode comment: %v", err)
	}
	// Permission-wise the write is the user's...
	if created.AuthorType != "member" || created.AuthorID != testUserID {
		t.Fatalf("comment was not authored by the calling user: %+v", created)
	}
	// ...and audit-wise it stays attributable to the plugin that produced it.
	var viaPlugin *string
	if err := testPool.QueryRow(context.Background(),
		`SELECT via_plugin_id::text FROM comment WHERE id = $1`, created.ID).Scan(&viaPlugin); err != nil {
		t.Fatalf("read via_plugin_id: %v", err)
	}
	if viaPlugin == nil || *viaPlugin != installationID {
		t.Fatalf("via_plugin_id = %v, want %s", viaPlugin, installationID)
	}
}

func TestPluginCommentTriggerAgentsWakesTheAssignee(t *testing.T) {
	installationID := installPluginForAction(t, []string{"issues:read", "comments:write", "tasks:write"})
	agentID := createHandlerTestAgent(t, "Plugin Review Assignee", nil)
	issueID := createCommentTriggerPreviewIssue(t, "plugin review wakes the assignee", "agent", agentID)

	post := func(body map[string]any) {
		t.Helper()
		recorder := httptest.NewRecorder()
		testHandler.CreatePluginComment(recorder, pluginActionRequest(http.MethodPost, "/comments", installationID,
			body, map[string]string{"issue_ref": issueID}))
		if recorder.Code != http.StatusCreated {
			t.Fatalf("create comment status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}

	post(map[string]any{"content": "plain plugin comment"})
	if got := countQueuedCommentTriggerTasks(t, issueID, agentID); got != 0 {
		t.Fatalf("comment without trigger_agents queued %d tasks, want 0", got)
	}
	post(map[string]any{"content": "preview review is ready", "trigger_agents": true})
	if got := countQueuedCommentTriggerTasks(t, issueID, agentID); got != 1 {
		t.Fatalf("trigger_agents queued %d tasks, want 1", got)
	}
}

func TestPluginCommentTriggerAgentsNeedsTasksWrite(t *testing.T) {
	agentID := createHandlerTestAgent(t, "Plugin Refused Assignee", nil)
	issueID := createCommentTriggerPreviewIssue(t, "plugin trigger refusal", "agent", agentID)

	installationID := installPluginForAction(t, []string{"issues:read", "comments:write"})
	recorder := httptest.NewRecorder()
	testHandler.CreatePluginComment(recorder, pluginActionRequest(http.MethodPost, "/comments", installationID,
		map[string]any{"content": "wake up", "trigger_agents": true}, map[string]string{"issue_ref": issueID}))
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "tasks:write") {
		t.Fatalf("trigger without tasks:write status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var comments int
	dbfx.QueryRow(t, `SELECT count(*) FROM comment WHERE issue_id = $1`, issueID).Scan(&comments)
	if comments != 0 || countQueuedCommentTriggerTasks(t, issueID, agentID) != 0 {
		t.Fatalf("refused trigger left comments=%d queued=%d", comments, countQueuedCommentTriggerTasks(t, issueID, agentID))
	}
}

// A Plugin acting as itself keeps its own name on the comment and lends only
// the installer's authority to the run it starts.
func TestPluginOwnTriggerRunsForTheInstaller(t *testing.T) {
	agentID := createHandlerTestAgent(t, "Plugin Installer Assignee", nil)
	issueID := createCommentTriggerPreviewIssue(t, "plugin trigger as installer", "agent", agentID)
	installationID := installPluginForAction(t, []string{"issues:read", "comments:write", "tasks:write"})
	token, err := testHandler.PluginService.IssueInstallToken(context.Background(), parseUUID(installationID))
	if err != nil {
		t.Fatalf("issue install token: %v", err)
	}
	post := func(content string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		testHandler.CreatePluginComment(recorder, pluginInstallTokenRequest(http.MethodPost, "/v1/issues/"+issueID+"/comments", token,
			map[string]any{"content": content, "trigger_agents": true}, map[string]string{"issue_ref": issueID}))
		return recorder
	}

	recorder := post("preview review is ready")
	if recorder.Code != http.StatusCreated {
		t.Fatalf("plugin trigger status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var comment struct {
		ID         string `json:"id"`
		AuthorType string `json:"author_type"`
		AuthorID   string `json:"author_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &comment); err != nil {
		t.Fatalf("decode comment: %v", err)
	}
	if comment.AuthorType != "plugin" || comment.AuthorID != installationID {
		t.Fatalf("plugin trigger comment author = %s %s, want the plugin %s", comment.AuthorType, comment.AuthorID, installationID)
	}
	var originator, accountable, source, triggerComment string
	dbfx.QueryRow(t, `SELECT originator_user_id::text, accountable_user_id::text, originator_source, trigger_comment_id::text
		FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'`,
		issueID, agentID).Scan(&originator, &accountable, &source, &triggerComment)
	if originator != testUserID || accountable != testUserID || source != "plugin_installer" || triggerComment != comment.ID {
		t.Fatalf("plugin-triggered task originator=%q accountable=%q source=%q trigger=%q, want installer %s via plugin_installer from %s",
			originator, accountable, source, triggerComment, testUserID, comment.ID)
	}

	dbfx.Exec(t, `UPDATE plugin_installation SET installed_by = gen_random_uuid() WHERE id = $1`, installationID)
	recorder = post("installer has left")
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "installer_membership_revoked") {
		t.Fatalf("trigger after installer left status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestPluginFindsLinkedPullRequestsByBranch(t *testing.T) {
	installationID := installPluginForAction(t, []string{"issues:read"})
	first := createTestIssue(t, "Branch lookup first", "todo", "none")
	second := createTestIssue(t, "Branch lookup second", "todo", "none")
	other := createTestIssue(t, "Branch lookup other branch", "todo", "none")
	linkPR := func(number int, branch string, issueIDs ...string) {
		prID := dbfx.Insert(t, "github_pull_request", testutil.Cols{
			"workspace_id":    testWorkspaceID,
			"installation_id": 1,
			"repo_owner":      "Acme",
			"repo_name":       "branch-lookup",
			"pr_number":       number,
			"title":           fmt.Sprintf("PR %d", number),
			"state":           "open",
			"html_url":        fmt.Sprintf("https://github.com/Acme/branch-lookup/pull/%d", number),
			"branch":          branch,
			"head_sha":        "abc",
			"pr_created_at":   testutil.Raw("now()"),
			"pr_updated_at":   testutil.Raw("now()"),
		})
		for _, issueID := range issueIDs {
			dbfx.InsertNoID(t, "issue_pull_request", testutil.Cols{"issue_id": issueID, "pull_request_id": prID},
				"pull_request_id = $1", prID)
		}
	}
	linkPR(7, "feat/preview", first, second)
	linkPR(8, "feat/other", other)

	lookup := func(query string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		testHandler.ListPluginPullRequestsByBranch(recorder, pluginActionRequest(http.MethodGet, "/v1/pull-requests?"+query, installationID, nil, nil))
		return recorder
	}

	recorder := lookup("repo_owner=acme&repo_name=BRANCH-LOOKUP&branch=feat/preview")
	if recorder.Code != http.StatusOK {
		t.Fatalf("lookup status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var found publicapiv1.LinkedPullRequestListResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &found); err != nil {
		t.Fatalf("decode lookup: %v", err)
	}
	if len(found.PullRequests) != 1 || found.PullRequests[0].Number != 7 {
		t.Fatalf("lookup = %+v, want only #7", found.PullRequests)
	}
	got := strings.Join(found.PullRequests[0].IssueIDs, ",")
	if !strings.Contains(got, first) || !strings.Contains(got, second) || len(found.PullRequests[0].IssueIDs) != 2 {
		t.Fatalf("issue ids = %v, want %s and %s", found.PullRequests[0].IssueIDs, first, second)
	}

	if recorder := lookup("repo_owner=acme&repo_name=branch-lookup"); recorder.Code != http.StatusBadRequest {
		t.Fatalf("lookup without branch status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestPluginListsLinkedPullRequestsNewestFirst(t *testing.T) {
	installationID := installPluginForAction(t, []string{"issues:read"})
	issueID := createTestIssue(t, "Plugin pull request list", "todo", "none")
	linkPR := func(number int, state, branch, createdAt string) {
		prID := dbfx.Insert(t, "github_pull_request", testutil.Cols{
			"workspace_id":    testWorkspaceID,
			"installation_id": 1,
			"repo_owner":      "acme",
			"repo_name":       "plugin-pr-list",
			"pr_number":       number,
			"title":           fmt.Sprintf("PR %d", number),
			"state":           state,
			"html_url":        fmt.Sprintf("https://github.com/acme/plugin-pr-list/pull/%d", number),
			"branch":          branch,
			"head_sha":        "abc",
			"pr_created_at":   testutil.Raw(createdAt),
			"pr_updated_at":   testutil.Raw("now()"),
		})
		dbfx.InsertNoID(t, "issue_pull_request", testutil.Cols{"issue_id": issueID, "pull_request_id": prID},
			"pull_request_id = $1", prID)
	}
	linkPR(41, "merged", "feat/older", "now() - interval '1 day'")
	linkPR(42, "open", "feat/preview", "now()")

	recorder := httptest.NewRecorder()
	testHandler.ListPluginPullRequests(recorder, pluginActionRequest(http.MethodGet, "/pull-requests", installationID, nil,
		map[string]string{"issue_ref": issueID}))
	if recorder.Code != http.StatusOK {
		t.Fatalf("list pull requests status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var listed publicapiv1.PullRequestListResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode pull requests: %v", err)
	}
	if len(listed.PullRequests) != 2 {
		t.Fatalf("pull requests = %+v, want 2", listed.PullRequests)
	}
	newest := listed.PullRequests[0]
	if newest.Number != 42 || newest.State != "open" || newest.Provider != "github" ||
		newest.Branch == nil || *newest.Branch != "feat/preview" ||
		newest.HTMLURL != "https://github.com/acme/plugin-pr-list/pull/42" {
		t.Fatalf("newest pull request = %+v", newest)
	}
	if listed.PullRequests[1].Number != 41 {
		t.Fatalf("second pull request = %+v, want #41", listed.PullRequests[1])
	}
}

func TestPluginActionCannotReachAnotherWorkspacesIssue(t *testing.T) {
	installationID := installPluginForAction(t, []string{"issues:read"})

	// An issue id from a workspace this user is not a member of must 404 — the
	// plugin inherits the user's reach and nothing more, so this is the same
	// answer the ordinary issue endpoint gives.
	otherWorkspaceIssue := createIssueInForeignWorkspace(t)
	recorder := httptest.NewRecorder()
	testHandler.GetPluginIssue(recorder, pluginActionRequest(http.MethodGet, "/issues", installationID, nil,
		map[string]string{"issue_ref": otherWorkspaceIssue}))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("cross-workspace read status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// createIssueInForeignWorkspace seeds a workspace the test user is NOT a member
// of, so a cross-tenant read has something real to fail against rather than a
// made-up uuid that would 404 for the wrong reason.
func createIssueInForeignWorkspace(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	var ownerID string
	if err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('Foreign Owner', $1) RETURNING id`,
		"foreign-plugin-"+testUserID+"@multica.test").Scan(&ownerID); err != nil {
		t.Fatalf("seed foreign user: %v", err)
	}
	var workspaceID string
	if err := testPool.QueryRow(ctx, `INSERT INTO workspace (name, slug) VALUES ('Foreign', $1) RETURNING id`,
		"foreign-plugin-"+testUserID).Scan(&workspaceID); err != nil {
		t.Fatalf("seed foreign workspace: %v", err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`,
		workspaceID, ownerID); err != nil {
		t.Fatalf("seed foreign member: %v", err)
	}
	var issueID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO issue (workspace_id, title, status, priority, creator_type, creator_id)
		 VALUES ($1, 'Foreign issue', 'todo', 'none', 'member', $2) RETURNING id`,
		workspaceID, ownerID).Scan(&issueID); err != nil {
		t.Fatalf("seed foreign issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE workspace_id = $1`, workspaceID)
		testPool.Exec(context.Background(), `DELETE FROM member WHERE workspace_id = $1`, workspaceID)
		testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, workspaceID)
		testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, ownerID)
	})
	return issueID
}
