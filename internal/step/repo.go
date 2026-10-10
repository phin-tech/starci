package step

import (
	"net/url"
	"strings"
)

// RepoPath is the owner and name part of the address a repository is
// fetched from, however the address is spelled: over HTTPS or SSH, with or
// without a user or ".git". A path on this machine is returned as it is.
func RepoPath(cloneURL string) string {
	s := strings.TrimSuffix(strings.TrimRight(cloneURL, "/"), ".git")
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		return strings.Trim(u.Path, "/")
	}
	// git@host:owner/repo, which is not a URL.
	if at := strings.Index(s, "@"); at >= 0 && !strings.Contains(s[:at], "/") {
		if host, path, ok := strings.Cut(s[at+1:], ":"); ok && !strings.Contains(host, "/") {
			return strings.Trim(path, "/")
		}
	}
	return s
}
