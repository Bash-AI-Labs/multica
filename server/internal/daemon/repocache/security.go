package repocache

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const gitRefMaxLength = 255

var trustedLocalRepositoriesForTesting sync.Map

// TrustLocalRepositoryForTesting permits one exact local repository path or
// file URL while a Go test is running. Production repository data must always
// use an explicitly supported remote transport.
func TrustLocalRepositoryForTesting(repo string) func() {
	name := filepath.Base(os.Args[0])
	if !strings.HasSuffix(name, ".test") && !strings.HasSuffix(name, ".test.exe") {
		panic("TrustLocalRepositoryForTesting called outside a Go test")
	}
	trustedLocalRepositoriesForTesting.Store(repo, struct{}{})
	return func() {
		trustedLocalRepositoriesForTesting.Delete(repo)
	}
}

func validateRepositoryURL(raw string) error {
	if raw == "" {
		return errors.New("repository URL is required")
	}
	if raw != strings.TrimSpace(raw) {
		return errors.New("repository URL must not have surrounding whitespace")
	}
	for _, r := range raw {
		if r <= ' ' || r == '\x7f' {
			return errors.New("repository URL must not contain spaces or control characters")
		}
	}
	if strings.HasPrefix(raw, "-") {
		return errors.New("repository URL must not begin with '-'")
	}
	if _, ok := trustedLocalRepositoriesForTesting.Load(raw); ok {
		return validateTrustedLocalRepository(raw)
	}
	if strings.Contains(raw, "\\") {
		return errors.New("repository URL must not contain backslashes")
	}

	parsed, err := url.Parse(raw)
	if err == nil && parsed.Scheme != "" {
		if parsed.Opaque != "" {
			return errors.New("repository URL must not use an opaque transport")
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https", "ssh", "git":
		default:
			return fmt.Errorf("unsupported repository URL scheme %q", parsed.Scheme)
		}
		if parsed.Host == "" || parsed.Hostname() == "" {
			return errors.New("repository URL host is required")
		}
		if parsed.RawQuery != "" || parsed.Fragment != "" {
			return errors.New("repository URL must not contain a query or fragment")
		}
		return validateRepositoryPath(parsed.Path)
	}

	return validateSCPLikeRepositoryURL(raw)
}

func validateTrustedLocalRepository(raw string) error {
	if parsed, err := url.Parse(raw); err == nil && parsed.Scheme == "file" {
		if parsed.RawQuery != "" || parsed.Fragment != "" {
			return errors.New("local repository URL must not contain a query or fragment")
		}
		if !filepath.IsAbs(filepath.FromSlash(parsed.Path)) {
			return errors.New("local repository URL path must be absolute")
		}
		return nil
	}
	if !filepath.IsAbs(raw) {
		return errors.New("local repository path must be absolute")
	}
	return nil
}

func validateSCPLikeRepositoryURL(raw string) error {
	if strings.Contains(raw, "://") {
		return errors.New("repository URL uses an unsupported transport")
	}

	colon := strings.Index(raw, ":")
	if closeBracket := strings.Index(raw, "]:"); closeBracket >= 0 {
		colon = closeBracket + 1
	}
	if colon <= 0 || colon == len(raw)-1 {
		return errors.New("repository URL must be http(s), ssh, git, or scp-like")
	}

	hostPart := raw[:colon]
	repoPath := raw[colon+1:]
	if strings.ContainsAny(hostPart, `/\`) || strings.Count(hostPart, "@") > 1 {
		return errors.New("invalid scp-like repository host")
	}
	host := hostPart
	if at := strings.Index(hostPart, "@"); at >= 0 {
		if at == 0 {
			return errors.New("scp-like repository user is required before '@'")
		}
		host = hostPart[at+1:]
	}
	if host == "" || strings.HasPrefix(host, "-") {
		return errors.New("invalid scp-like repository host")
	}
	if strings.HasPrefix(host, "[") {
		if !strings.HasSuffix(host, "]") || len(host) == 2 {
			return errors.New("invalid bracketed scp-like repository host")
		}
	} else if strings.Contains(host, ":") {
		return errors.New("IPv6 scp-like repository hosts must be bracketed")
	}
	return validateRepositoryPath(repoPath)
}

func validateRepositoryPath(repoPath string) error {
	repoPath = strings.Trim(repoPath, "/")
	if repoPath == "" {
		return errors.New("repository path is required")
	}
	for _, r := range repoPath {
		if r <= ' ' || r == '\x7f' {
			return errors.New("repository path must not contain spaces or control characters")
		}
	}
	for _, segment := range strings.Split(repoPath, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return errors.New("repository path must not contain empty, '.' or '..' segments")
		}
	}
	name := strings.TrimSuffix(filepath.Base(filepath.FromSlash(repoPath)), ".git")
	if err := validatePathComponent(name, "repository name"); err != nil {
		return err
	}
	return nil
}

func validatePathComponent(value, field string) error {
	if value == "" || value == "." || value == ".." {
		return fmt.Errorf("%s must be a non-empty path component", field)
	}
	if value != strings.TrimSpace(value) || strings.ContainsAny(value, `/\<>:"|?*`) {
		return fmt.Errorf("%s must be a single path component", field)
	}
	if filepath.VolumeName(value) != "" || !filepath.IsLocal(value) || filepath.Base(value) != value {
		return fmt.Errorf("%s must be a local path component", field)
	}
	return nil
}

func joinUnder(root string, components ...string) (string, error) {
	candidate := filepath.Join(append([]string{root}, components...)...)
	rel, err := filepath.Rel(root, candidate)
	if err != nil || !filepath.IsLocal(rel) {
		if err == nil {
			err = errors.New("path escapes root")
		}
		return "", err
	}
	return candidate, nil
}

func workspaceCacheDir(root, workspaceID string) (string, error) {
	if err := validatePathComponent(workspaceID, "workspace ID"); err != nil {
		return "", err
	}
	return joinUnder(root, workspaceID)
}

func repositoryCachePath(root, workspaceID, repoURL string) (string, error) {
	if err := validateRepositoryURL(repoURL); err != nil {
		return "", err
	}
	wsDir, err := workspaceCacheDir(root, workspaceID)
	if err != nil {
		return "", err
	}
	name := bareDirName(repoURL)
	if err := validatePathComponent(name, "repository cache name"); err != nil {
		return "", err
	}
	return joinUnder(wsDir, name)
}

func repositoryCheckoutPath(workDir, repoURL string) (string, error) {
	if err := validateRepositoryURL(repoURL); err != nil {
		return "", err
	}
	name := repoNameFromURL(repoURL)
	if err := validatePathComponent(name, "repository checkout name"); err != nil {
		return "", err
	}
	return joinUnder(workDir, name)
}

func validateGitRef(ref string) error {
	if ref == "" {
		return nil
	}
	if len(ref) > gitRefMaxLength {
		return fmt.Errorf("ref must be at most %d characters", gitRefMaxLength)
	}
	if strings.HasPrefix(ref, "-") {
		return errors.New("ref must not begin with '-'")
	}
	for _, r := range ref {
		if r <= ' ' || r == '\x7f' || strings.ContainsRune("~^:?*[\\", r) {
			return errors.New("ref contains a character reserved by Git")
		}
	}
	if strings.Contains(ref, "..") ||
		strings.Contains(ref, "@{") ||
		ref == "@" ||
		strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "//") ||
		strings.HasSuffix(ref, ".") {
		return errors.New("ref is not a valid branch, tag, or commit")
	}
	for _, segment := range strings.Split(ref, "/") {
		if strings.HasPrefix(segment, ".") || strings.HasSuffix(segment, ".lock") {
			return errors.New("ref is not a valid branch, tag, or commit")
		}
	}
	return nil
}

func requirePathWithin(root, candidate, field string) (string, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve %s root: %w", field, err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(absoluteRoot); resolveErr == nil {
		absoluteRoot = resolved
	}
	absoluteCandidate, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", field, err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(absoluteCandidate); resolveErr == nil {
		absoluteCandidate = resolved
	}
	rel, err := filepath.Rel(absoluteRoot, absoluteCandidate)
	if err != nil || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("%s %q is outside %q", field, candidate, root)
	}
	return filepath.Clean(absoluteCandidate), nil
}
