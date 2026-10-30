package prstack

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

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

// git runs a git command without failing hard; check .Code.
func git(args ...string) runResult {
	return runCmd("", append([]string{"git"}, args...)...)
}

// gitOut runs a git command and dies on failure (CLI convenience).
func gitOut(args ...string) string {
	r := git(args...)
	if r.Code != 0 {
		panic("git " + strings.Join(args, " ") + ": " + r.Stderr)
	}
	return r.Stdout
}

// gitOutErr runs a git command and returns its error.
func gitOutErr(args ...string) (string, error) {
	r := git(args...)
	if r.Code != 0 {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), r.Stderr)
	}
	return r.Stdout, nil
}

// ghOutErr runs a gh command and returns its error.
func ghOutErr(args ...string) (string, error) {
	r := runCmd("", append([]string{"gh"}, args...)...)
	if r.Code != 0 {
		return "", fmt.Errorf("gh %s: %s", strings.Join(args, " "), r.Stderr)
	}
	return r.Stdout, nil
}

// passthrough runs a command inheriting the terminal (interactive use).
func passthrough(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
