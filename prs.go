package prstack

import (
	"encoding/json"
	"fmt"
	"strings"
)

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

func prInfoFromJSON(pr prJSON) PRInfo {
	return PRInfo{
		Number: pr.Number,
		URL:    pr.URL,
		State:  pr.State,
		Title:  pr.Title,
		Base:   pr.BaseRefName,
	}
}

// collectPRs lists every PR whose head branch is part of this stack
// (prefix-*) and returns them keyed by head branch.  One gh call instead
// of one per branch.
func collectPRs(prefix string) (map[string]prRecord, error) {
	out, err := ghOutErr("pr", "list",
		"--state", "all",
		"--limit", "500",
		"--json", "number,url,state,title,baseRefName,isDraft,headRefName,body",
	)
	if err != nil {
		return nil, err
	}
	var prs []prJSON
	if err := json.Unmarshal([]byte(out), &prs); err != nil {
		return nil, fmt.Errorf("cannot parse `gh pr list` output: %w", err)
	}
	result := make(map[string]prRecord)
	for _, pr := range prs {
		if !strings.HasPrefix(pr.HeadRefName, prefix+"-") {
			continue
		}
		pr.State = strings.ToUpper(pr.State)
		if pr.IsDraft && pr.State == PRStateOpen {
			pr.State = PRStateDraft
		}
		man, _ := ParseManifest(pr.Body)
		result[pr.HeadRefName] = prRecord{PR: pr, Man: man}
	}
	return result, nil
}
