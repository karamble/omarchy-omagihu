package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// gitTimeout bounds any single git invocation. A repository on a stalled network
// mount must not hold the watcher.
const gitTimeout = 10 * time.Second

// Inspect gathers the full state of one checkout. It never returns an error:
// a repository that cannot be read reports the problem in Repo.Error so one bad
// checkout cannot blank the whole view.
func Inspect(ctx context.Context, path string) Repo {
	repo := Repo{
		Path:       path,
		Name:       filepath.Base(path),
		ObservedAt: time.Now(),
	}

	statusOut, err := runGit(ctx, path, "status", "--porcelain=v2", "-z", "--branch")
	if err != nil {
		repo.Error = err.Error()
		return repo
	}
	parseStatus(statusOut, &repo)

	repo.Remotes = parseRemotes(mustRun(ctx, path, "remote", "-v"))
	repo.Operation = detectOperation(path)
	repo.Stashes = countLines(mustRun(ctx, path, "stash", "list"))
	repo.Last = parseLastCommit(mustRun(ctx, path,
		"log", "-1", "--format=%H%x00%s%x00%an%x00%aI"))

	// A fork's distance from the project it was forked from. This reads only
	// refs that are already on disk, so Inspect stays free of network calls;
	// the background fetch is what keeps it honest.
	if _, forked := repo.Remotes["upstream"]; forked {
		if n, err := strconv.Atoi(strings.TrimSpace(
			mustRun(ctx, path, "rev-list", "--count", "HEAD..refs/remotes/upstream/HEAD"))); err == nil {
			repo.UpstreamBehind = n
		}
	}

	// Unpushed is defined as commits reachable from HEAD but from no remote
	// tracking ref: work that exists only on this machine. With no remotes at
	// all there is nowhere to push, so the count would flag every scratch repo.
	if len(repo.Remotes) > 0 {
		if n, err := strconv.Atoi(strings.TrimSpace(
			mustRun(ctx, path, "rev-list", "--count", "HEAD", "--not", "--remotes"))); err == nil {
			repo.Unpushed = n
		}
	}
	return repo
}

// parseStatus reads git status --porcelain=v2 -z --branch.
//
// Header lines carry the branch, its upstream and the ahead/behind pair. Entry
// lines are one per path: "1" changed, "2" renamed or copied, "u" unmerged,
// "?" untracked. The two status characters are staged and worktree state, with
// "." meaning unmodified.
func parseStatus(out string, repo *Repo) {
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if f == "" {
			continue
		}
		switch {
		case strings.HasPrefix(f, "# branch.head "):
			head := strings.TrimPrefix(f, "# branch.head ")
			if head == "(detached)" {
				repo.Detached = true
			}
			repo.Branch = head
		case strings.HasPrefix(f, "# branch.upstream "):
			repo.Upstream = strings.TrimPrefix(f, "# branch.upstream ")
		case strings.HasPrefix(f, "# branch.ab "):
			repo.Ahead, repo.Behind = parseAheadBehind(strings.TrimPrefix(f, "# branch.ab "))
		case strings.HasPrefix(f, "# "):
			// other headers (oid, stash) are not needed
		case strings.HasPrefix(f, "? "):
			repo.Untracked++
		case strings.HasPrefix(f, "! "):
			// ignored, never counted
		case strings.HasPrefix(f, "u "):
			repo.Conflicted++
		case strings.HasPrefix(f, "1 "), strings.HasPrefix(f, "2 "):
			countEntry(f, repo)
			// A rename entry is followed by its original path as a separate
			// NUL-terminated field; skip it so it is not read as an entry.
			if strings.HasPrefix(f, "2 ") {
				i++
			}
		}
	}
	// A branch with no upstream cannot be ahead or behind anything.
	repo.NoUpstream = repo.Upstream == "" && !repo.Detached
}

// countEntry classifies one changed entry by its XY status characters.
func countEntry(entry string, repo *Repo) {
	parts := strings.SplitN(entry, " ", 3)
	if len(parts) < 2 || len(parts[1]) < 2 {
		return
	}
	staged, worktree := parts[1][0], parts[1][1]

	if staged != '.' {
		repo.Staged++
	}
	switch worktree {
	case 'M':
		repo.Modified++
	case 'D':
		repo.Deleted++
	}
	// A staged deletion with a clean worktree still removes a file.
	if staged == 'D' && worktree == '.' {
		repo.Deleted++
	}
}

func parseAheadBehind(ab string) (ahead, behind int) {
	for _, part := range strings.Fields(ab) {
		if len(part) < 2 {
			continue
		}
		n, err := strconv.Atoi(part[1:])
		if err != nil {
			continue
		}
		switch part[0] {
		case '+':
			ahead = n
		case '-':
			behind = n
		}
	}
	return ahead, behind
}

func parseRemotes(out string) map[string]string {
	remotes := make(map[string]string)
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// "origin  git@github.com:o/r.git (fetch)", keeping the fetch url.
		if len(fields) >= 3 && fields[2] != "(fetch)" {
			continue
		}
		remotes[fields[0]] = fields[1]
	}
	if len(remotes) == 0 {
		return nil
	}
	return remotes
}

func parseLastCommit(out string) Commit {
	parts := strings.Split(strings.TrimSpace(out), "\x00")
	if len(parts) < 4 {
		return Commit{}
	}
	c := Commit{SHA: parts[0], Subject: parts[1], Author: parts[2]}
	if at, err := time.Parse(time.RFC3339, parts[3]); err == nil {
		c.At = at
	}
	return c
}

// detectOperation looks for the marker files git leaves behind when an
// operation is interrupted. These are the states people forget about.
func detectOperation(repoPath string) Operation {
	dir, err := resolveGitDir(repoPath)
	if err != nil {
		return OpNone
	}
	markers := []struct {
		path string
		op   Operation
	}{
		{"rebase-merge", OpRebase},
		{"rebase-apply", OpRebase},
		{"MERGE_HEAD", OpMerge},
		{"CHERRY_PICK_HEAD", OpCherryPick},
		{"REVERT_HEAD", OpRevert},
		{"BISECT_LOG", OpBisect},
	}
	for _, m := range markers {
		if _, err := os.Stat(filepath.Join(dir, m.path)); err == nil {
			// rebase-apply is shared by rebase and am; applying-mailbox has its
			// own marker inside.
			if m.op == OpRebase && m.path == "rebase-apply" {
				if _, err := os.Stat(filepath.Join(dir, "rebase-apply", "applying")); err == nil {
					return OpApplyMailbox
				}
			}
			return m.op
		}
	}
	return OpNone
}

// resolveGitDir returns the real .git directory, following the "gitdir:"
// pointer file used by worktrees and submodules.
func resolveGitDir(repoPath string) (string, error) {
	p := filepath.Join(repoPath, ".git")
	info, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return p, nil
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir: ")
	if !ok {
		return "", errors.New("unrecognised .git file")
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(repoPath, target)
	}
	return target, nil
}

// runGit executes git in dir with its own deadline.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// Keep git from prompting for credentials or reading the user's pager.
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_PAGER=cat",
	)

	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(exit.Stderr)))
		}
		return "", fmt.Errorf("git %s in %s: %w", args[0], dir, err)
	}
	return string(out), nil
}

// mustRun is runGit for facts that are nice to have: a failure yields an empty
// string rather than sinking the whole inspection.
func mustRun(ctx context.Context, dir string, args ...string) string {
	out, err := runGit(ctx, dir, args...)
	if err != nil {
		return ""
	}
	return out
}

func countLines(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}
