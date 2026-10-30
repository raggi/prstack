package prstack

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ── sync ──────────────────────────────────────────────────────────────────────

// SyncOptions controls a Sync run.
type SyncOptions struct {
	// DryRun plans every action (push, PR create/edit) but executes none.
	DryRun bool
	// Log, if set, receives streaming progress lines while Sync runs
	// (one line per format call, no trailing newline).  Use it to render
	// progress as it happens; inspect the returned SyncReport afterwards
	// for structured results.
	Log func(format string, args ...any)
}

func (o SyncOptions) logf(format string, args ...any) {
	if o.Log != nil {
		o.Log(format, args...)
	}
}

// SyncAction describes what Sync did (or, in DryRun, would do) per entry.
type SyncAction string

const (
	ActionPushed    SyncAction = "pushed"     // branch pushed (PR existed or not yet created)
	ActionPRCreated SyncAction = "pr-created" // PR created
	ActionPRUpdated SyncAction = "pr-updated" // PR title/body/base updated
	ActionFailed    SyncAction = "failed"     // entry skipped after an error; see Err
)

// EntryResult is the outcome of syncing one stack entry.
type EntryResult struct {
	Idx         int
	Commit      Commit
	Branch      string
	BaseBranch  string
	Action      SyncAction
	PRNumber    int    // 0 if none
	PRURL       string // "" if none
	PRState     string
	MatchReason string
	Flagged     bool
	Err         error // set when Action == ActionFailed
}

// SyncReport is the outcome of a Sync run.
type SyncReport struct {
	// DryRun is true when no actions were executed.
	DryRun bool
	// Entries holds one EntryResult per stack entry, in stack order.
	Entries []EntryResult
	// Orphans are the stack PRs matched by no local commit.  Sync never
	// touches them; see Clean, Link, or close them yourself.
	Orphans []Orphan
}

// Sync pushes every entry's branch to the remote and creates or updates its
// PR, rewriting the hidden manifest so future syncs match exactly.  Each
// entry's PR base is chained to the branch below it.
//
// Per-entry failures are recorded in EntryResult.Err and do not abort the
// run; the returned error is reserved for whole-run failures (which
// currently only happen before any action).
func (s *Stack) Sync(o SyncOptions) (*SyncReport, error) {
	report := &SyncReport{DryRun: o.DryRun, Orphans: s.Orphans}
	if len(s.Entries) == 0 {
		return report, nil
	}

	dryTag := ""
	if o.DryRun {
		dryTag = "[dry-run] "
	}

	for i := range s.Entries {
		entry := &s.Entries[i]
		res := EntryResult{
			Idx:         entry.Idx,
			Commit:      entry.Commit,
			Branch:      entry.Branch,
			BaseBranch:  entry.BaseBranch,
			MatchReason: entry.MatchReason,
			Flagged:     entry.Flagged,
			PRNumber:    entry.PR.Number,
			PRURL:       entry.PR.URL,
			PRState:     entry.PR.State,
		}
		fail := func(err error) {
			res.Action, res.Err = ActionFailed, err
			report.Entries = append(report.Entries, res)
		}

		o.logf("")
		o.logf("[%d/%d] %s [%s]", entry.Idx, len(s.Entries), entry.Commit.Title, entry.MatchReason)
		if entry.Flagged {
			o.logf("  ⚠ matched by title only — verify this is the right PR")
		}

		// Push.
		action := "create"
		if localBranchExists(entry.Branch) {
			action = "update"
		}
		o.logf("  %sbranch %s (%s) → push %s", dryTag, entry.Branch, action, s.Remote)
		if err := pushBranch(s.Remote, entry.Branch, entry.Commit.SHA, o.DryRun); err != nil {
			o.logf("  ✗ push failed: %v", err)
			fail(err)
			continue
		}
		if !o.DryRun {
			o.logf("  ✓ pushed %s", entry.Branch)
		}
		res.Action = ActionPushed

		body := prBodyFor(entry, s.Prefix)

		if entry.PR.Number != 0 {
			// Update existing PR (title, body+manifest, base if changed).
			baseChanged := entry.PR.Base != "" && entry.PR.Base != entry.BaseBranch
			newBase := ""
			if baseChanged {
				newBase = entry.BaseBranch
			}
			updates := []string{"title", "body"}
			if baseChanged {
				updates = append(updates, fmt.Sprintf("base → %s", entry.BaseBranch))
			}
			if !o.DryRun {
				if err := ghEditPR(entry.PR.Number, entry.Commit.Title, body, newBase); err != nil {
					o.logf("  ✗ gh pr edit: %v", err)
					fail(err)
					continue
				}
			}
			o.logf("  %s✓ PR #%d updated (%s)", dryTag, entry.PR.Number, strings.Join(updates, ", "))
			if entry.PR.URL != "" {
				o.logf("    %s", entry.PR.URL)
			}
			res.Action = ActionPRUpdated
		} else {
			// Create new PR — body already carries the manifest.
			if o.DryRun {
				o.logf("  %swould create PR: head=%s base=%s", dryTag, entry.Branch, entry.BaseBranch)
				res.Action = ActionPRCreated
				report.Entries = append(report.Entries, res)
				continue
			}
			num, url, err := ghCreatePR(entry.Branch, entry.BaseBranch,
				entry.Commit.Title, body, s.Draft)
			if err != nil {
				o.logf("  ✗ gh pr create: %v", err)
				fail(err)
				continue
			}
			entry.PR.Number = num
			entry.PR.URL = url
			entry.PR.State = PRStateOpen
			if s.Draft {
				entry.PR.State = PRStateDraft
			}
			res.PRNumber, res.PRURL, res.PRState = num, url, entry.PR.State
			res.Action = ActionPRCreated
			o.logf("  ✓ PR #%d created%s", num, draftNote(s.Draft))
			o.logf("    %s", url)
		}
		report.Entries = append(report.Entries, res)
	}

	return report, nil
}

func draftNote(draft bool) string {
	if draft {
		return " (draft)"
	}
	return ""
}

// pushBranch points the local branch at sha and force-pushes it.
func pushBranch(remote, branch, sha string, dryRun bool) error {
	if dryRun {
		return nil
	}
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
