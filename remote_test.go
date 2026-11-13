package prstack

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRepoFromURL(t *testing.T) {
	for _, tt := range []struct {
		url, want string
	}{
		{"https://github.com/owner/repo.git", "github.com/owner/repo"},
		{"https://github.com/owner/repo/", "github.com/owner/repo"},
		{"git@github.com:owner/repo.git", "github.com/owner/repo"},
		{"github.com:owner/repo.git", "github.com/owner/repo"},
		{"ssh://git@github.com/owner/repo.git", "github.com/owner/repo"},
		{"ssh://git@ghe.example.com:2222/owner/repo.git", "ghe.example.com/owner/repo"},
		{"https://ghe.example.com/owner/repo.git", "ghe.example.com/owner/repo"},
		{"git://github.com/owner/repo.git", "github.com/owner/repo"},
	} {
		t.Run(tt.url, func(t *testing.T) {
			got, err := repoFromURL(tt.url)
			if err != nil || got != tt.want {
				t.Fatalf("repoFromURL(%q) = %q, %v; want %q", tt.url, got, err, tt.want)
			}
		})
	}
	for _, raw := range []string{"", "/tmp/repo", "file:///tmp/repo", "https://github.com/owner", "https://github.com/owner/repo/extra", "https://github.com//repo", "https://[invalid"} {
		t.Run(raw, func(t *testing.T) {
			if got, err := repoFromURL(raw); err == nil {
				t.Fatalf("repoFromURL(%q) = %q; want error", raw, got)
			}
		})
	}
}

func initTestRepo(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Chdir(t.TempDir())
	testGit(t, "init", "-b", "main")
	testGit(t, "config", "user.name", "Test")
	testGit(t, "config", "user.email", "test@example.com")
}

func testGit(t *testing.T, args ...string) string {
	t.Helper()
	r := git(args...)
	if r.Code != 0 {
		t.Fatalf("git %v: %s", args, r.Stderr)
	}
	return r.Stdout
}

func TestGithubRepoSelectedRemote(t *testing.T) {
	initTestRepo(t)
	testGit(t, "remote", "add", "origin", "https://github.com/upstream/project.git")
	testGit(t, "remote", "add", "nosnipe", "git@github.com:fork/project.git")

	got, err := githubRepo("nosnipe")
	if err != nil || got != "github.com/fork/project" {
		t.Fatalf("githubRepo(nosnipe) = %q, %v", got, err)
	}

	testGit(t, "remote", "set-url", "--push", "nosnipe", "ssh://git@ghe.example.com/team/project.git")
	got, err = githubRepo("nosnipe")
	if err != nil || got != "ghe.example.com/team/project" {
		t.Fatalf("githubRepo(nosnipe) with pushurl = %q, %v", got, err)
	}

	if _, err := githubRepo("missing"); err == nil {
		t.Fatal("missing remote: want error")
	}
	testGit(t, "remote", "add", "local", "/tmp/project.git")
	if _, err := githubRepo("local"); err == nil {
		t.Fatal("non-GitHub remote: want error")
	}
}

func TestPRCommandsUseSelectedRepo(t *testing.T) {
	initTestRepo(t)
	testGit(t, "commit", "--allow-empty", "-m", "base")
	bare := filepath.Join(t.TempDir(), "remote.git")
	testGit(t, "init", "--bare", bare)
	testGit(t, "remote", "add", "nosnipe", bare)

	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "gh.log")
	t.Setenv("GH_TEST_LOG", log)
	// A conflicting default must not affect explicit repository selection.
	t.Setenv("GH_REPO", "github.com/wrong/default")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$GH_TEST_LOG"
repo=
prev=
for arg do
  if [ "$prev" = --repo ]; then repo=$arg; fi
  prev=$arg
done
if [ "$repo" != ghe.example.com/team/project ]; then
  echo "wrong repository: $repo" >&2
  exit 1
fi
case "$2" in
  list)
    printf '%s\n' '[{"number":41,"url":"https://ghe.example.com/team/project/pull/41","state":"OPEN","title":"old title","baseRefName":"old-base","headRefName":"topic-aaaaaaaa"}]' ;;
  create) echo 'https://ghe.example.com/team/project/pull/42' ;;
  edit|view) ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	testGit(t, "commit", "--allow-empty", "-m", "first")
	first := testGit(t, "rev-parse", "HEAD")
	testGit(t, "commit", "--allow-empty", "-m", "second")
	second := testGit(t, "rev-parse", "HEAD")
	commits := []Commit{
		{SHA: first, Title: "first", Body: "first body", ChangeID: "I" + strings.Repeat("a", 40)},
		{SHA: second, Title: "second", Body: "second body", ChangeID: "I" + strings.Repeat("b", 40)},
	}
	const repo = "ghe.example.com/team/project"
	stack, err := buildStack(commits, "topic", "main", repo)
	if err != nil {
		t.Fatal(err)
	}
	stack.Remote, stack.Draft = "nosnipe", true
	if stack.Entries[0].PR.Number != 41 || stack.Entries[1].PR.Number != 0 {
		t.Fatalf("unexpected PR lookup: %+v", stack)
	}
	report, err := stack.Sync(SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range report.Entries {
		if result.Err != nil {
			t.Fatalf("sync entry %d: %v", result.Idx, result.Err)
		}
	}
	if stack.Entries[1].PR.Number != 42 {
		t.Fatalf("PR not created: %+v", stack.Entries[1])
	}
	if err := stack.Open(1); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{
		"pr list --repo " + repo + " --state all --limit 500 --json number,url,state,title,baseRefName,isDraft,headRefName,body",
		"pr edit 41 --repo " + repo + " --title first --body-file - --base main",
		"pr create --repo " + repo + " --head topic-bbbbbbbb --base topic-aaaaaaaa --title second --body-file - --draft",
		"pr view 41 --repo " + repo + " --web",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gh commands:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, entry := range stack.Entries {
		cmd := exec.Command("git", "--git-dir="+bare, "rev-parse", "refs/heads/"+entry.Branch)
		out, err := cmd.Output()
		if err != nil || strings.TrimSpace(string(out)) != entry.Commit.SHA {
			t.Errorf("remote branch %s = %q, %v; want %s", entry.Branch, out, err, entry.Commit.SHA)
		}
	}

	// The new library entry points must use the same remote selection.
	testGit(t, "remote", "set-url", "nosnipe", "https://ghe.example.com/team/project.git")
	testGit(t, "remote", "add", "origin", "https://github.com/wrong/default.git")
	testGit(t, "config", "prstack.remote", "nosnipe")
	prs, err := ListPRs("topic")
	if err != nil || len(prs) != 1 || prs[0].Number != 41 {
		t.Fatalf("ListPRs = %+v, %v", prs, err)
	}
	stack.Entries[0].Branch = "unbound"
	if err := stack.Link(1, "topic-aaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	got = strings.Split(strings.TrimSpace(string(data)), "\n")
	want = append(want,
		"pr list --repo "+repo+" --state all --limit 500 --json number,url,state,title,baseRefName,isDraft,headRefName,body",
		"pr list --repo "+repo+" --state all --limit 500 --json number,url,state,title,baseRefName,isDraft,headRefName,body",
		"pr edit 41 --repo "+repo+" --title first --body-file -",
	)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gh commands:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
