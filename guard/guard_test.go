// Package guard holds the release guard.
//
// The marketplace refuses plugins that ship or expose instructions aimed at
// coding agents, whether installed, printed or embedded in a binary. This test
// walks the whole repository and fails when such content reappears, so a
// release cannot regress into it.
package guard

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// skipDirs are not part of a release.
var skipDirs = map[string]bool{
	".git": true,
	"bin":  true,
}

// forbiddenNames are filenames that are an instruction channel by convention,
// whatever they contain.
var forbiddenNames = []string{
	"agents.md",
	"skill.md",
	"claude.md",
	"agent.md",
}

// forbiddenDirs are the roots a plugin must never write to or vendor.
var forbiddenDirs = []string{
	".claude",
	".agents",
	"skills",
}

// directive matches prose addressed to an agent rather than to a person.
//
// The phrases are assembled from fragments so this file does not match itself.
var directive = regexp.MustCompile(`(?i)` + strings.Join([]string{
	`for cod` + `ing agents`,
	`you are an ` + `agent`,
	`before ` + `acting`,
	`the ` + `agent must`,
	`instructions for ` + `agents`,
	`agent-` + `facing guide`,
	// UI copy counts too: a button tooltip reading "the guide an agent reads"
	// shipped in QML through the first version of this guard.
	`guide an ` + `agent`,
	`an ` + `agent reads`,
	`print the ` + `guide`,
}, "|"))

// embedMarkdown catches a Go file embedding markdown into a binary, which is
// how a printed guide survives a deleted file.
var embedMarkdown = regexp.MustCompile(`go:embed\s+\S*\.md`)

// frontMatter catches a leading YAML block with a name and description, which
// is the shape that makes a markdown file auto-load as a skill.
var frontMatter = regexp.MustCompile(`(?s)\A---\r?\n.*?\bname:.*?\bdescription:.*?\r?\n---`)

func TestNoAgentInstructionSurface(t *testing.T) {
	root := ".."
	self, err := filepath.Abs("guard_test.go")
	if err != nil {
		t.Fatal(err)
	}

	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			for _, bad := range forbiddenDirs {
				if strings.EqualFold(d.Name(), bad) {
					t.Errorf("%s: a plugin must not ship a %s directory", path, bad)
					return filepath.SkipDir
				}
			}
			return nil
		}

		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if abs == self {
			return nil
		}

		name := strings.ToLower(d.Name())
		for _, bad := range forbiddenNames {
			if name == bad {
				t.Errorf("%s: %s is an agent instruction channel", path, bad)
			}
		}

		switch filepath.Ext(name) {
		case ".md", ".go", ".qml", ".json", ".txt":
		default:
			return nil
		}

		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(body)

		if m := directive.FindString(text); m != "" {
			t.Errorf("%s: reads as an instruction to an agent (%q)", path, m)
		}
		if m := embedMarkdown.FindString(text); m != "" {
			t.Errorf("%s: embeds markdown into a binary (%q)", path, m)
		}
		if frontMatter.MatchString(text) {
			t.Errorf("%s: has skill front matter", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

var (
	manifestVersion = regexp.MustCompile(`"version"\s*:\s*"([^"]+)"`)
	makefileVersion = regexp.MustCompile(`(?m)^VERSION\s*\?=\s*(\S+)`)
)

// TestVersionsAgree keeps the three places a version lives from drifting.
//
// The marketplace shows the manifest's version, the Makefile stamps the
// binary's, and a release tag names both. Nothing else checks they match, and a
// listing claiming one version while the binary reports another is the kind of
// thing nobody notices until someone is comparing them for a reason.
//
// Neither file is read through git, so this works in a fresh clone and offline.
func TestVersionsAgree(t *testing.T) {
	manifest, err := os.ReadFile("../manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	m := manifestVersion.FindSubmatch(manifest)
	if m == nil {
		t.Fatal("manifest.json has no version field")
	}

	makefile, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	mk := makefileVersion.FindSubmatch(makefile)
	if mk == nil {
		t.Fatal("the Makefile has no VERSION")
	}

	if string(m[1]) != string(mk[1]) {
		t.Errorf("manifest.json says %q, the Makefile says %q", m[1], mk[1])
	}
}

// TestRebuildPromptIsDistinctFromFirstBuild pins the two states apart.
//
// omarchy plugin update fast-forwards the checkout and never compiles, so the
// panel has to notice that its helpers predate their source. The two
// conditions want different words and different actions: a first install has
// no binary and needs omagihu-setup install, while a stale one has a working
// binary and needs a compile and a restart. Running the interactive installer
// for a rebuild would ask again for directories already answered, and telling
// somebody with no binary to "rebuild" names something that is not there.
func TestRebuildPromptIsDistinctFromFirstBuild(t *testing.T) {
	panel, err := os.ReadFile(filepath.Join("..", "Panel.qml"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(panel)

	for _, want := range []string{
		// The probe reads mtimes rather than a version, because nothing bumps
		// a version on a plain fast-forward.
		`"-newer", root.helperPath`,
		// Stale is its own state. Folding it into helperMissing would blank a
		// dashboard that still works.
		"property bool helperStale",
		"visible: root.helperStale && !root.helperMissing",
		// A rebuild restarts the shell, because the daemon the shell already
		// started keeps running the old binary otherwise.
		"function runRebuild()",
		"root.restarter",
	} {
		if !strings.Contains(source, want) {
			t.Errorf("Panel.qml no longer contains %q", want)
		}
	}

	// The rebuild path must not reach the interactive installer.
	rebuild := source[strings.Index(source, "function runRebuild()"):]
	rebuild = rebuild[:strings.Index(rebuild, "\n  }")]
	if strings.Contains(rebuild, "setupPath") {
		t.Error("runRebuild runs omagihu-setup, which asks for directories already answered")
	}

	// Only Go that reaches the binary should trigger it. QML is read at load,
	// and most commits touch a test, so counting either would light the badge
	// for a rebuild that changes nothing.
	probe := source[strings.Index(source, "id: stalenessProbe"):]
	probe = probe[:strings.Index(probe, "stdout:")]
	if strings.Contains(probe, "*.qml") {
		t.Error("the staleness probe counts QML, which never needs compiling")
	}
	if !strings.Contains(probe, `"-not", "-name", "*_test.go"`) {
		t.Error("the staleness probe counts test files, which never reach the binary")
	}
}
