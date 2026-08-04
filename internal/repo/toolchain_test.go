// Package repo holds guards about the shape of the repository itself rather
// than the behaviour of the emulator — conventions that are otherwise enforced
// by nothing but memory.
package repo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// skipDir names directories that are not ours to police: third-party manifests,
// build output, and — the one that actually bit — `.claude/worktrees`, which
// holds a full checkout of this repository belonging to another session.
func skipDir(name string) bool {
	switch name {
	case "node_modules", ".git", "dist", ".venv", ".claude":
		return true
	}
	return false
}

// TestEveryPackageForcesPnpm closes a hole the root guard does not cover.
//
// `package.json` at the root carries `preinstall: npx only-allow pnpm`, which
// stops `npm install` THERE. It does nothing for `cd portal && npm install`:
// npm reads that directory's own manifest, finds no guard, resolves its own
// tree and writes a `package-lock.json`. The workspace then has two lockfiles
// disagreeing about versions, and whichever CI reads is a coin toss.
//
// So every manifest carries the guard, not just the one at the top.
//
// `npx`, deliberately, and it is not an oversight to tidy into `pnpm dlx`: the
// script has to run under the package manager being REFUSED. Someone typing
// `npm install` has npx; they may well not have pnpm at all, and a guard that
// needs pnpm to tell you to use pnpm helps nobody.
func TestEveryPackageForcesPnpm(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	const guard = "only-allow pnpm"
	var manifests []string
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// node_modules holds thousands of third-party manifests, none of
			// them ours to police.
			if skipDir(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if info.Name() == "package.json" {
			manifests = append(manifests, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A walk that found nothing would pass while checking nothing.
	if len(manifests) < 3 {
		t.Fatalf("found %d package.json files; expected the workspace root, "+
			"portal and website at least — the walk is not reaching them", len(manifests))
	}

	for _, m := range manifests {
		raw, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if err := json.Unmarshal(raw, &pkg); err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		rel, _ := filepath.Rel(root, m)
		if !strings.Contains(pkg.Scripts["preinstall"], guard) {
			t.Errorf("%s has no `preinstall: npx %s` — `npm install` in that "+
				"directory would resolve its own tree and write a second lockfile",
				filepath.ToSlash(rel), guard)
		}
	}
}

// TestNoRivalLockfileIsCommitted is the other half: the guard above stops the
// lockfile being CREATED by someone who reads the error; this catches one that
// arrives anyway — committed from a machine that skipped scripts, or dragged in
// with a vendored directory.
//
// Two lockfiles is worse than the wrong one. Each tool reads its own and both
// report success, so the divergence surfaces as a version that is somehow
// different in CI, with nothing in the diff to explain it.
func TestNoRivalLockfileIsCommitted(t *testing.T) {
	root, _ := filepath.Abs("../..")
	rivals := []string{"package-lock.json", "yarn.lock", "npm-shrinkwrap.json", "bun.lockb"}

	var found []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if skipDir(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		for _, r := range rivals {
			if info.Name() == r {
				rel, _ := filepath.Rel(root, path)
				found = append(found, filepath.ToSlash(rel))
			}
		}
		return nil
	})
	if len(found) > 0 {
		t.Fatalf("lockfiles from another package manager: %s — pnpm-lock.yaml is "+
			"the only one this repository resolves against", strings.Join(found, ", "))
	}

	// And the one that SHOULD be there is.
	if _, err := os.Stat(filepath.Join(root, "pnpm-lock.yaml")); err != nil {
		t.Fatalf("pnpm-lock.yaml is missing: %v", err)
	}
}

// TestNoAutomationShellsOutToNpmOrYarn keeps the rule true where it actually
// matters — the places that run unattended and would silently install a
// different tree than the one anybody tested.
func TestNoAutomationShellsOutToNpmOrYarn(t *testing.T) {
	root, _ := filepath.Abs("../..")
	// A LEADING BOUNDARY, because "npm install" is a substring of "pnpm
	// install" — the first version of this guard failed on the two lines in CI
	// that were already correct, which is the most useless kind of red.
	//
	// `npx` is exempt: it is how the only-allow guard runs, and it executes a
	// published package rather than resolving this workspace's tree.
	banned := regexp.MustCompile(`(^|[^\w.-])(npm|yarn)\s+(install|ci|run|add|exec)\b`)

	dirs := []string{".github/workflows", "scripts", "e2e"}
	var offenders []string
	scanned := 0
	for _, d := range dirs {
		_ = filepath.Walk(filepath.Join(root, d), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			ext := filepath.Ext(path)
			if ext != ".yml" && ext != ".yaml" && ext != ".sh" && ext != ".py" {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			scanned++
			rel, _ := filepath.Rel(root, path)
			for i, line := range strings.Split(string(raw), "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "#") {
					continue
				}
				if banned.MatchString(line) {
					offenders = append(offenders,
						filepath.ToSlash(rel)+":"+itoa(i+1)+": "+trimmed)
				}
			}
			return nil
		})
	}
	if scanned < 10 {
		t.Fatalf("only scanned %d automation files; the walk is not reaching them", scanned)
	}
	if len(offenders) > 0 {
		t.Fatalf("automation shells out to npm/yarn, which would resolve a "+
			"different tree than pnpm-lock.yaml pins:\n  %s",
			strings.Join(offenders, "\n  "))
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
