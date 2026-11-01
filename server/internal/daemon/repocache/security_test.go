package repocache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateRepositoryURL(t *testing.T) {
	t.Parallel()

	valid := []string{
		"https://github.com/multica-ai/multica.git",
		"http://git.internal.example/team/repo",
		"ssh://git@gitlab.example.com:2222/group/repo.git",
		"git://git.example.com/group/repo.git",
		"git@github.com:multica-ai/multica.git",
		"git@[2001:db8::1]:group/repo.git",
	}
	for _, repoURL := range valid {
		if err := validateRepositoryURL(repoURL); err != nil {
			t.Errorf("validateRepositoryURL(%q) = %v, want nil", repoURL, err)
		}
	}

	invalid := []string{
		"",
		" https://github.com/org/repo.git",
		"--upload-pack=touch owned",
		"-c core.sshCommand=touch-owned",
		"ext::sh -c 'touch owned'",
		"file:///etc",
		"/Users/owner/private-repo",
		"../private-repo",
		"https://github.com/org/../private.git",
		"https://github.com/org/repo.git?upload-pack=evil",
		"https://github.com/org/repo.git#fragment",
		`https://github.com/org\repo.git`,
		"https://github.com/...git",
		"git@-host:org/repo.git",
	}
	for _, repoURL := range invalid {
		if err := validateRepositoryURL(repoURL); err == nil {
			t.Errorf("validateRepositoryURL(%q) = nil, want error", repoURL)
		}
	}
}

func TestTrustedLocalRepositoryIsTestOnlyAndExact(t *testing.T) {
	localRepo := filepath.Join(t.TempDir(), "repo")
	if err := validateRepositoryURL(localRepo); err == nil {
		t.Fatal("untrusted local repository path was accepted")
	}

	cleanup := TrustLocalRepositoryForTesting(localRepo)
	t.Cleanup(cleanup)
	if err := validateRepositoryURL(localRepo); err != nil {
		t.Fatalf("trusted test repository rejected: %v", err)
	}
	if err := validateRepositoryURL(localRepo + "-other"); err == nil {
		t.Fatal("trust for one local repository leaked to another path")
	}
}

func TestRepositoryCachePathRejectsWorkspaceTraversal(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for _, workspaceID := range []string{"../escape", `..\escape`, "/absolute", "nested/workspace"} {
		if path, err := repositoryCachePath(root, workspaceID, "https://github.com/org/repo.git"); err == nil || path != "" {
			t.Errorf("repositoryCachePath(%q) = %q, %v; want rejection", workspaceID, path, err)
		}
	}
	if path, err := repositoryCachePath(root, "a05b0e10-ee7a-4603-a72d-a548b2390cb2", "https://github.com/org/repo.git"); err != nil {
		t.Fatalf("valid cache path rejected: %v", err)
	} else if rel, err := filepath.Rel(root, path); err != nil || !filepath.IsLocal(rel) {
		t.Fatalf("cache path %q escaped root %q", path, root)
	}
}

func TestBareDirNameEscapesSeparatorCharacters(t *testing.T) {
	t.Parallel()

	withSlash := bareDirName("https://example.com/a/b.git")
	withPlus := bareDirName("https://example.com/a+b.git")
	if withSlash == withPlus {
		t.Fatalf("cache names collided: %q", withSlash)
	}
	if !strings.Contains(withPlus, "%2B") {
		t.Fatalf("literal plus was not escaped in %q", withPlus)
	}
}

func TestLookupRejectsCacheOriginCollision(t *testing.T) {
	t.Parallel()

	cache := New(t.TempDir(), testLogger())
	const (
		httpsURL = "https://example.com/org/repo.git"
		httpURL  = "http://example.com/org/repo.git"
	)
	if cache.BarePath("ws-1", httpsURL) != cache.BarePath("ws-1", httpURL) {
		t.Fatal("test URLs no longer exercise the shared legacy cache name")
	}
	barePath := cache.BarePath("ws-1", httpsURL)
	if err := os.MkdirAll(barePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(barePath, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := recordCacheOriginURL(barePath, httpsURL); err != nil {
		t.Fatal(err)
	}
	if got := cache.Lookup("ws-1", httpURL); got != "" {
		t.Fatalf("Lookup returned colliding cache %q for %q", got, httpURL)
	}
}

func TestValidateGitRef(t *testing.T) {
	t.Parallel()

	for _, ref := range []string{"main", "release/v2", "refs/heads/feature", "v1.2.3", "0123456789abcdef"} {
		if err := validateGitRef(ref); err != nil {
			t.Errorf("validateGitRef(%q) = %v, want nil", ref, err)
		}
	}
	for _, ref := range []string{
		"-c",
		"--upload-pack=evil",
		"feature..other",
		"feature@{1}",
		"feature lock",
		"feature\\other",
		"/main",
		"main/",
		"feature//other",
		".hidden",
		"feature.lock",
	} {
		if err := validateGitRef(ref); err == nil {
			t.Errorf("validateGitRef(%q) = nil, want error", ref)
		}
	}
}

func TestRepositoryCheckoutPathRejectsMaliciousBasename(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	if path, err := repositoryCheckoutPath(workDir, "https://example.com/...git"); err == nil || path != "" {
		t.Fatalf("repositoryCheckoutPath malicious URL = %q, %v; want rejection", path, err)
	}
	path, err := repositoryCheckoutPath(workDir, "https://example.com/org/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != workDir || filepath.Base(path) != "repo" {
		t.Fatalf("checkout path = %q, want %s/repo", path, workDir)
	}
}

func TestTaskKeyRemovesGitRefSyntax(t *testing.T) {
	t.Parallel()

	got := taskKey("../../hooks/-c core.sshCommand=evil")
	if len(got) > taskKeyLen || strings.ContainsAny(got, `./\- =`) {
		t.Fatalf("taskKey retained unsafe Git ref syntax: %q", got)
	}
}

func TestCreateWorktreeRejectsRedirectedIsolatedWorktree(t *testing.T) {
	t.Parallel()

	sourceRepo := createTestRepo(t)
	cache := New(t.TempDir(), testLogger())
	if err := cache.Sync("ws-1", []RepoInfo{{URL: sourceRepo}}); err != nil {
		t.Fatal(err)
	}
	params := WorktreeParams{
		WorkspaceID:         "ws-1",
		RepoURL:             sourceRepo,
		WorkDir:             t.TempDir(),
		AgentName:           "Codex",
		TaskID:              "11111111-1111-1111-1111-111111111111",
		IsolatedGitMetadata: true,
	}
	first, err := cache.CreateWorktree(params)
	if err != nil {
		t.Fatal(err)
	}

	outside := t.TempDir()
	sentinel := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runGit("-C", first.Path, "config", "core.worktree", outside); err != nil {
		t.Fatal(err)
	}

	params.Fresh = true
	params.TaskID = "22222222-2222-2222-2222-222222222222"
	if _, err := cache.CreateWorktree(params); err == nil {
		t.Fatal("redirected isolated checkout was accepted")
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
		t.Fatalf("outside worktree was modified: data=%q err=%v", data, err)
	}
}
