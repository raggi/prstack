package prstack

import (
	"fmt"
	"strconv"
)

// ── checkout ──────────────────────────────────────────────────────────────────

// Checkout creates entry n's local branch if needed (at the entry's
// commit) and runs `git checkout` on it, inheriting the terminal.
func (s *Stack) Checkout(n int) error {
	entry, err := s.Entry(n)
	if err != nil {
		return err
	}
	if !localBranchExists(entry.Branch) {
		if _, err := gitOutErr("branch", entry.Branch, entry.Commit.SHA); err != nil {
			return fmt.Errorf("git branch: %w", err)
		}
	}
	return passthrough("git", "checkout", entry.Branch)
}

// ── open ──────────────────────────────────────────────────────────────────────

// Open opens entry n's PR in a browser via `gh pr view --web`, inheriting
// the terminal.
func (s *Stack) Open(n int) error {
	entry, err := s.Entry(n)
	if err != nil {
		return err
	}
	if entry.PR.Number == 0 {
		return fmt.Errorf("no PR for entry %d — run Sync first", n)
	}
	return passthrough("gh", "pr", "view", strconv.Itoa(entry.PR.Number), "--web")
}

// ── link ──────────────────────────────────────────────────────────────────────

// Link manually binds entry n to an existing stack branch/PR and persists
// the binding by writing the entry's hints (change-id, patch-id, title,
// author) into that PR's manifest, so future Loads match it exactly.
//
// Use it when reconciliation guesses wrong — e.g. after a commit was
// rewritten so heavily that neither patch-id nor title matches — or to
// adopt an orphaned PR.
func (s *Stack) Link(n int, branch string) error {
	entry, err := s.Entry(n)
	if err != nil {
		return err
	}
	if branch == entry.Branch {
		return fmt.Errorf("entry %d is already linked to %s", n, branch)
	}
	for _, e := range s.Entries {
		if e.Idx != entry.Idx && e.Branch == branch {
			return fmt.Errorf("branch %s is already matched to entry %d — link that entry elsewhere first", branch, e.Idx)
		}
	}
	prs, err := collectPRs(s.Prefix)
	if err != nil {
		return err
	}
	rec, ok := prs[branch]
	if !ok || rec.PR.Number == 0 {
		return fmt.Errorf("no PR found for branch %s — run Sync first, or create the PR", branch)
	}
	if rec.PR.State == PRStateMerged || rec.PR.State == PRStateClosed {
		return fmt.Errorf("PR #%d is %s — cannot link to it", rec.PR.Number, rec.PR.State)
	}

	man := Manifest{
		Stack:    s.Prefix,
		Branch:   branch,
		PR:       rec.PR.Number,
		Slot:     entry.Idx,
		Title:    entry.Commit.Title,
		Author:   entry.Commit.Author,
		ChangeID: entry.Commit.ChangeID,
		PatchID:  entry.Commit.PatchID,
	}
	body := joinBodyManifest(StripManifest(rec.PR.Body), man.Serialize())
	if err := ghEditPR(rec.PR.Number, entry.Commit.Title, body, ""); err != nil {
		return fmt.Errorf("gh pr edit: %w", err)
	}

	// Rebind the entry in place so callers see consistent state.
	entry.Branch = branch
	entry.PR = prInfoFromJSON(rec.PR)
	entry.MatchReason = "manual"
	return nil
}

// ── clean ─────────────────────────────────────────────────────────────────────

// CleanOptions controls a Clean run.
type CleanOptions struct {
	// DryRun plans deletions but executes none.
	DryRun bool
	// Log, if set, receives streaming progress lines.
	Log func(format string, args ...any)
}

func (o CleanOptions) logf(format string, args ...any) {
	if o.Log != nil {
		o.Log(format, args...)
	}
}

// CleanRemoval records one branch deletion attempted by Clean.
type CleanRemoval struct {
	Branch string
	Ref    string // e.g. "#12 MERGED (orphaned)"
	Err    error  // deletion failure, if any
}

// CleanReport is the outcome of a Clean run.
type CleanReport struct {
	// Removed lists every branch deletion attempted (see Err per entry).
	Removed []CleanRemoval
	// OpenOrphans are orphaned branches whose PRs are still open; Clean
	// leaves them alone.  Close them, or Link them if they were mis-matched.
	OpenOrphans []Orphan
	// DryRun is true when no actions were executed.
	DryRun bool
}

// Clean deletes the branches of merged/closed stack PRs, both locally and
// on the remote: current entries whose PRs were merged/closed, and
// orphaned branches (commit gone from the stack).  Orphaned branches whose
// PRs are still open are reported, not deleted.
func (s *Stack) Clean(o CleanOptions) (*CleanReport, error) {
	report := &CleanReport{DryRun: o.DryRun}
	remove := func(branch, ref string) {
		o.logf("  %s [%s]", branch, ref)
		var err error
		if !o.DryRun {
			err = deleteStackBranch(s.Remote, branch)
		}
		report.Removed = append(report.Removed, CleanRemoval{Branch: branch, Ref: ref, Err: err})
	}

	// Current entries whose PRs were merged/closed.
	for _, entry := range s.Entries {
		if entry.PR.State != PRStateMerged && entry.PR.State != PRStateClosed {
			continue
		}
		remove(entry.Branch, fmt.Sprintf("#%d %s", entry.PR.Number, entry.PR.State))
	}

	// Orphaned branches (commit gone from the stack entirely).
	for _, orph := range s.Orphans {
		if orph.PR.State == PRStateMerged || orph.PR.State == PRStateClosed {
			remove(orph.Branch, fmt.Sprintf("#%d %s (orphaned)", orph.PR.Number, orph.PR.State))
		} else {
			report.OpenOrphans = append(report.OpenOrphans, orph)
		}
	}
	return report, nil
}

func deleteStackBranch(remote, branch string) error {
	if localBranchExists(branch) {
		if r := git("branch", "-D", branch); r.Code != 0 {
			return fmt.Errorf("git branch -D: %s", r.Stderr)
		}
	}
	if remoteBranchExists(remote, branch) {
		if r := git("push", remote, "--delete", branch); r.Code != 0 {
			return fmt.Errorf("git push --delete: %s", r.Stderr)
		}
	}
	return nil
}
