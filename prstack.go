// prstack — manage a stack of GitHub pull requests.
//
// Each commit in your local branch (rebased on origin/main) becomes its own
// PR.  Commits are tracked across rebases/amends/reorders WITHOUT requiring
// Gerrit-style Change-Id trailers:
//
//  1. A machine-readable manifest is embedded as an HTML comment in each
//     PR body (invisible on GitHub) recording the commit's stable hints:
//     change-id, patch-id, title, author.
//  2. On sync, local commits are reconciled against the manifests of
//     existing PRs (matching is order-independent, so reordering works):
//     - exact change-id match            (if a Change-Id trailer exists)
//     - exact patch-id match              (clean rebase / reorder)
//     - legacy branch-name match          (Change-Id/sha-derived names)
//     - title+author heuristic match      (amends — flagged for review)
//  3. Unmatched commits become new PRs; unmatched PRs are reported as
//     orphans; ambiguous bindings can be fixed with `prstack link`.
//
// If commits DO carry Change-Id trailers they are used as the strongest
// key (and stripped from the PR body), but they are never required.
//
// Usage:
//
//	prstack status              show stack state
//	prstack sync [--dry-run]    push branches and create/update PRs
//	prstack checkout N          switch to branch for commit N (1-indexed)
//	prstack open N              open PR N in browser
//	prstack link N BRANCH       manually bind commit N to an existing branch/PR
//	prstack clean [--dry-run]   delete branches for merged/closed PRs
//
// Flags (all commands):
//
//	--base BRANCH    upstream base branch      (default: main)
//	--remote NAME    git remote                (default: origin)
//	--prefix PFX     branch prefix override
//	--draft          create new PRs as draft   (default: true)
//	--no-draft       create new PRs as ready
//	--dry-run        print actions, no changes
//	-v               verbose output
//
// Per-repo git config (checked before flag defaults):
//
//	git config prstack.base    main
//	git config prstack.remote  origin
//	git config prstack.prefix  raggi/eventbus-foo
//	git config prstack.draft   true
//
// Branch naming:
//
//	working branch:  raggi/eventbus-foo
//	stack branches:  raggi/eventbus-foo-<suffix>
//
// The suffix is 8 hex chars of the Change-Id when present, else the short
// SHA.  This is only used to MINT a name for a brand-new stack entry;
// once a commit is matched to a PR, the PR's existing branch name is kept
// forever (recorded in the manifest).
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ── ANSI colours ──────────────────────────────────────────────────────────────

var isTTY = func() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}()

const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiRed    = "\033[31m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiBlue   = "\033[34m"
	ansiCyan   = "\033[36m"
)

func col(s string, codes ...string) string {
	if !isTTY {
		return s
	}
	return strings.Join(codes, "") + s + ansiReset
}

// ── subprocess helpers ────────────────────────────────────────────────────────

// runResult holds the output of a subprocess call.
type runResult struct {
	Stdout string
	Stderr string
	Code   int
}

func runCmd(stdin string, args ...string) runResult {
	cmd := exec.Command(args[0], args[1:]...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = 1
		}
	}
	return runResult{
		Stdout: strings.TrimRight(stdout.String(), "\n"),
		Stderr: strings.TrimRight(stderr.String(), "\n"),
		Code:   code,
	}
}

// mustRun runs a command and exits on failure.
func mustRun(args ...string) string {
	r := runCmd("", args...)
	if r.Code != 0 {
		die("%s\n%s", strings.Join(args, " "), r.Stderr)
	}
	return r.Stdout
}

func git(args ...string) runResult {
	return runCmd("", append([]string{"git"}, args...)...)
}

func gitOut(args ...string) string {
	return mustRun(append([]string{"git"}, args...)...)
}

func ghOut(args ...string) string {
	return mustRun(append([]string{"gh"}, args...)...)
}

// passthrough runs a command inheriting the terminal (for interactive use).
func passthrough(name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.Exit(1)
	}
}

// ── data types ────────────────────────────────────────────────────────────────

// Commit represents a single commit in the stack.
type Commit struct {
	SHA      string
	Title    string // first line of commit message
	Body     string // everything after the first blank line
	Author   string // "Name <email>"
	ChangeID string // full "I<40hex>" value, or "" if absent
	PatchID  string // stable hash of the commit diff, or "" if unset
}

// StackEntry is one slot in the stack — one commit, one branch, one PR.
type StackEntry struct {
	Idx         int // 1-indexed (1 = oldest, closest to base)
	Commit      Commit
	Branch      string // local/remote branch for this commit
	BaseBranch  string // PR target on GitHub
	PRNumber    int    // 0 if no PR found
	PRURL       string
	PRState     string // OPEN, DRAFT, MERGED, CLOSED
	PRTitle     string
	PRBase      string // base as currently recorded on GitHub
	MatchReason string // change-id | patch-id | branch-name | title | new
	Flagged     bool   // heuristic match — verify before trusting
}

// Orphan is a stack branch/PR whose commit no longer exists in the stack
// (squash-merged, dropped, or superseded by a new commit).
type Orphan struct {
	Branch string
	PR     prJSON
}

// Stack is the full reconciled state of a working branch.
type Stack struct {
	Prefix  string
	Base    string
	Entries []StackEntry
	Orphans []Orphan
}

// ── git helpers ───────────────────────────────────────────────────────────────

func currentBranch() string {
	return gitOut("rev-parse", "--abbrev-ref", "HEAD")
}

func detectRemote() string {
	out := gitOut("remote")
	for _, r := range strings.Split(out, "\n") {
		if r == "origin" {
			return "origin"
		}
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) > 0 && lines[0] != "" {
		return lines[0]
	}
	die("no git remotes configured")
	return ""
}

func findForkSHA(remote, base string) string {
	// Fetch quietly so we have an up-to-date remote tracking ref.
	git("fetch", remote, base, "--quiet")
	ref := remote + "/" + base
	r := git("merge-base", "HEAD", ref)
	if r.Code != 0 {
		// Fallback: try without remote prefix (local branch exists).
		r = git("merge-base", "HEAD", base)
	}
	if r.Code != 0 {
		die("cannot find merge-base between HEAD and %s/%s", remote, base)
	}
	return r.Stdout
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

func commitsSince(forkSHA string) []Commit {
	log := gitOut("log", "--format=%H%x1f%an%x1f%ae%x1f%B%x1e", forkSHA+"..HEAD")
	if log == "" {
		return nil
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
	return commits
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

// branchName mints a stack branch name for a brand-new commit.
// Uses the full working branch as prefix, appends 8 hex chars of the
// Change-Id if present, else the short SHA.  Once a commit has been matched
// to a PR, the PR's existing branch name is used instead (see buildStack).
func branchName(prefix string, c Commit) string {
	if c.ChangeID != "" {
		// ChangeID is "I<40hex>" — skip the leading 'I', take 8 chars.
		return prefix + "-" + c.ChangeID[1:9]
	}
	return prefix + "-" + shortSHA(c.SHA)
}

// ── git config helpers ────────────────────────────────────────────────────────

func gitConfig(key string) string {
	r := git("config", "--local", "prstack."+key)
	return r.Stdout // empty string if not set
}

// ── PR manifest ──────────────────────────────────────────────────────────────

// Manifest is the machine-readable mapping between a stack entry and its
// PR, embedded as an HTML comment in the PR body.  GitHub renders HTML
// comments as invisible, so the PR body looks untouched to humans while
// prstack can re-read the mapping on any clone.
type Manifest struct {
	Stack    string // working-branch prefix this PR belongs to
	Branch   string
	PR       int
	Slot     int // 1-indexed position at last sync (informational)
	Title    string
	Author   string
	ChangeID string // last-known Change-Id trailer, "" if none
	PatchID  string // last-known patch-id of the commit
}

var manifestBlockRe = regexp.MustCompile(`(?s)<!--\s*prstack v1\s*\n(.*?)-->`)
var manifestLineRe = regexp.MustCompile(`(?m)^([a-z-]+):\s*(.*)$`)

// parseManifest extracts the prstack manifest from a PR body, if any.
func parseManifest(body string) (*Manifest, bool) {
	m := manifestBlockRe.FindStringSubmatch(body)
	if m == nil {
		return nil, false
	}
	man := &Manifest{}
	for _, ln := range manifestLineRe.FindAllStringSubmatch(m[1], -1) {
		k, v := ln[1], strings.TrimSpace(ln[2])
		switch k {
		case "stack":
			man.Stack = v
		case "branch":
			man.Branch = v
		case "pr":
			man.PR, _ = strconv.Atoi(v)
		case "slot":
			man.Slot, _ = strconv.Atoi(v)
		case "title":
			man.Title = v
		case "author":
			man.Author = v
		case "change-id":
			man.ChangeID = v
		case "patch-id":
			man.PatchID = v
		}
	}
	return man, true
}

func (man Manifest) serialize() string {
	var b strings.Builder
	b.WriteString("<!-- prstack v1\n")
	fmt.Fprintf(&b, "stack: %s\n", man.Stack)
	fmt.Fprintf(&b, "branch: %s\n", man.Branch)
	if man.PR != 0 {
		fmt.Fprintf(&b, "pr: %d\n", man.PR)
	}
	if man.Slot != 0 {
		fmt.Fprintf(&b, "slot: %d\n", man.Slot)
	}
	fmt.Fprintf(&b, "title: %s\n", man.Title)
	if man.Author != "" {
		fmt.Fprintf(&b, "author: %s\n", man.Author)
	}
	if man.ChangeID != "" {
		fmt.Fprintf(&b, "change-id: %s\n", man.ChangeID)
	}
	if man.PatchID != "" {
		fmt.Fprintf(&b, "patch-id: %s\n", man.PatchID)
	}
	b.WriteString("-->")
	return b.String()
}

// stripManifestBlock removes any prstack manifest from a PR body, returning
// the human-visible remainder.
func stripManifestBlock(body string) string {
	return strings.TrimSpace(manifestBlockRe.ReplaceAllString(body, ""))
}

// joinBodyManifest appends a manifest comment to a PR body.
func joinBodyManifest(body, man string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return man
	}
	return body + "\n\n" + man
}

// ── PR lookup ─────────────────────────────────────────────────────────────────

type prJSON struct {
	Number      int    `json:"number"`
	URL         string `json:"url"`
	State       string `json:"state"`
	Title       string `json:"title"`
	BaseRefName string `json:"baseRefName"`
	IsDraft     bool   `json:"isDraft"`
	HeadRefName string `json:"headRefName"`
	Body        string `json:"body"`
}

// prRecord pairs a PR with its parsed manifest (nil if absent/unparseable).
type prRecord struct {
	PR  prJSON
	Man *Manifest
}

// collectPRs lists every PR whose head branch is part of this stack
// (prefix-*) and returns them keyed by head branch.  One gh call instead
// of one per branch.
func collectPRs(prefix string) map[string]prRecord {
	out := ghOut("pr", "list",
		"--state", "all",
		"--limit", "500",
		"--json", "number,url,state,title,baseRefName,isDraft,headRefName,body",
	)
	var prs []prJSON
	if err := json.Unmarshal([]byte(out), &prs); err != nil {
		die("cannot parse `gh pr list` output: %v", err)
	}
	result := make(map[string]prRecord)
	for _, pr := range prs {
		if !strings.HasPrefix(pr.HeadRefName, prefix+"-") {
			continue
		}
		pr.State = strings.ToUpper(pr.State)
		if pr.IsDraft && pr.State == "OPEN" {
			pr.State = "DRAFT"
		}
		man, _ := parseManifest(pr.Body)
		result[pr.HeadRefName] = prRecord{PR: pr, Man: man}
	}
	return result
}

func sortedBranches(prs map[string]prRecord) []string {
	keys := make([]string, 0, len(prs))
	for k := range prs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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

// ── stack builder ─────────────────────────────────────────────────────────────

// buildStack reconciles the local commits against the PRs/manifests that
// already exist for this stack prefix, then chains each entry's base to the
// branch below it.
//
// Matching precedence (order-independent, so reordering the stack is free):
//  1. manifest change-id == commit Change-Id trailer    (exact)
//  2. manifest patch-id == commit patch-id             (exact)
//  3. legacy branch name derived from the commit        (exact — adopts
//     pre-manifest stacks, incl. Change-Id- and sha-named branches)
//  4. manifest title == commit title (+ author)        (heuristic —
//     catches amends, which change the patch-id; flagged for review)
//
// Anything unmatched mints a fresh branch and gets a new PR on sync;
// leftover PRs are reported as orphans.
func buildStack(commits []Commit, prefix, base string) *Stack {
	prs := collectPRs(prefix)
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
				branch, reason = br, "change-id"
				entry.PRNumber = rec.PR.Number
				entry.PRURL = rec.PR.URL
				entry.PRState = rec.PR.State
				entry.PRTitle = rec.PR.Title
				entry.PRBase = rec.PR.BaseRefName
			}
		}

		// 2. exact patch-id match (clean rebase / reorder).
		if branch == "" && commit.PatchID != "" {
			if br, rec, ok := findManifest(prs, used, func(m *Manifest) bool {
				return m.PatchID == commit.PatchID
			}); ok {
				branch, reason = br, "patch-id"
				entry.PRNumber = rec.PR.Number
				entry.PRURL = rec.PR.URL
				entry.PRState = rec.PR.State
				entry.PRTitle = rec.PR.Title
				entry.PRBase = rec.PR.BaseRefName
			}
		}

		// 3. legacy branch-name match: PRs created before manifests existed
		// are named after the commit's Change-Id/short SHA.
		if branch == "" {
			br := branchName(prefix, commit)
			if rec, ok := prs[br]; ok && !used[br] {
				branch, reason = br, "branch-name"
				entry.PRNumber = rec.PR.Number
				entry.PRURL = rec.PR.URL
				entry.PRState = rec.PR.State
				entry.PRTitle = rec.PR.Title
				entry.PRBase = rec.PR.BaseRefName
			}
		}

		// 4. title+author heuristic (amends change the patch-id but almost
		// always keep the title).  Flagged so the user verifies.
		if branch == "" {
			if br, rec, ok := findManifest(prs, used, func(m *Manifest) bool {
				return m.Title == commit.Title &&
					(m.Author == "" || m.Author == commit.Author)
			}); ok {
				branch, reason = br, "title"
				entry.Flagged = true
				entry.PRNumber = rec.PR.Number
				entry.PRURL = rec.PR.URL
				entry.PRState = rec.PR.State
				entry.PRTitle = rec.PR.Title
				entry.PRBase = rec.PR.BaseRefName
			}
		}

		if branch == "" {
			branch = branchName(prefix, commit)
			reason = "new"
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
		orphans = append(orphans, Orphan{Branch: br, PR: prs[br].PR})
	}

	return &Stack{Prefix: prefix, Base: base, Entries: entries, Orphans: orphans}
}

// ── PR body formatting ────────────────────────────────────────────────────────

var changeIDLineRe = regexp.MustCompile(`(?i)^Change-Id:\s*I[0-9a-f]{40}\s*$`)

// formatPRBody returns the commit body with the Change-Id trailer stripped
// (trailers never belong in the PR body).
func formatPRBody(entry StackEntry) string {
	var lines []string
	for _, ln := range strings.Split(entry.Commit.Body, "\n") {
		if !changeIDLineRe.MatchString(ln) {
			lines = append(lines, ln)
		}
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// manifestFor builds the manifest to persist for a stack entry.
func manifestFor(entry StackEntry, prefix string) Manifest {
	return Manifest{
		Stack:    prefix,
		Branch:   entry.Branch,
		PR:       entry.PRNumber,
		Slot:     entry.Idx,
		Title:    entry.Commit.Title,
		Author:   entry.Commit.Author,
		ChangeID: entry.Commit.ChangeID,
		PatchID:  entry.Commit.PatchID,
	}
}

// prBodyFor renders the full PR body: visible commit body + hidden manifest.
func prBodyFor(entry StackEntry, prefix string) string {
	return joinBodyManifest(formatPRBody(entry), manifestFor(entry, prefix).serialize())
}

// ── status command ────────────────────────────────────────────────────────────

var stateColour = map[string][]string{
	"OPEN":   {ansiGreen},
	"DRAFT":  {ansiYellow},
	"MERGED": {ansiBlue},
	"CLOSED": {ansiDim},
}

func fillPRFields(entry *StackEntry, rec prRecord) {
	entry.PRNumber = rec.PR.Number
	entry.PRURL = rec.PR.URL
	entry.PRState = rec.PR.State
	entry.PRTitle = rec.PR.Title
	entry.PRBase = rec.PR.BaseRefName
}

func cmdStatus(stack *Stack) {
	fmt.Println()
	fmt.Println(col("  working prefix : "+stack.Prefix, ansiDim))
	fmt.Println(col("  base           : "+stack.Base, ansiDim))
	fmt.Println()

	if len(stack.Entries) == 0 {
		fmt.Println(col("  (no commits above base)", ansiDim))
	}

	for _, entry := range stack.Entries {
		shaS := col(shortSHA(entry.Commit.SHA), ansiDim)
		idxS := col(fmt.Sprintf("[%d]", entry.Idx), ansiBold)
		brS := col(entry.Branch, ansiCyan)

		if entry.PRNumber != 0 {
			codes := stateColour[entry.PRState]
			if codes == nil {
				codes = []string{}
			}
			prS := col(fmt.Sprintf("#%d", entry.PRNumber), append(append([]string{}, codes...), ansiBold)...)
			stateS := col("("+entry.PRState+")", codes...)

			baseNote := ""
			if entry.PRBase != "" && entry.PRBase != entry.BaseBranch {
				baseNote = col(
					fmt.Sprintf("  ⚠ base: GitHub=%s → want %s", entry.PRBase, entry.BaseBranch),
					ansiYellow,
				)
			} else {
				baseNote = col("  ✓ base: "+entry.BaseBranch, ansiDim)
			}

			titleNote := ""
			if entry.PRTitle != entry.Commit.Title {
				titleNote = col("  ⚠ title differs", ansiYellow)
			}

			fmt.Printf("  %s %s  %s\n", idxS, shaS, entry.Commit.Title)
			fmt.Printf("       %s  →  %s %s%s%s\n", brS, prS, stateS, baseNote, titleNote)
		} else {
			fmt.Printf("  %s %s  %s\n", idxS, shaS, entry.Commit.Title)
			fmt.Printf("       %s  %s\n", brS, col("(no PR)", ansiYellow))
		}
		fmt.Printf("       %s\n", col("match: "+entry.MatchReason, ansiDim))
		if entry.Flagged {
			fmt.Println(col("       ⚠ matched by title only — verify this is the right PR", ansiYellow))
		}
		fmt.Println()
	}

	if len(stack.Orphans) > 0 {
		fmt.Println(col("  orphaned branches (no matching commit):", ansiYellow))
		for _, o := range stack.Orphans {
			fmt.Printf("    %s → PR #%d (%s)%s\n",
				col(o.Branch, ansiCyan), o.PR.Number, o.PR.State,
				col("  — prstack clean to remove, or gh pr close", ansiDim))
		}
		fmt.Println()
	}
}

// ── sync command ──────────────────────────────────────────────────────────────

func pushBranch(remote, branch, sha string, dryRun bool) error {
	if !dryRun {
		if localBranchExists(branch) {
			r := git("branch", "-f", branch, sha)
			if r.Code != 0 {
				return fmt.Errorf("branch -f: %s", r.Stderr)
			}
		} else {
			r := git("branch", branch, sha)
			if r.Code != 0 {
				return fmt.Errorf("branch: %s", r.Stderr)
			}
		}
		// Try force-with-lease first; fall back to --force for brand-new branches
		// where there's no remote tracking ref yet.
		refspec := branch + ":" + branch
		r := git("push", remote, refspec, "--force-with-lease")
		if r.Code != 0 {
			r = git("push", remote, refspec, "--force")
			if r.Code != 0 {
				return fmt.Errorf("push: %s", r.Stderr)
			}
		}
	}
	return nil
}

var prURLRe = regexp.MustCompile(`/pull/(\d+)`)

func ghCreatePR(branch, base, title, body string, draft bool) (int, string, error) {
	args := []string{"gh", "pr", "create",
		"--head", branch,
		"--base", base,
		"--title", title,
		"--body-file", "-",
	}
	if draft {
		args = append(args, "--draft")
	}
	r := runCmd(body, args...)
	if r.Code != 0 {
		return 0, "", fmt.Errorf("%s", r.Stderr)
	}
	url := strings.TrimSpace(r.Stdout)
	m := prURLRe.FindStringSubmatch(url)
	num := 0
	if m != nil {
		num, _ = strconv.Atoi(m[1])
	}
	return num, url, nil
}

func ghEditPR(number int, title, body, newBase string) error {
	args := []string{"gh", "pr", "edit", strconv.Itoa(number),
		"--title", title,
		"--body-file", "-",
	}
	if newBase != "" {
		args = append(args, "--base", newBase)
	}
	r := runCmd(body, args...)
	if r.Code != 0 {
		return fmt.Errorf("%s", r.Stderr)
	}
	return nil
}

func cmdSync(stack *Stack, remote string, draft, dryRun, verbose bool) {
	if len(stack.Entries) == 0 {
		fmt.Println("Nothing to sync — no commits above base.")
		return
	}

	dryTag := ""
	if dryRun {
		dryTag = col("[dry-run] ", ansiYellow)
	}

	// ── Pass 1: push branches + create/update PRs ─────────────────────────────
	for i := range stack.Entries {
		entry := &stack.Entries[i]
		fmt.Printf("\n%s %s  %s\n",
			col(fmt.Sprintf("[%d/%d]", entry.Idx, len(stack.Entries)), ansiBold),
			entry.Commit.Title,
			col("["+entry.MatchReason+"]", ansiDim),
		)

		if entry.Flagged {
			fmt.Println(col("  ⚠  matched by title only — verify this is the right PR", ansiYellow))
		}

		// Push
		action := "create"
		if localBranchExists(entry.Branch) {
			action = "update"
		}
		if verbose || dryRun {
			fmt.Printf("  %sbranch %s (%s) → push %s\n",
				dryTag, col(entry.Branch, ansiCyan), action, remote)
		}
		if err := pushBranch(remote, entry.Branch, entry.Commit.SHA, dryRun); err != nil {
			fmt.Println(col("  ✗ push failed: "+err.Error(), ansiRed))
			continue
		}
		if !dryRun {
			fmt.Printf("  ✓ pushed %s\n", col(entry.Branch, ansiCyan))
		}

		body := prBodyFor(*entry, stack.Prefix)

		if entry.PRNumber != 0 {
			// Update existing PR (title, body+manifest, base if changed).
			baseChanged := entry.PRBase != "" && entry.PRBase != entry.BaseBranch
			newBase := ""
			if baseChanged {
				newBase = entry.BaseBranch
			}

			updates := []string{"title", "body"}
			if baseChanged {
				updates = append(updates, fmt.Sprintf("base → %s", entry.BaseBranch))
			}

			if !dryRun {
				if err := ghEditPR(entry.PRNumber, entry.Commit.Title, body, newBase); err != nil {
					fmt.Println(col("  ✗ gh pr edit: "+err.Error(), ansiRed))
					continue
				}
			}
			fmt.Printf("  %s✓ PR #%d updated (%s)\n", dryTag, entry.PRNumber, strings.Join(updates, ", "))
			if entry.PRURL != "" {
				fmt.Println("    " + entry.PRURL)
			}
		} else {
			// Create new PR — body already carries the manifest.
			if dryRun {
				fmt.Printf("  %swould create PR: head=%s base=%s\n",
					dryTag, entry.Branch, entry.BaseBranch)
				continue
			}
			num, url, err := ghCreatePR(entry.Branch, entry.BaseBranch,
				entry.Commit.Title, body, draft)
			if err != nil {
				fmt.Println(col("  ✗ gh pr create: "+err.Error(), ansiRed))
				continue
			}
			entry.PRNumber = num
			entry.PRURL = url
			entry.PRState = "OPEN"
			if draft {
				entry.PRState = "DRAFT"
			}
			draftNote := ""
			if draft {
				draftNote = " (draft)"
			}
			fmt.Printf("  ✓ PR #%d created%s\n    %s\n", num, draftNote, url)
		}
	}

	if !dryRun {
		for _, o := range stack.Orphans {
			fmt.Println(col(
				fmt.Sprintf("⚠ orphaned: %s → PR #%d (%s) — no matching commit", o.Branch, o.PR.Number, o.PR.State),
				ansiYellow))
		}
	}

	fmt.Println(col("\n✓ Done.", ansiGreen+ansiBold))
}

// ── checkout command ──────────────────────────────────────────────────────────

func cmdCheckout(stack *Stack, n int) {
	entry := resolveIndex(stack.Entries, n)
	if !localBranchExists(entry.Branch) {
		r := git("branch", entry.Branch, entry.Commit.SHA)
		if r.Code != 0 {
			die("git branch: %s", r.Stderr)
		}
		fmt.Println("created local branch", entry.Branch)
	}
	passthrough("git", "checkout", entry.Branch)
}

// ── open command ──────────────────────────────────────────────────────────────

func cmdOpen(stack *Stack, n int) {
	entry := resolveIndex(stack.Entries, n)
	if entry.PRNumber == 0 {
		die("no PR for entry %d — run `prstack sync` first", n)
	}
	passthrough("gh", "pr", "view", strconv.Itoa(entry.PRNumber), "--web")
}

// ── link command ─────────────────────────────────────────────────────────────

// cmdLink manually binds commit N to an existing stack branch/PR and
// persists the binding by writing the commit's hints into that PR's
// manifest, so future syncs match it exactly.
func cmdLink(stack *Stack, n int, branch string) {
	entry := resolveIndex(stack.Entries, n)
	if branch == entry.Branch {
		fmt.Println("already linked")
		return
	}
	for _, e := range stack.Entries {
		if e.Idx != entry.Idx && e.Branch == branch {
			die("branch %s is already matched to entry %d — link that entry elsewhere first", branch, e.Idx)
		}
	}
	prs := collectPRs(stack.Prefix)
	rec, ok := prs[branch]
	if !ok || rec.PR.Number == 0 {
		die("no PR found for branch %s — run `prstack sync` first, or create the PR", branch)
	}
	if rec.PR.State == "MERGED" || rec.PR.State == "CLOSED" {
		die("PR #%d is %s — cannot link to it", rec.PR.Number, rec.PR.State)
	}

	man := Manifest{
		Stack:    stack.Prefix,
		Branch:   branch,
		PR:       rec.PR.Number,
		Slot:     entry.Idx,
		Title:    entry.Commit.Title,
		Author:   entry.Commit.Author,
		ChangeID: entry.Commit.ChangeID,
		PatchID:  entry.Commit.PatchID,
	}
	body := joinBodyManifest(stripManifestBlock(rec.PR.Body), man.serialize())
	if err := ghEditPR(rec.PR.Number, entry.Commit.Title, body, ""); err != nil {
		die("gh pr edit: %s", err)
	}
	fmt.Printf("✓ linked [%d] %q → %s (PR #%d)\n", entry.Idx, entry.Commit.Title, branch, rec.PR.Number)
	fmt.Println("  binding persisted in the PR manifest; run `prstack sync` to push the commit")
}

// ── clean command ─────────────────────────────────────────────────────────────

func deleteStackBranch(remote, branch string, dryRun bool) {
	if !dryRun {
		if localBranchExists(branch) {
			git("branch", "-D", branch)
		}
		if remoteBranchExists(remote, branch) {
			git("push", remote, "--delete", branch)
		}
		fmt.Println("    deleted local + remote")
	} else {
		fmt.Println(col("    [dry-run] would delete local + remote", ansiYellow))
	}
}

func cmdClean(stack *Stack, remote string, dryRun bool) {
	removed := 0
	remove := func(branch string, ref string) {
		fmt.Printf("  %s [%s]\n", branch, ref)
		deleteStackBranch(remote, branch, dryRun)
		removed++
	}

	// Current entries whose PRs were merged/closed.
	for _, entry := range stack.Entries {
		if entry.PRState != "MERGED" && entry.PRState != "CLOSED" {
			continue
		}
		remove(entry.Branch, fmt.Sprintf("#%d %s", entry.PRNumber, entry.PRState))
	}

	// Orphaned branches (commit gone from the stack entirely).
	for _, o := range stack.Orphans {
		if o.PR.State == "MERGED" || o.PR.State == "CLOSED" {
			remove(o.Branch, fmt.Sprintf("#%d %s (orphaned)", o.PR.Number, o.PR.State))
		} else {
			fmt.Printf("  %s → PR #%d (%s) — open but orphaned; close it or `prstack link` it\n",
				col(o.Branch, ansiCyan), o.PR.Number, o.PR.State)
		}
	}

	if removed == 0 {
		fmt.Println("Nothing to clean.")
	}
}

// ── utilities ─────────────────────────────────────────────────────────────────

func resolveIndex(entries []StackEntry, n int) StackEntry {
	if n < 1 || n > len(entries) {
		die("index %d out of range (stack has %d entries)", n, len(entries))
	}
	return entries[n-1]
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, col("error: ", ansiRed)+format+"\n", args...)
	os.Exit(1)
}

// ── main ──────────────────────────────────────────────────────────────────────

const usage = `usage: prstack [flags] <command> [args]

commands:
  status              show stack state
  sync                push branches and create/update PRs
  checkout N          switch to branch for commit N (1-indexed)
  open N              open PR N in browser
  link N BRANCH       manually bind commit N to an existing branch/PR
  clean               delete branches for merged/closed PRs

flags:
  --base BRANCH       upstream base branch (default: main)
  --remote NAME      git remote (default: origin)
  --prefix PFX        branch prefix (default: current branch)
  --draft             new PRs as draft [default]
  --no-draft          new PRs as ready
  --dry-run           show actions without making changes
  -v                  verbose output

commits are matched to PRs across rebases by (in order):
  1. Change-Id trailer (exact)
  2. patch-id          (exact; survives clean rebase + reorder)
  3. legacy branch name (exact; pre-manifest stacks)
  4. title + author    (heuristic; catches amends, flagged)
the mapping is persisted as a hidden manifest comment in each PR body.
`

func main() {
	fs := flag.NewFlagSet("prstack", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	baseFlag := fs.String("base", "", "upstream base branch")
	remoteFlag := fs.String("remote", "", "git remote")
	prefixFlag := fs.String("prefix", "", "branch prefix override")
	draftFlag := fs.Bool("draft", true, "create new PRs as draft")
	noDraft := fs.Bool("no-draft", false, "create new PRs as ready")
	dryRun := fs.Bool("dry-run", false, "print actions, no changes")
	verbose := fs.Bool("v", false, "verbose output")

	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}

	args := fs.Args()
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	command := args[0]
	cmdArgs := args[1:]

	// Accept flags after the command too: `prstack sync --dry-run`.
	// flag.Parse stops at the first positional, so re-parse the remainder
	// and drop any flag-looking tokens from the positional args.
	if err := fs.Parse(cmdArgs); err == nil {
		cmdArgs = fs.Args()
		filtered := cmdArgs[:0]
		for _, a := range cmdArgs {
			if strings.HasPrefix(a, "-") {
				continue
			}
			filtered = append(filtered, a)
		}
		cmdArgs = filtered
	}

	// Resolve git context
	branch := currentBranch()
	if branch == "HEAD" || branch == "main" || branch == "master" || branch == "trunk" {
		die("you are on '%s'; checkout a feature branch first", branch)
	}

	// Config priority: flag > git config > default
	resolve := func(flag, configKey, def string) string {
		if flag != "" {
			return flag
		}
		if v := gitConfig(configKey); v != "" {
			return v
		}
		return def
	}

	remote := resolve(*remoteFlag, "remote", detectRemote())
	base := resolve(*baseFlag, "base", "main")
	prefix := resolve(*prefixFlag, "prefix", branch)

	// Draft: --no-draft wins over --draft; then git config; then default true.
	draft := *draftFlag
	if *noDraft {
		draft = false
	}
	if *draftFlag == true && !*noDraft { // not explicitly set by flags
		if v := gitConfig("draft"); v != "" {
			draft = v != "false" && v != "0" && v != "no"
		}
	}

	if *verbose {
		fmt.Println(col(
			fmt.Sprintf("remote=%s  base=%s  prefix=%s  draft=%v", remote, base, prefix, draft),
			ansiDim,
		))
	}

	// Build stack
	fork := findForkSHA(remote, base)
	commits := commitsSince(fork)

	if *verbose {
		fmt.Println(col(fmt.Sprintf("fork sha: %s  commits: %d", shortSHA(fork), len(commits)), ansiDim))
	}

	stack := buildStack(commits, prefix, base)

	// Dispatch
	switch command {
	case "status":
		cmdStatus(stack)

	case "sync":
		cmdSync(stack, remote, draft, *dryRun, *verbose)

	case "checkout":
		if len(cmdArgs) == 0 {
			die("checkout requires an index: prstack checkout N")
		}
		n, err := strconv.Atoi(cmdArgs[0])
		if err != nil {
			die("invalid index: %s", cmdArgs[0])
		}
		cmdCheckout(stack, n)

	case "open":
		if len(cmdArgs) == 0 {
			die("open requires an index: prstack open N")
		}
		n, err := strconv.Atoi(cmdArgs[0])
		if err != nil {
			die("invalid index: %s", cmdArgs[0])
		}
		cmdOpen(stack, n)

	case "link":
		if len(cmdArgs) < 2 {
			die("link requires an index and a branch: prstack link N BRANCH")
		}
		n, err := strconv.Atoi(cmdArgs[0])
		if err != nil {
			die("invalid index: %s", cmdArgs[0])
		}
		cmdLink(stack, n, cmdArgs[1])

	case "clean":
		cmdClean(stack, remote, *dryRun)

	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n%s", command, usage)
		os.Exit(2)
	}
}
