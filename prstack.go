// prstack — manage a stack of GitHub pull requests.
//
// Each commit in your local branch (rebased on origin/main) becomes its own
// PR.  Commits are tracked across rebases via Gerrit-style Change-Id trailers.
//
// Usage:
//
//	prstack status              show stack state
//	prstack sync [--dry-run]    push branches and create/update PRs
//	prstack checkout N          switch to branch for commit N (1-indexed)
//	prstack open N              open PR N in browser
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
//	stack branches:  raggi/eventbus-foo-<8 hex chars of Change-Id>
//
// The prefix is the full working branch name.  Each stack branch appends
// a short Change-Id suffix, e.g. raggi/eventbus-foo-a1b2c3d4.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// ── ANSI colours ──────────────────────────────────────────────────────────────

var isTTY = func() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}()

const (
	ansiReset = "\033[0m"
	ansiBold  = "\033[1m"
	ansiDim   = "\033[2m"
	ansiRed   = "\033[31m"
	ansiGreen = "\033[32m"
	ansiYellow = "\033[33m"
	ansiBlue  = "\033[34m"
	ansiCyan  = "\033[36m"
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

// mustRunStdin runs a command with stdin and exits on failure.
func mustRunStdin(stdin string, args ...string) string {
	r := runCmd(stdin, args...)
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
	ChangeID string // full "I<40hex>" value, or "" if absent
}

// StackEntry is one slot in the stack — one commit, one branch, one PR.
type StackEntry struct {
	Idx        int    // 1-indexed (1 = oldest, closest to base)
	Commit     Commit
	Branch     string // local/remote branch for this commit
	BaseBranch string // PR target on GitHub
	PRNumber   int    // 0 if no PR found
	PRURL      string
	PRState    string // OPEN, DRAFT, MERGED, CLOSED
	PRTitle    string
	PRBase     string // base as currently recorded on GitHub
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

func commitsSince(forkSHA string) []Commit {
	log := gitOut("log", "--format=%H", forkSHA+"..HEAD")
	if log == "" {
		return nil
	}
	shas := strings.Split(log, "\n")
	// reverse so index 0 is oldest (closest to base)
	for i, j := 0, len(shas)-1; i < j; i, j = i+1, j-1 {
		shas[i], shas[j] = shas[j], shas[i]
	}
	commits := make([]Commit, 0, len(shas))
	for _, sha := range shas {
		if sha == "" {
			continue
		}
		msg := gitOut("log", "-1", "--format=%B", sha)
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
			ChangeID: extractChangeID(msg),
		})
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

// branchName returns the stack branch name for a commit.
// Uses the full working branch as prefix, appends 8 hex chars of the Change-Id.
// Falls back to short SHA when no Change-Id is present.
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

// ── PR lookup ─────────────────────────────────────────────────────────────────

type prJSON struct {
	Number      int    `json:"number"`
	URL         string `json:"url"`
	State       string `json:"state"`
	Title       string `json:"title"`
	BaseRefName string `json:"baseRefName"`
	IsDraft     bool   `json:"isDraft"`
}

// fetchPRInfo queries GitHub for PR state for each branch in the list.
// Returns a map from branch name to PR metadata.
func fetchPRInfo(branches []string) map[string]prJSON {
	result := make(map[string]prJSON, len(branches))
	for _, branch := range branches {
		r := runCmd("", "gh", "pr", "list",
			"--head", branch,
			"--state", "all",
			"--json", "number,url,state,title,baseRefName,isDraft",
		)
		if r.Code != 0 || strings.TrimSpace(r.Stdout) == "" {
			continue
		}
		var prs []prJSON
		if err := json.Unmarshal([]byte(r.Stdout), &prs); err != nil || len(prs) == 0 {
			continue
		}
		pr := prs[0]
		if pr.IsDraft {
			pr.State = "DRAFT"
		} else {
			pr.State = strings.ToUpper(pr.State)
		}
		result[branch] = pr
	}
	return result
}

// ── stack builder ─────────────────────────────────────────────────────────────

func buildStack(commits []Commit, prefix, baseBranch string) []StackEntry {
	entries := make([]StackEntry, len(commits))
	branches := make([]string, len(commits))

	for i, commit := range commits {
		br := branchName(prefix, commit)
		base := baseBranch
		if i > 0 {
			base = entries[i-1].Branch
		}
		entries[i] = StackEntry{
			Idx:        i + 1,
			Commit:     commit,
			Branch:     br,
			BaseBranch: base,
		}
		branches[i] = br
	}

	prInfo := fetchPRInfo(branches)
	for i, entry := range entries {
		info, ok := prInfo[entry.Branch]
		if !ok {
			continue
		}
		entries[i].PRNumber = info.Number
		entries[i].PRURL    = info.URL
		entries[i].PRState  = info.State
		entries[i].PRTitle  = info.Title
		entries[i].PRBase   = info.BaseRefName
	}
	return entries
}

// ── PR body formatting ────────────────────────────────────────────────────────

var changeIDLineRe = regexp.MustCompile(`(?i)^Change-Id:\s*I[0-9a-f]{40}\s*$`)

// formatPRBody returns the commit body with the Change-Id trailer stripped.
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

// ── status command ────────────────────────────────────────────────────────────

var stateColour = map[string][]string{
	"OPEN":   {ansiGreen},
	"DRAFT":  {ansiYellow},
	"MERGED": {ansiBlue},
	"CLOSED": {ansiDim},
}

func cmdStatus(stack []StackEntry, prefix, base string) {
	fmt.Println()
	fmt.Println(col("  working prefix : "+prefix, ansiDim))
	fmt.Println(col("  base           : "+base, ansiDim))
	fmt.Println()

	if len(stack) == 0 {
		fmt.Println(col("  (no commits above base)", ansiDim))
		return
	}

	for _, entry := range stack {
		shaS  := col(shortSHA(entry.Commit.SHA), ansiDim)
		idxS  := col(fmt.Sprintf("[%d]", entry.Idx), ansiBold)
		brS   := col(entry.Branch, ansiCyan)

		cidWarn := ""
		if entry.Commit.ChangeID == "" {
			cidWarn = col(" ⚠ no Change-Id", ansiYellow)
		}

		if entry.PRNumber != 0 {
			codes := stateColour[entry.PRState]
			if codes == nil {
				codes = []string{}
			}
			prS     := col(fmt.Sprintf("#%d", entry.PRNumber), append(codes, ansiBold)...)
			stateS  := col("("+entry.PRState+")", codes...)

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
			fmt.Printf("       %s  %s%s\n", brS, col("(no PR)", ansiYellow), cidWarn)
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

func cmdSync(stack []StackEntry, remote string, draft, dryRun, verbose bool) {
	if len(stack) == 0 {
		fmt.Println("Nothing to sync — no commits above base.")
		return
	}

	dryTag := ""
	if dryRun {
		dryTag = col("[dry-run] ", ansiYellow)
	}

	// ── Pass 1: push branches + create/update PRs ─────────────────────────────
	for i := range stack {
		entry := &stack[i]
		fmt.Printf("\n%s %s\n",
			col(fmt.Sprintf("[%d/%d]", entry.Idx, len(stack)), ansiBold),
			entry.Commit.Title,
		)

		if entry.Commit.ChangeID == "" {
			fmt.Println(col("  ⚠  No Change-Id — branch uses SHA; will break on rebase", ansiYellow))
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

		body := formatPRBody(*entry)

		if entry.PRNumber != 0 {
			// Update existing PR
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
			// Create new PR
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
			entry.PRURL    = url
			entry.PRState  = "OPEN"
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

	fmt.Println(col("\n✓ Done.", ansiGreen+ansiBold))
}

// ── checkout command ──────────────────────────────────────────────────────────

func cmdCheckout(stack []StackEntry, n int) {
	entry := resolveIndex(stack, n)
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

func cmdOpen(stack []StackEntry, n int) {
	entry := resolveIndex(stack, n)
	if entry.PRNumber == 0 {
		die("no PR for entry %d — run `prstack sync` first", n)
	}
	passthrough("gh", "pr", "view", strconv.Itoa(entry.PRNumber), "--web")
}

// ── clean command ─────────────────────────────────────────────────────────────

func cmdClean(stack []StackEntry, remote string, dryRun bool) {
	removed := 0
	for _, entry := range stack {
		if entry.PRState != "MERGED" && entry.PRState != "CLOSED" {
			continue
		}
		ref := "(no PR)"
		if entry.PRNumber != 0 {
			ref = fmt.Sprintf("#%d", entry.PRNumber)
		}
		fmt.Printf("  %s [%s %s]\n", entry.Branch, ref, entry.PRState)
		if !dryRun {
			if localBranchExists(entry.Branch) {
				git("branch", "-D", entry.Branch)
			}
			if remoteBranchExists(remote, entry.Branch) {
				git("push", remote, "--delete", entry.Branch)
			}
			fmt.Println("    deleted local + remote")
		} else {
			fmt.Println(col("    [dry-run] would delete local + remote", ansiYellow))
		}
		removed++
	}
	if removed == 0 {
		fmt.Println("Nothing to clean.")
	}
}

// ── utilities ─────────────────────────────────────────────────────────────────

func resolveIndex(stack []StackEntry, n int) StackEntry {
	if n < 1 || n > len(stack) {
		die("index %d out of range (stack has %d entries)", n, len(stack))
	}
	return stack[n-1]
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
  clean               delete branches for merged/closed PRs

flags:
  --base BRANCH       upstream base branch (default: main)
  --remote NAME       git remote (default: origin)
  --prefix PFX        branch prefix (default: current branch)
  --draft             new PRs as draft [default]
  --no-draft          new PRs as ready
  --dry-run           show actions without making changes
  -v                  verbose output
`

func main() {
	fs := flag.NewFlagSet("prstack", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	baseFlag   := fs.String("base", "", "upstream base branch")
	remoteFlag := fs.String("remote", "", "git remote")
	prefixFlag := fs.String("prefix", "", "branch prefix override")
	draftFlag  := fs.Bool("draft", true, "create new PRs as draft")
	noDraft    := fs.Bool("no-draft", false, "create new PRs as ready")
	dryRun     := fs.Bool("dry-run", false, "print actions, no changes")
	verbose    := fs.Bool("v", false, "verbose output")

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
	base   := resolve(*baseFlag, "base", "main")
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
	fork    := findForkSHA(remote, base)
	commits := commitsSince(fork)

	if *verbose {
		fmt.Println(col(fmt.Sprintf("fork sha: %s  commits: %d", shortSHA(fork), len(commits)), ansiDim))
	}

	stack := buildStack(commits, prefix, base)

	// Dispatch
	switch command {
	case "status":
		cmdStatus(stack, prefix, base)

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

	case "clean":
		cmdClean(stack, remote, *dryRun)

	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n%s", command, usage)
		os.Exit(2)
	}
}
