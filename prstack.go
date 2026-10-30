// Package prstack manages a stack of GitHub pull requests — one commit per
// PR — without requiring Gerrit-style Change-Id trailers.
//
// A stack is the set of commits between the upstream base and your working
// branch's head.  Each commit gets its own branch and PR, chained base-to-head.
// Commits are tracked across rebases/amends/reorders by reconciling them
// against manifests embedded as hidden HTML comments in each PR body:
//
//  1. manifest change-id  (exact — used when the commit has a Change-Id trailer)
//  2. manifest patch-id   (exact — survives clean rebase and reorder)
//  3. legacy branch name  (exact — adopts pre-manifest stacks)
//  4. title + author     (heuristic — catches amends; flagged for review)
//
// Commit messages are never modified.  Existing stacks built with
// Change-Id-derived branch names keep working (rule 3), and one Sync call
// seeds manifests onto every PR so future rebases match exactly.
//
// Typical use:
//
//	stack, err := prstack.Load(prstack.Options{Prefix: "raggi/foo"})
//	if err != nil {
//		return err
//	}
//	report, err := stack.Sync(prstack.SyncOptions{})
//
// All commands run `git` and `gh` from the current working directory, so
// call Load from inside the repository you are managing.
package prstack

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ── options ───────────────────────────────────────────────────────────────────

// Options configures stack resolution.  Empty fields fall back to per-repo
// git config (`prstack.*`) and then to defaults; see each field.
type Options struct {
	// Base is the upstream base branch.  Empty: `prstack.base`, then "main".
	Base string
	// Remote is the git remote to push to.  Empty: `prstack.remote`,
	// then "origin" if present, else the first remote.
	Remote string
	// Prefix is the working branch / stack prefix.  Empty: `prstack.prefix`,
	// then the current branch.
	Prefix string
	// Draft controls whether new PRs are created as drafts.  Nil:
	// `prstack.draft`, then true.
	Draft *bool
}

// resolveStr resolves a flag/config/default string option.
func resolveStr(flagVal, configKey string, def func() (string, error)) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if v := gitConfig(configKey); v != "" {
		return v, nil
	}
	if def != nil {
		return def()
	}
	return "", nil
}

func resolveDraft(flagVal *bool) bool {
	if flagVal != nil {
		return *flagVal
	}
	if v := gitConfig("draft"); v != "" {
		return v != "false" && v != "0" && v != "no"
	}
	return true
}

// ── data types ────────────────────────────────────────────────────────────────

// PR state values as used in StackEntry.PR.State and Orphan.PR.State.
const (
	PRStateOpen   = "OPEN"
	PRStateDraft  = "DRAFT"
	PRStateMerged = "MERGED"
	PRStateClosed = "CLOSED"
)

// Commit represents a single commit in the stack.
type Commit struct {
	SHA      string
	Title    string // first line of the commit message
	Body     string // everything after the first blank line
	Author   string // "Name <email>"
	ChangeID string // full "I<40hex>" value, or "" if absent
	PatchID  string // stable hash of the commit diff, or "" if unset
}

// PRInfo describes a PR as recorded on GitHub.
type PRInfo struct {
	Number int
	URL    string
	State  string // OPEN, DRAFT, MERGED, CLOSED
	Title  string
	Base   string // base branch as currently recorded on GitHub
	// Manifest is the prstack manifest parsed from the PR body, if any.
	Manifest *Manifest
}

// StackEntry is one slot in the stack — one commit, one branch, one PR.
type StackEntry struct {
	Idx         int // 1-indexed (1 = oldest, closest to base)
	Commit      Commit
	Branch      string // branch for this commit
	BaseBranch  string // PR target on GitHub (previous entry's branch, or the stack base)
	PR          PRInfo // zero value if no PR
	MatchReason string // change-id | patch-id | branch-name | title | new
	Flagged     bool   // heuristic match — verify before trusting
}

// MatchReason values reported in StackEntry.MatchReason.
const (
	MatchChangeID  = "change-id"   // exact: manifest change-id == commit Change-Id trailer
	MatchPatchID   = "patch-id"    // exact: manifest patch-id == commit patch-id
	MatchBranchNam = "branch-name" // exact: legacy Change-Id/SHA-derived branch name
	MatchTitle     = "title"       // heuristic: title (+author); catches amends
	MatchNew       = "new"         // no match; Sync mints a fresh branch + PR
)

// Orphan is a stack branch/PR whose commit no longer exists in the stack
// (squash-merged, dropped, or superseded by a rewritten commit).
type Orphan struct {
	Branch string
	PR     PRInfo
}

// Stack is the reconciled state of one working branch.
type Stack struct {
	Prefix  string // working-branch prefix
	Base    string // upstream base branch
	Remote  string // git remote
	Draft   bool   // whether Sync creates new PRs as drafts
	Entries []StackEntry
	Orphans []Orphan
}

// Entry returns stack entry n (1-indexed, oldest first).
func (s *Stack) Entry(n int) (*StackEntry, error) {
	if n < 1 || n > len(s.Entries) {
		return nil, fmt.Errorf("index %d out of range (stack has %d entries)", n, len(s.Entries))
	}
	return &s.Entries[n-1], nil
}

// ── loading ───────────────────────────────────────────────────────────────────

// Load resolves options against git config and repository state, then
// reconciles the local commits against the stack's existing PRs.  It hits
// the network once (one `gh pr list`) plus a `git fetch` of the base branch.
//
// Load must be called from inside a git repository, on a non-base branch.
func Load(opts Options) (*Stack, error) {
	branch, err := currentBranch()
	if err != nil {
		return nil, err
	}
	if isBaseBranch(branch) {
		return nil, fmt.Errorf("on branch %q — checkout a feature branch first", branch)
	}

	remote, err := resolveStr(opts.Remote, "remote", detectRemote)
	if err != nil {
		return nil, err
	}
	base, err := resolveStr(opts.Base, "base", func() (string, error) { return "main", nil })
	if err != nil {
		return nil, err
	}
	prefix, err := resolveStr(opts.Prefix, "prefix", func() (string, error) { return branch, nil })
	if err != nil {
		return nil, err
	}

	fork, err := findForkSHA(remote, base)
	if err != nil {
		return nil, err
	}
	commits, err := commitsSince(fork)
	if err != nil {
		return nil, err
	}

	stack, err := buildStack(commits, prefix, base)
	if err != nil {
		return nil, err
	}
	stack.Remote = remote
	stack.Draft = resolveDraft(opts.Draft)
	return stack, nil
}

func isBaseBranch(branch string) bool {
	switch branch {
	case "HEAD", "main", "master", "trunk":
		return true
	}
	return false
}

// ── git helpers ───────────────────────────────────────────────────────────────

func currentBranch() (string, error) {
	out, err := gitOutErr("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("not a git repository: %w", err)
	}
	return out, nil
}

func detectRemote() (string, error) {
	out := gitOut("remote")
	for _, r := range strings.Split(out, "\n") {
		if r == "origin" {
			return "origin", nil
		}
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) > 0 && lines[0] != "" {
		return lines[0], nil
	}
	return "", fmt.Errorf("no git remotes configured")
}

func findForkSHA(remote, base string) (string, error) {
	// Fetch quietly so we have an up-to-date remote tracking ref.
	git("fetch", remote, base, "--quiet")
	ref := remote + "/" + base
	r := git("merge-base", "HEAD", ref)
	if r.Code != 0 {
		// Fallback: try without remote prefix (local branch exists).
		r = git("merge-base", "HEAD", base)
	}
	if r.Code != 0 {
		return "", fmt.Errorf("cannot find merge-base between HEAD and %s/%s", remote, base)
	}
	return r.Stdout, nil
}

var changeIDRe = regexp.MustCompile(`(?im)^Change-Id:\s*(I[0-9a-f]{40})\s*$`)

func extractChangeID(msg string) string {
	m := changeIDRe.FindStringSubmatch(msg)
	if m != nil {
		return m[1]
	}
	return ""
}

// patchIDOf returns the stable patch-id of a commit's diff — the hash git
// itself uses to recognize "the same change" across rebases.  It is stable
// under reorder and clean rebase, but changes when the commit is amended
// or conflicts are resolved against a moved base.
func patchIDOf(sha string) string {
	d := runCmd("", "git", "diff-tree", "-p", sha)
	if d.Code != 0 || strings.TrimSpace(d.Stdout) == "" {
		return ""
	}
	r := runCmd(d.Stdout, "git", "patch-id", "--stable")
	if r.Code != 0 {
		return ""
	}
	f := strings.Fields(r.Stdout)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

func commitsSince(forkSHA string) ([]Commit, error) {
	log, err := gitOutErr("log", "--format=%H%x1f%an%x1f%ae%x1f%B%x1e", forkSHA+"..HEAD")
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}
	if log == "" {
		return nil, nil
	}
	commits := []Commit{}
	for _, rec := range strings.Split(log, "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		if strings.TrimSpace(rec) == "" {
			continue
		}
		parts := strings.SplitN(rec, "\x1f", 4)
		if len(parts) != 4 {
			continue
		}
		sha, name, email, msg := parts[0], parts[1], parts[2], parts[3]
		lines := strings.SplitN(msg, "\n", 2)
		title := lines[0]
		body := ""
		if len(lines) == 2 {
			body = strings.TrimSpace(lines[1])
		}
		commits = append(commits, Commit{
			SHA:      sha,
			Title:    title,
			Body:     body,
			Author:   fmt.Sprintf("%s <%s>", name, email),
			ChangeID: extractChangeID(msg),
			PatchID:  patchIDOf(sha),
		})
	}
	// reverse so index 0 is oldest (closest to base)
	for i, j := 0, len(commits)-1; i < j; i, j = i+1, j-1 {
		commits[i], commits[j] = commits[j], commits[i]
	}
	return commits, nil
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func localBranchExists(name string) bool {
	r := git("show-ref", "--verify", "refs/heads/"+name)
	return r.Code == 0
}

func remoteBranchExists(remote, name string) bool {
	r := git("show-ref", "--verify", "refs/remotes/"+remote+"/"+name)
	return r.Code == 0
}

// ── branch naming ─────────────────────────────────────────────────────────────

// branchName mints a stack branch name for a brand-new commit: the working
// branch prefix plus 8 hex chars of the Change-Id if present, else the short
// SHA.  Once a commit has been matched to a PR, the PR's existing branch
// name is used instead (see reconcile).
func branchName(prefix string, c Commit) string {
	if c.ChangeID != "" {
		// ChangeID is "I<40hex>" — skip the leading 'I', take 8 chars.
		return prefix + "-" + c.ChangeID[1:9]
	}
	return prefix + "-" + shortSHA(c.SHA)
}

// ── git config ────────────────────────────────────────────────────────────────

func gitConfig(key string) string {
	r := git("config", "--local", "prstack."+key)
	return r.Stdout // empty string if not set
}

// ── PR lookup ─────────────────────────────────────────────────────────────────

// ListPRs returns every PR (any state) whose head branch belongs to the
// stack with the given prefix, with its manifest parsed if present.  This
// is the remote half of the stack state; combine with local commits via
// Load for full reconciliation.
func ListPRs(prefix string) ([]PRInfo, error) {
	prs, err := collectPRs(prefix)
	if err != nil {
		return nil, err
	}
	out := make([]PRInfo, 0, len(prs))
	for _, br := range sortedBranches(prs) {
		rec := prs[br]
		info := prInfoFromJSON(rec.PR)
		info.Manifest = rec.Man
		out = append(out, info)
	}
	return out, nil
}

func sortedBranches(prs map[string]prRecord) []string {
	keys := make([]string, 0, len(prs))
	for k := range prs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ── reconciliation ────────────────────────────────────────────────────────────

// buildStack reconciles local commits against the stack's existing PRs
// (one gh call) and chains each entry's base to the branch below it.
func buildStack(commits []Commit, prefix, base string) (*Stack, error) {
	prs, err := collectPRs(prefix)
	if err != nil {
		return nil, err
	}
	return reconcile(commits, prefix, base, prs), nil
}

// reconcile pairs commits with existing PR manifests.  Matching precedence
// (order-independent, so reordering the stack is free):
//
//  1. manifest change-id == commit Change-Id trailer   (exact)
//  2. manifest patch-id == commit patch-id            (exact)
//  3. legacy branch name derived from the commit       (exact — adopts
//     pre-manifest stacks, incl. Change-Id- and sha-named branches)
//  4. manifest title == commit title (+ author)       (heuristic —
//     catches amends, which change the patch-id; flagged for review)
//
// Anything unmatched mints a fresh branch (new PR on sync); leftover PRs
// become orphans.
func reconcile(commits []Commit, prefix, base string, prs map[string]prRecord) *Stack {
	used := map[string]bool{}
	entries := make([]StackEntry, 0, len(commits))

	for i, commit := range commits {
		entry := StackEntry{Idx: i + 1, Commit: commit}
		branch, reason := "", ""

		// 1. exact change-id match (commits carrying Change-Id trailers).
		if commit.ChangeID != "" {
			if br, rec, ok := findManifest(prs, used, func(m *Manifest) bool {
				return m.ChangeID == commit.ChangeID
			}); ok {
				branch, reason = br, MatchChangeID
				entry.PR = prInfoFromJSON(rec.PR)
			}
		}

		// 2. exact patch-id match (clean rebase / reorder).
		if branch == "" && commit.PatchID != "" {
			if br, rec, ok := findManifest(prs, used, func(m *Manifest) bool {
				return m.PatchID == commit.PatchID
			}); ok {
				branch, reason = br, MatchPatchID
				entry.PR = prInfoFromJSON(rec.PR)
			}
		}

		// 3. legacy branch-name match: PRs created before manifests existed
		// are named after the commit's Change-Id/short SHA.
		if branch == "" {
			br := branchName(prefix, commit)
			if rec, ok := prs[br]; ok && !used[br] {
				branch, reason = br, MatchBranchNam
				entry.PR = prInfoFromJSON(rec.PR)
			}
		}

		// 4. title+author heuristic (amends change the patch-id but almost
		// always keep the title).  Flagged so callers can ask the user.
		if branch == "" {
			if br, rec, ok := findManifest(prs, used, func(m *Manifest) bool {
				return m.Title == commit.Title &&
					(m.Author == "" || m.Author == commit.Author)
			}); ok {
				branch, reason = br, MatchTitle
				entry.Flagged = true
				entry.PR = prInfoFromJSON(rec.PR)
			}
		}

		if branch == "" {
			branch = branchName(prefix, commit)
			reason = MatchNew
		}
		used[branch] = true

		entry.Branch = branch
		entry.MatchReason = reason
		entries = append(entries, entry)
	}

	// Chain each entry's PR base to the branch below it.
	for i := range entries {
		if i == 0 {
			entries[i].BaseBranch = base
		} else {
			entries[i].BaseBranch = entries[i-1].Branch
		}
	}

	// Orphans: PRs in this stack matched by no commit.
	orphans := []Orphan{}
	for _, br := range sortedBranches(prs) {
		if used[br] {
			continue
		}
		orphans = append(orphans, Orphan{Branch: br, PR: prInfoFromJSON(prs[br].PR)})
	}

	return &Stack{Prefix: prefix, Base: base, Entries: entries, Orphans: orphans}
}

// findManifest returns the first unused PR record whose manifest satisfies
// pred, for deterministic matching.
func findManifest(prs map[string]prRecord, used map[string]bool, pred func(*Manifest) bool) (string, prRecord, bool) {
	for _, br := range sortedBranches(prs) {
		if used[br] {
			continue
		}
		rec := prs[br]
		if rec.Man == nil {
			continue
		}
		if pred(rec.Man) {
			return br, rec, true
		}
	}
	return "", prRecord{}, false
}
