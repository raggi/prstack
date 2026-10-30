// Example: using prstack as a library in a workflow tool.
//
// Run from inside a git repository on a feature branch with `gh`
// authenticated; it prints the reconciled stack and dry-runs a sync.
package main

import (
	"fmt"
	"os"

	"github.com/raggi/prstack"
)

func main() {
	// Load resolves git config (prstack.base/remote/prefix/draft) and
	// reconciles local commits against the stack's PRs.
	stack, err := prstack.Load(prstack.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	fmt.Printf("stack %s (base %s, remote %s)\n", stack.Prefix, stack.Base, stack.Remote)
	for _, e := range stack.Entries {
		pr := "-"
		if e.PR.Number != 0 {
			pr = fmt.Sprintf("#%d %s", e.PR.Number, e.PR.State)
		}
		fmt.Printf("  [%d] %s\n       branch=%s match=%s flagged=%v pr=%s\n",
			e.Idx, e.Commit.Title, e.Branch, e.MatchReason, e.Flagged, pr)
		if e.Flagged {
			// Heuristic (title) match — ask the user, or inspect
			// e.PR / e.Commit before trusting it.
			fmt.Println("       ⚠ verify: matched by title only")
		}
	}
	for _, o := range stack.Orphans {
		fmt.Printf("  orphan: %s → #%d %s\n", o.Branch, o.PR.Number, o.PR.State)
	}

	// Plan a sync without executing anything.
	report, err := stack.Sync(prstack.SyncOptions{
		DryRun: true,
		Log:    func(format string, args ...any) { fmt.Printf(format+"\n", args...) },
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "sync error:", err)
		os.Exit(1)
	}
	for _, r := range report.Entries {
		fmt.Printf("  sync[%d]: action=%s pr=%d err=%v\n", r.Idx, r.Action, r.PRNumber, r.Err)
	}

	// Other entry points:
	//   stack.Link(n, branch)                 — manual rebinding, persisted in the PR manifest
	//   stack.Clean(prstack.CleanOptions{...}) — delete branches of merged/closed PRs
	//   stack.Checkout(n) / stack.Open(n)      — interactive commands
	//   prstack.ListPRs(prefix)                — just the remote stack state
	//   prstack.ParseManifest(prBody)          — manifest of any PR body
}
