package prstack

import (
	"reflect"
	"testing"
)

func mkRec(branch string, pr int, man *Manifest) prRecord {
	return prRecord{
		PR: prJSON{
			Number:      pr,
			URL:         "https://github.com/example/ex/pull/" + itoa(pr),
			State:       PRStateOpen,
			Title:       "pr " + branch,
			BaseRefName: "main",
			HeadRefName: branch,
			Body:        "",
		},
		Man: man,
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestManifestRoundtrip(t *testing.T) {
	man := Manifest{
		Stack:    "raggi/foo",
		Branch:   "raggi/foo-aa11bb22",
		PR:       7,
		Slot:     2,
		Title:    "add c",
		Author:   "T <t@t>",
		ChangeID: "I1111111111111111111111111111111111111111",
		PatchID:  "4f8c91a2d3e0",
	}
	got, ok := ParseManifest(man.Serialize())
	if !ok {
		t.Fatal("manifest not found in serialized body")
	}
	if !reflect.DeepEqual(*got, man) {
		t.Fatalf("roundtrip mismatch:\n got  %#v\n want %#v", *got, man)
	}
}

func TestParseManifestEdgeCases(t *testing.T) {
	if _, ok := ParseManifest("no manifest here"); ok {
		t.Fatal("unexpected manifest in plain body")
	}
	// manifest embedded mid-body with user text around it
	body := "some user text\n\n" + Manifest{Stack: "s", Branch: "b", Title: "t"}.Serialize() + "\n\nmore text"
	man, ok := ParseManifest(body)
	if !ok || man.Stack != "s" || man.Branch != "b" || man.Title != "t" {
		t.Fatalf("bad parse: %#v ok=%v", man, ok)
	}
	if StripManifest(body) != "some user text\n\nmore text" {
		t.Fatalf("StripManifest left residue: %q", StripManifest(body))
	}
}

func TestReconcile(t *testing.T) {
	const prefix = "raggi/foo"
	cid := "I1111111111111111111111111111111111111111"

	// Existing stack state: PR #1 via change-id manifest, PR #2 via
	// patch-id manifest, PR #3 legacy (no manifest, sha-named).
	prs := map[string]prRecord{
		prefix + "-11111111": mkRec(prefix+"-11111111", 1, &Manifest{
			Stack: prefix, Branch: prefix + "-11111111", PR: 1,
			Title: "add b", Author: "T <t@t>", ChangeID: cid,
		}),
		prefix + "-deadbeef": mkRec(prefix+"-deadbeef", 2, &Manifest{
			Stack: prefix, Branch: prefix + "-deadbeef", PR: 2,
			Title: "add c", Author: "T <t@t>", PatchID: "pid-c",
		}),
		prefix + "-a96cf267": mkRec(prefix+"-a96cf267", 3, nil), // legacy, no manifest
	}

	commits := []Commit{
		{ // matches PR #2 by patch-id
			SHA: "ffffffffffffffffffffffffffffffffffffffff", Title: "add c",
			Author: "T <t@t>", PatchID: "pid-c",
		},
		{ // matches PR #1 by change-id
			SHA: "2222222222222222222222222222222222222222", Title: "add b",
			Author: "T <t@t>", ChangeID: cid, PatchID: "pid-b",
		},
		{ // matches PR #3 by legacy branch-name (sha-derived)
			SHA: "a96cf2670000000000000000000000000000000", Title: "add d",
			Author: "T <t@t>", PatchID: "pid-d",
		},
	}

	stack := reconcile(commits, prefix, "main", prs)

	if len(stack.Entries) != 3 {
		t.Fatalf("want 3 entries, got %d", len(stack.Entries))
	}
	cases := []struct {
		idx    int
		branch string
		reason string
		pr     int
	}{
		{1, prefix + "-deadbeef", MatchPatchID, 2},
		{2, prefix + "-11111111", MatchChangeID, 1},
		{3, prefix + "-a96cf267", MatchBranchNam, 3},
	}
	for _, c := range cases {
		e := stack.Entries[c.idx-1]
		if e.Branch != c.branch || e.MatchReason != c.reason || e.PR.Number != c.pr {
			t.Errorf("entry %d: got branch=%s reason=%s pr=%d, want branch=%s reason=%s pr=%d",
				c.idx, e.Branch, e.MatchReason, e.PR.Number, c.branch, c.reason, c.pr)
		}
	}

	// Bases must chain in stack order, not PR number order.
	if stack.Entries[0].BaseBranch != "main" {
		t.Errorf("entry 1 base = %q, want main", stack.Entries[0].BaseBranch)
	}
	for i := 1; i < 3; i++ {
		want := stack.Entries[i-1].Branch
		if stack.Entries[i].BaseBranch != want {
			t.Errorf("entry %d base = %q, want %q", i+1, stack.Entries[i].BaseBranch, want)
		}
	}

	// No orphans: all three PRs were consumed.
	if len(stack.Orphans) != 0 {
		t.Errorf("want 0 orphans, got %d: %v", len(stack.Orphans), stack.Orphans)
	}
}

func TestReconcileAmendTitleHeuristic(t *testing.T) {
	const prefix = "raggi/foo"
	prs := map[string]prRecord{
		prefix + "-aa11bb22": mkRec(prefix+"-aa11bb22", 5, &Manifest{
			Stack: prefix, Branch: prefix + "-aa11bb22", PR: 5,
			Title: "add c", Author: "T <t@t>", PatchID: "old-pid",
		}),
	}
	// Amended commit: same title+author, new patch-id, no change-id.
	commits := []Commit{{
		SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Title: "add c",
		Author: "T <t@t>", PatchID: "new-pid",
	}}

	stack := reconcile(commits, prefix, "main", prs)
	e := stack.Entries[0]
	if e.MatchReason != MatchTitle || !e.Flagged || e.PR.Number != 5 || e.Branch != prefix+"-aa11bb22" {
		t.Fatalf("amend not matched by title heuristic: %#v", e)
	}
}

func TestReconcileOrphansAndNew(t *testing.T) {
	const prefix = "raggi/foo"
	prs := map[string]prRecord{
		prefix + "-gone1234": mkRec(prefix+"-gone1234", 9, &Manifest{
			Stack: prefix, Branch: prefix + "-gone1234", PR: 9,
			Title: "dropped commit", Author: "T <t@t>", PatchID: "pid-x",
		}),
	}
	commits := []Commit{{
		SHA: "cccccccccccccccccccccccccccccccccccccccc", Title: "brand new",
		Author: "T <t@t>", PatchID: "pid-y",
	}}

	stack := reconcile(commits, prefix, "main", prs)
	e := stack.Entries[0]
	if e.MatchReason != MatchNew || e.PR.Number != 0 {
		t.Fatalf("unmatched commit should be new: %#v", e)
	}
	if e.Branch != prefix+"-cccccccc" {
		t.Fatalf("new branch minted from short sha: %q", e.Branch)
	}
	if len(stack.Orphans) != 1 || stack.Orphans[0].Branch != prefix+"-gone1234" {
		t.Fatalf("orphan not detected: %#v", stack.Orphans)
	}
}
