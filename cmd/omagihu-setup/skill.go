package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/karamble/omarchy-omagihu/alerts"
	"github.com/karamble/omarchy-omagihu/skill"
)

// skillRoots are where agents look for skills. Both are written, because an
// agent reads one or the other depending on how it was started.
func skillRoots() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("finding your home directory: %w", err)
	}
	return []string{
		filepath.Join(home, ".claude", "skills"),
		filepath.Join(home, ".agents", "skills"),
	}, nil
}

// catalogueMarkers bracket the generated section of SKILL.md, so the prose
// around it is written by hand and the catalogue never is.
const (
	catalogueBegin = "<!-- catalogue:begin -->"
	catalogueEnd   = "<!-- catalogue:end -->"
)

func manageSkill(opts options) error {
	f := opts.alerts
	switch {
	case f.generate != "":
		return generateSkill(f.generate)
	case f.recipes:
		fmt.Print(skill.Recipes)
		return nil
	case f.uninstall:
		return uninstallSkill()
	case f.install:
		return installSkill()
	}
	return errors.New("skill needs --install, --uninstall, --recipes or --generate PATH")
}

// installSkill writes the skill into every root an agent reads.
func installSkill() error {
	roots, err := skillRoots()
	if err != nil {
		return err
	}
	for _, root := range roots {
		dir := filepath.Join(root, skill.Name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
		files := map[string]string{
			"SKILL.md":   skill.Doc,
			"recipes.md": skill.Recipes,
		}
		for name, body := range files {
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				return fmt.Errorf("writing %s: %w", path, err)
			}
		}
		fmt.Println("installed", dir)
	}
	fmt.Println("agents in a new session can now arm watches with: omagihu-setup arm")
	return nil
}

// uninstallSkill removes only what installSkill wrote, and only the directory
// it owns.
func uninstallSkill() error {
	roots, err := skillRoots()
	if err != nil {
		return err
	}
	for _, root := range roots {
		dir := filepath.Join(root, skill.Name)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("removing %s: %w", dir, err)
		}
		fmt.Println("removed", dir)
	}
	return nil
}

// generateSkill rewrites the catalogue section of a SKILL.md in place. It is
// what "make skill" runs, so the documented paths cannot drift from the ones
// the daemon actually offers.
func generateSkill(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	doc := string(raw)

	start := strings.Index(doc, catalogueBegin)
	end := strings.Index(doc, catalogueEnd)
	if start < 0 || end < 0 || end < start {
		return fmt.Errorf("%s has no catalogue markers to fill", path)
	}

	section := catalogueBegin +
		"\n<!-- Generated from alerts.Catalogue() by make skill. Do not edit by hand. -->\n" +
		alerts.CatalogueMarkdown() + "\n"

	updated := doc[:start] + section + doc[end:]
	if updated == doc {
		fmt.Println(path, "is already current")
		return nil
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	fmt.Println("regenerated the catalogue in", path)
	return nil
}
