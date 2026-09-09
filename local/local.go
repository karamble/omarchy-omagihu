// Package local watches git checkouts on this machine. It answers the question
// the remote plane cannot: where is work at risk right now, because it exists
// only here.
package local

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Defaults cover the usual places source lives, including ~/go/src, which is
// where a GOPATH tree actually sits.
var (
	DefaultRoots    = []string{"~/Projects", "~/src", "~/go/src", "~/.config/omarchy/plugins"}
	DefaultExcludes = []string{"node_modules", "target", "vendor", "dist", "build"}
)

const (
	DefaultMaxDepth = 4
	maxRepos        = 500
)

// Config decides where to look. The zero value is not useful; call
// DefaultConfig and adjust.
type Config struct {
	Roots    []string
	MaxDepth int
	Excludes []string
}

// DefaultConfig is the built-in scan configuration.
func DefaultConfig() Config {
	return Config{
		Roots:    slices.Clone(DefaultRoots),
		MaxDepth: DefaultMaxDepth,
		Excludes: slices.Clone(DefaultExcludes),
	}
}

// Operation names an interrupted git operation left in the working tree.
type Operation string

const (
	OpNone         Operation = ""
	OpRebase       Operation = "rebase"
	OpMerge        Operation = "merge"
	OpCherryPick   Operation = "cherry-pick"
	OpRevert       Operation = "revert"
	OpBisect       Operation = "bisect"
	OpApplyMailbox Operation = "am"
)

// Commit is the tip commit of a repository.
type Commit struct {
	SHA     string    `json:"sha"`
	Subject string    `json:"subject"`
	Author  string    `json:"author"`
	At      time.Time `json:"at,omitzero"`
}

// Repo is everything known about one checkout.
type Repo struct {
	Path     string `json:"path"`
	Name     string `json:"name"`
	Branch   string `json:"branch"`
	Upstream string `json:"upstream,omitempty"`
	Detached bool   `json:"detached,omitempty"`

	Ahead  int `json:"ahead"`
	Behind int `json:"behind"`

	Staged     int `json:"staged"`
	Modified   int `json:"modified"`
	Deleted    int `json:"deleted"`
	Untracked  int `json:"untracked"`
	Conflicted int `json:"conflicted"`
	Stashes    int `json:"stashes"`

	// Unpushed is work that exists only on this machine: commits ahead of the
	// upstream, or every commit on a branch that has no upstream at all.
	Unpushed   int  `json:"unpushed"`
	NoUpstream bool `json:"noUpstream,omitempty"`

	// UpstreamBehind is how far a fork trails the repository it was forked
	// from, counted against the refs last fetched.
	UpstreamBehind int `json:"upstreamBehind,omitempty"`

	Operation Operation         `json:"operation,omitempty"`
	Remotes   map[string]string `json:"remotes,omitempty"`
	Last      Commit            `json:"last,omitzero"`

	ObservedAt time.Time `json:"observedAt,omitzero"`
	Error      string    `json:"error,omitempty"`
}

// Dirty reports whether the working tree has uncommitted changes.
func (r Repo) Dirty() bool {
	return r.Staged+r.Modified+r.Deleted+r.Untracked+r.Conflicted > 0
}

// AtRisk reports whether this repo holds something a person would not want to
// lose or forget: unpushed commits, an interrupted operation, or dirt.
func (r Repo) AtRisk() bool {
	return r.Unpushed > 0 || r.Operation != OpNone || r.Dirty()
}

// Snapshot is a consistent read of every watched repository.
type Snapshot struct {
	TakenAt time.Time `json:"takenAt"`
	Roots   []string  `json:"roots"`
	Repos   []Repo    `json:"repos"`
}

// ExpandPath resolves a leading ~ or $HOME against the current user's home.
func ExpandPath(raw string) string {
	p := strings.TrimSpace(raw)
	if p == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	switch {
	case p == "~" || p == "$HOME":
		return home
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(home, p[2:])
	case strings.HasPrefix(p, "$HOME/"):
		return filepath.Join(home, p[6:])
	}
	return p
}

// SplitList parses a comma, colon or newline separated setting.
func SplitList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ':' || r == '\n'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// Discover walks the configured roots and returns the repository directories it
// finds, stopping at maxRepos so a pathological tree cannot run away.
func Discover(ctx context.Context, cfg Config) []string {
	var found []string
	seen := make(map[string]struct{})

	for _, raw := range cfg.Roots {
		if ctx.Err() != nil || len(found) >= maxRepos {
			break
		}
		root := ExpandPath(raw)
		if root == "" {
			continue
		}
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			continue
		}
		walk(ctx, root, 0, cfg, seen, &found)
	}
	slices.Sort(found)
	return found
}

func walk(ctx context.Context, dir string, depth int, cfg Config, seen map[string]struct{}, out *[]string) {
	if ctx.Err() != nil || depth > cfg.MaxDepth || len(*out) >= maxRepos {
		return
	}
	// A directory holding .git is a repository; do not descend into it, so
	// submodules and vendored checkouts do not multiply the list.
	if isRepo(dir) {
		if _, dup := seen[dir]; !dup {
			seen[dir] = struct{}{}
			*out = append(*out, dir)
		}
		return
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if ctx.Err() != nil || len(*out) >= maxRepos {
			return
		}
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") || slices.Contains(cfg.Excludes, name) {
			continue
		}
		// Do not follow symlinked directories: they invite cycles and
		// duplicate entries.
		if info, err := e.Info(); err == nil && info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		walk(ctx, filepath.Join(dir, name), depth+1, cfg, seen, out)
	}
}

// isRepo reports whether dir is the root of a git checkout. A .git file rather
// than a directory means a worktree or submodule, which still counts.
func isRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}
