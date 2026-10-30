package prstack

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

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
var manifestStripRe = regexp.MustCompile(`(?s)[ \t]*<!--\s*prstack v1\s*\n.*?-->[ \t]*(\n[ \t]*)*`)

// ParseManifest extracts the prstack manifest from a PR body, if any.
func ParseManifest(body string) (*Manifest, bool) {
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

// Serialize renders the manifest as a hidden HTML comment suitable for a
// PR body.  Zero fields are omitted.
func (man Manifest) Serialize() string {
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

// StripManifest removes any prstack manifest from a PR body, returning
// the human-visible remainder.
func StripManifest(body string) string {
	return strings.TrimSpace(manifestStripRe.ReplaceAllString(body, ""))
}

// joinBodyManifest appends a manifest comment to a PR body.
func joinBodyManifest(body, man string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return man
	}
	return body + "\n\n" + man
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
func manifestFor(entry *StackEntry, prefix string) Manifest {
	return Manifest{
		Stack:    prefix,
		Branch:   entry.Branch,
		PR:       entry.PR.Number,
		Slot:     entry.Idx,
		Title:    entry.Commit.Title,
		Author:   entry.Commit.Author,
		ChangeID: entry.Commit.ChangeID,
		PatchID:  entry.Commit.PatchID,
	}
}

// prBodyFor renders the full PR body: visible commit body + hidden manifest.
func prBodyFor(entry *StackEntry, prefix string) string {
	return joinBodyManifest(formatPRBody(*entry), manifestFor(entry, prefix).Serialize())
}
