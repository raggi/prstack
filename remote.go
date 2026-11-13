package prstack

import (
	"fmt"
	"net/url"
	"strings"
)

func githubRepo(remote string) (string, error) {
	r := git("remote", "get-url", "--push", remote)
	if r.Code != 0 {
		return "", fmt.Errorf("remote %q: %s", remote, r.Stderr)
	}
	repo, err := repoFromURL(r.Stdout)
	if err != nil {
		return "", fmt.Errorf("remote %q: %w", remote, err)
	}
	return repo, nil
}

func repoFromURL(raw string) (string, error) {
	var host, path string
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", fmt.Errorf("invalid GitHub remote URL %q: %w", raw, err)
		}
		switch u.Scheme {
		case "https", "http", "ssh", "git":
			host, path = u.Hostname(), u.Path
		}
	} else if before, after, ok := strings.Cut(raw, ":"); ok {
		// Git also accepts scp-style SSH URLs: git@host:owner/repo.git.
		_, host, ok = strings.Cut(before, "@")
		if !ok {
			host = before
		}
		path = after
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if host == "" || strings.ContainsAny(host, "/\\") || len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("cannot determine GitHub repository from remote URL %q", raw)
	}
	return host + "/" + path, nil
}

func (s *Stack) repository() (string, error) {
	if s.repo != "" {
		return s.repo, nil
	}
	remote, err := resolveStr(s.Remote, "remote", detectRemote)
	if err != nil {
		return "", err
	}
	return githubRepo(remote)
}
