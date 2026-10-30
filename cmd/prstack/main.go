// prstack — manage a stack of GitHub pull requests.
//
// Each commit in your local branch (rebased on origin/main) becomes its own
// PR.  Commits are tracked across rebases/amends/reorders WITHOUT requiring
// Gerrit-style Change-Id trailers; see the prstack package for the matching
// rules.  This binary is a thin convenience wrapper over the library.
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
// Flags work before or after the command.  Per-repo git config
// (`prstack.base`, `prstack.remote`, `prstack.prefix`, `prstack.draft`)
// is checked between flags and defaults.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/raggi/prstack"
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

var stateColour = map[string][]string{
	prstack.PRStateOpen:   {ansiGreen},
	prstack.PRStateDraft:  {ansiYellow},
	prstack.PRStateMerged: {ansiBlue},
	prstack.PRStateClosed: {ansiDim},
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, col("error: ", ansiRed)+format+"\n", args...)
	os.Exit(1)
}

// ── status rendering ─────────────────────────────────────────────────────────

func cmdStatus(stack *prstack.Stack) {
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

		if entry.PR.Number != 0 {
			codes := stateColour[entry.PR.State]
			if codes == nil {
				codes = []string{}
			}
			prS := col(fmt.Sprintf("#%d", entry.PR.Number), append(append([]string{}, codes...), ansiBold)...)
			stateS := col("("+entry.PR.State+")", codes...)

			baseNote := ""
			if entry.PR.Base != "" && entry.PR.Base != entry.BaseBranch {
				baseNote = col(
					fmt.Sprintf("  ⚠ base: GitHub=%s → want %s", entry.PR.Base, entry.BaseBranch),
					ansiYellow,
				)
			} else {
				baseNote = col("  ✓ base: "+entry.BaseBranch, ansiDim)
			}

			titleNote := ""
			if entry.PR.Title != entry.Commit.Title {
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
				col("  — prstack clean to remove, or prstack link", ansiDim))
		}
		fmt.Println()
	}
}

// ── sync / clean rendering ───────────────────────────────────────────────────

func logLine(format string, args ...any) {
	fmt.Println(fmt.Sprintf(format, args...))
}

func cmdSync(stack *prstack.Stack, dryRun bool) {
	report, err := stack.Sync(prstack.SyncOptions{DryRun: dryRun, Log: logLine})
	if err != nil {
		die("sync: %v", err)
	}
	for _, o := range report.Orphans {
		fmt.Println(col(
			fmt.Sprintf("⚠ orphaned: %s → PR #%d (%s) — no matching commit", o.Branch, o.PR.Number, o.PR.State),
			ansiYellow))
	}
	fmt.Println(col("\n✓ Done.", ansiGreen+ansiBold))
}

func cmdClean(stack *prstack.Stack, dryRun bool) {
	report, err := stack.Clean(prstack.CleanOptions{DryRun: dryRun, Log: logLine})
	if err != nil {
		die("clean: %v", err)
	}
	failed := false
	for _, r := range report.Removed {
		if r.Err != nil {
			fmt.Println(col(fmt.Sprintf("  ✗ %s: %v", r.Branch, r.Err), ansiRed))
			failed = true
		}
	}
	for _, o := range report.OpenOrphans {
		fmt.Printf("  %s → PR #%d (%s) — open but orphaned; close it or `prstack link` it\n",
			col(o.Branch, ansiCyan), o.PR.Number, o.PR.State)
	}
	if len(report.Removed) == 0 && len(report.OpenOrphans) == 0 {
		fmt.Println("Nothing to clean.")
	}
	if failed {
		os.Exit(1)
	}
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

	// Draft: --no-draft wins over --draft; git config applies unless a
	// flag was explicit.  (--draft is the default, so only --no-draft
	// is truly explicit.)
	var draft *bool
	if *noDraft {
		draft = new(bool) // false
	}

	opts := prstack.Options{
		Base:   *baseFlag,
		Remote: *remoteFlag,
		Prefix: *prefixFlag,
		Draft:  draft,
	}
	_ = *draftFlag // kept for usage; default handled by the library

	stack, err := prstack.Load(opts)
	if err != nil {
		die("%v", err)
	}

	if *verbose {
		fmt.Println(col(
			fmt.Sprintf("remote=%s  base=%s  prefix=%s  draft=%v",
				stack.Remote, stack.Base, stack.Prefix, stack.Draft),
			ansiDim,
		))
	}

	indexArg := func(name string) int {
		if len(cmdArgs) == 0 {
			die("%s requires an index: prstack %s N", name, name)
		}
		n, err := strconv.Atoi(cmdArgs[0])
		if err != nil {
			die("invalid index: %s", cmdArgs[0])
		}
		return n
	}

	switch command {
	case "status":
		cmdStatus(stack)

	case "sync":
		cmdSync(stack, *dryRun)

	case "checkout":
		n := indexArg("checkout")
		if err := stack.Checkout(n); err != nil {
			die("%v", err)
		}

	case "open":
		n := indexArg("open")
		if err := stack.Open(n); err != nil {
			die("%v", err)
		}

	case "link":
		if len(cmdArgs) < 2 {
			die("link requires an index and a branch: prstack link N BRANCH")
		}
		n, err := strconv.Atoi(cmdArgs[0])
		if err != nil {
			die("invalid index: %s", cmdArgs[0])
		}
		if err := stack.Link(n, cmdArgs[1]); err != nil {
			die("%v", err)
		}
		fmt.Printf("✓ linked [%d] → %s\n", n, cmdArgs[1])
		fmt.Println("  binding persisted in the PR manifest; run `prstack sync` to push the commit")

	case "clean":
		cmdClean(stack, *dryRun)

	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n%s", command, usage)
		os.Exit(2)
	}
}
