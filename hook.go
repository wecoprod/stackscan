package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Git hooks: after a pull, a checkout or a rebase, and before a commit,
// stackscan looks at the manifests that changed (composer.lock,
// package.json, Dockerfile, compose files...), lists the version changes and
// reports anything that is, or is about to be, end of life.
//
// The hooks never block anything unless installed with --strict, which
// makes pre-commit refuse a commit that brings in an end-of-life version.

const (
	markBegin = "# >>> stackscan >>>"
	markEnd   = "# <<< stackscan <<<"
)

// Installed by default: pre-commit only. --pull adds the hooks that run after
// git pull, checkout and rebase. Uninstall removes all of them.
var hookNames = []string{"post-merge", "post-checkout", "post-rewrite", "pre-commit"}
var pullHooks = []string{"post-merge", "post-checkout", "post-rewrite"}

func hookMain(args []string) {
	if st, err := os.Stdout.Stat(); err != nil || st.Mode()&os.ModeCharDevice == 0 || os.Getenv("NO_COLOR") != "" {
		useColor = false
	}
	if len(args) == 0 {
		hookUsage()
		os.Exit(1)
	}
	switch args[0] {
	case "install":
		fs := flag.NewFlagSet("hook install", flag.ExitOnError)
		strict := fs.Bool("strict", false, "pre-commit refuses a commit that brings in an end-of-life version")
		pull := fs.Bool("pull", false, "also report after git pull, checkout and rebase")
		all := fs.String("all", "", "install in every git repository found under this directory")
		fs.Parse(args[1:])
		repos := targetRepos(*all, fs.Args())
		for _, r := range repos {
			installHooks(r, *strict, *pull)
		}
	case "uninstall":
		fs := flag.NewFlagSet("hook uninstall", flag.ExitOnError)
		all := fs.String("all", "", "uninstall from every git repository found under this directory")
		fs.Parse(args[1:])
		for _, r := range targetRepos(*all, fs.Args()) {
			uninstallHooks(r)
		}
	case "run":
		if len(args) < 2 {
			hookUsage()
			os.Exit(1)
		}
		os.Exit(runHook(args[1], args[2:]))
	default:
		hookUsage()
		os.Exit(1)
	}
}

func hookUsage() {
	fmt.Fprint(os.Stderr, `Usage:
  stackscan hook install [--strict] [--pull] [repo]   install the pre-commit hook (current repository by default)
  stackscan hook install --all ~/Development          install in every repository under a folder
  stackscan hook uninstall [--all dir] [repo]         remove the hooks

On git commit, when a manifest is part of the commit (composer.lock,
package.json, Dockerfile, compose...), stackscan lists the version changes and
what is at or near end of life. It never blocks the commit, unless installed
with --strict: it then refuses a commit that brings in an end-of-life version.
--pull also reports after git pull, checkout and rebase.
Set STACKSCAN_SKIP=1 to silence the hooks for one command.
`)
}

// ---------- install ----------

func targetRepos(all string, rest []string) []string {
	if all != "" {
		var repos []string
		root, _ := filepath.Abs(all)
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			if skipDirs[d.Name()] || (p != root && strings.HasPrefix(d.Name(), ".")) || strings.Count(rel, string(os.PathSeparator)) > 4 {
				return filepath.SkipDir
			}
			if gi, err := os.Stat(filepath.Join(p, ".git")); err == nil && gi.IsDir() {
				repos = append(repos, p)
				return filepath.SkipDir
			}
			return nil
		})
		if len(repos) == 0 {
			fmt.Fprintln(os.Stderr, "No git repository found under", root)
		}
		return repos
	}
	dir := "."
	if len(rest) > 0 {
		dir = rest[0]
	}
	return []string{dir}
}

func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func installHooks(repo string, strict, pull bool) {
	top, err := gitOut(repo, "rev-parse", "--show-toplevel")
	if err != nil {
		fmt.Fprintln(os.Stderr, repo+": not a git repository")
		return
	}
	if hp, _ := gitOut(top, "config", "core.hooksPath"); hp != "" {
		fmt.Printf("%s: hooks are managed elsewhere (core.hooksPath = %s).\n", top, hp)
		fmt.Println("  Add this line to your pre-commit hook:")
		fmt.Println(`    command -v stackscan >/dev/null && stackscan hook run pre-commit "$@" || true`)
		return
	}
	dir, err := gitOut(top, "rev-parse", "--git-path", "hooks")
	if err != nil {
		fmt.Fprintln(os.Stderr, top+": cannot find the hooks directory")
		return
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(top, dir)
	}
	os.MkdirAll(dir, 0o755)

	self, _ := os.Executable()
	names := []string{"pre-commit"}
	if pull {
		names = append(pullHooks, names...)
	}
	var done, skipped []string
	for _, h := range names {
		block := hookBlock(h, self, strict)
		if err := writeHook(filepath.Join(dir, h), block); err != nil {
			skipped = append(skipped, h+" ("+err.Error()+")")
			continue
		}
		done = append(done, h)
	}
	mode := ""
	if strict {
		mode = " — strict: pre-commit blocks end-of-life versions"
	}
	fmt.Printf("%s %s: %s%s\n", col(cGreen, "✓"), top, strings.Join(done, ", "), mode)
	for _, s := range skipped {
		fmt.Printf("  %s skipped %s\n", col(cYellow, "!"), s)
	}
}

func hookBlock(name, self string, strict bool) string {
	// Never fail the hook (and so never block git) unless strict mode asks
	// pre-commit to refuse end-of-life versions. A missing binary or
	// STACKSCAN_SKIP leaves the hook a no-op.
	run := fmt.Sprintf(`"$S" hook run %s "$@" || true`, name)
	if name == "pre-commit" && strict {
		run = fmt.Sprintf(`"$S" hook run %s --strict "$@" || exit $?`, name)
	}
	// Prefer stackscan from PATH; fall back to the binary that installed the
	// hook, since GUI git clients often run hooks with a minimal PATH.
	return fmt.Sprintf(`%s
S=$(command -v stackscan 2>/dev/null || echo %q)
if [ -x "$S" ] && [ -z "$STACKSCAN_SKIP" ]; then
  %s
fi
%s
`, markBegin, self, run, markEnd)
}

func writeHook(path, block string) error {
	old, err := os.ReadFile(path)
	content := ""
	switch {
	case os.IsNotExist(err):
		content = "#!/bin/sh\n" + block
	case err != nil:
		return err
	default:
		s := string(old)
		first := strings.SplitN(s, "\n", 2)[0]
		if !strings.HasPrefix(first, "#!") || !(strings.Contains(first, "sh") || strings.Contains(first, "bash")) {
			return fmt.Errorf("existing hook is not a shell script")
		}
		s = removeBlock(s)
		if !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		content = s + block
	}
	return os.WriteFile(path, []byte(content), 0o755)
}

func removeBlock(s string) string {
	i := strings.Index(s, markBegin)
	j := strings.Index(s, markEnd)
	if i < 0 || j < i {
		return s
	}
	end := j + len(markEnd)
	if end < len(s) && s[end] == '\n' {
		end++
	}
	return s[:i] + s[end:]
}

func uninstallHooks(repo string) {
	top, err := gitOut(repo, "rev-parse", "--show-toplevel")
	if err != nil {
		fmt.Fprintln(os.Stderr, repo+": not a git repository")
		return
	}
	dir, _ := gitOut(top, "rev-parse", "--git-path", "hooks")
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(top, dir)
	}
	removed := 0
	for _, h := range hookNames {
		p := filepath.Join(dir, h)
		b, err := os.ReadFile(p)
		if err != nil || !strings.Contains(string(b), markBegin) {
			continue
		}
		rest := removeBlock(string(b))
		if strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), "#!/bin/sh")) == "" {
			os.Remove(p)
		} else {
			os.WriteFile(p, []byte(rest), 0o755)
		}
		removed++
	}
	fmt.Printf("%s %s: %d hook(s) removed\n", col(cGreen, "✓"), top, removed)
}

// ---------- run ----------

func runHook(name string, args []string) int {
	strict := false
	var rest []string
	for _, a := range args {
		if a == "--strict" {
			strict = true
		} else {
			rest = append(rest, a)
		}
	}
	if os.Getenv("STACKSCAN_SKIP") != "" {
		return 0
	}
	if st, err := os.Stdout.Stat(); err != nil || st.Mode()&os.ModeCharDevice == 0 || os.Getenv("NO_COLOR") != "" {
		useColor = false
	}
	top, err := gitOut(".", "rev-parse", "--show-toplevel")
	if err != nil {
		return 0
	}

	// Which revisions to compare. newRev "" means the index (pre-commit).
	var oldRev, newRev string
	switch name {
	case "post-merge":
		oldRev, newRev = "ORIG_HEAD", "HEAD"
	case "post-rewrite":
		if len(rest) > 0 && rest[0] != "rebase" {
			return 0 // amend: pre-commit already reported
		}
		oldRev, newRev = "ORIG_HEAD", "HEAD"
	case "post-checkout":
		if len(rest) < 3 || rest[2] != "1" || rest[0] == rest[1] {
			return 0 // file checkout, or same commit
		}
		if strings.Trim(rest[0], "0") == "" {
			return 0 // fresh clone: nothing to compare with
		}
		oldRev, newRev = rest[0], rest[1]
	case "pre-commit":
		oldRev, newRev = "HEAD", ""
		if _, err := gitOut(top, "rev-parse", "--verify", "-q", "HEAD"); err != nil {
			oldRev = "" // first commit
		}
	default:
		return 0
	}

	changed := changedManifests(top, oldRev, newRev)
	if len(changed) == 0 {
		return 0
	}

	diffs, indirect := manifestDiff(top, oldRev, newRev, changed)

	// Lifecycle of the whole project as it is now.
	pr, err := scanProject(top, 4)
	if err != nil {
		return 0
	}
	reg := NewRegistry(false)
	reg.EnrichLifecycle(pr)

	fmt.Println()
	fmt.Printf("%s %s\n", col(cBold, "stackscan ·"), col(cDim, strings.Join(changed, ", ")+" changed"))
	if len(diffs) > 0 {
		for _, d := range diffs {
			fmt.Println("  " + d)
		}
	}
	if indirect > 0 {
		fmt.Println(col(cDim, fmt.Sprintf("  + %d indirect dependency change(s)", indirect)))
	}
	if len(diffs) == 0 && indirect == 0 {
		fmt.Println(col(cDim, "  no version change"))
	}

	// Pull / checkout: remind to install what the lock files now require.
	if name != "pre-commit" {
		for _, h := range installHints(changed) {
			fmt.Println(col(cCyan, "  → "+h))
		}
	}

	renderBrief(pr)
	fmt.Println()

	if strict && name == "pre-commit" {
		introduced := eolIntroduced(pr, diffKeys)
		if len(introduced) > 0 {
			fmt.Println(col(cRed, "Commit blocked by stackscan: this commit brings in end-of-life versions: "+strings.Join(introduced, ", ")))
			fmt.Println(col(cDim, "Use git commit --no-verify, or STACKSCAN_SKIP=1, to commit anyway."))
			return 1
		}
	}
	return 0
}

// changedManifests lists the manifest files that differ between two
// revisions (newRev "" = the index).
func changedManifests(top, oldRev, newRev string) []string {
	var out string
	var err error
	switch {
	case newRev == "" && oldRev == "":
		out, err = gitOut(top, "diff", "--cached", "--name-only", "--root")
		if err != nil {
			out, err = gitOut(top, "ls-files")
		}
	case newRev == "":
		out, err = gitOut(top, "diff", "--cached", "--name-only", oldRev)
	default:
		out, err = gitOut(top, "diff", "--name-only", oldRev, newRev)
	}
	if err != nil {
		return nil
	}
	var files []string
	for _, f := range strings.Split(out, "\n") {
		if f == "" || !isManifest(filepath.Base(f)) || ignoredPath(f) {
			continue
		}
		files = append(files, f)
	}
	return files
}

func ignoredPath(p string) bool {
	parts := strings.Split(filepath.ToSlash(p), "/")
	for i, part := range parts[:len(parts)-1] {
		if skipDirs[part] || strings.HasPrefix(part, ".") {
			return true
		}
		if i > 0 && isBundled(strings.Join(parts[:i+1], "/")) {
			return true
		}
	}
	return false
}

// componentsAt parses the manifests of the given folders as they are at a
// revision (rev "" = the index), by copying them to a temporary folder.
func componentsAt(top, rev string, dirs []string) []Component {
	if rev == "-" {
		return nil
	}
	tmp, err := os.MkdirTemp("", "stackscan-")
	if err != nil {
		return nil
	}
	defer os.RemoveAll(tmp)
	var files []string
	for _, d := range dirs {
		var list string
		prefix := ""
		if d != "." {
			prefix = d + "/"
		}
		switch {
		case rev == "" && prefix == "":
			list, _ = gitOut(top, "ls-files")
		case rev == "":
			list, _ = gitOut(top, "ls-files", "--", prefix)
		case prefix == "":
			list, _ = gitOut(top, "ls-tree", "--name-only", rev)
		default:
			list, _ = gitOut(top, "ls-tree", "--name-only", rev, "--", prefix)
		}
		for _, f := range strings.Split(list, "\n") {
			if f == "" || filepath.Dir(f) != d || !isManifest(filepath.Base(f)) {
				continue
			}
			cmd := exec.Command("git", "show", rev+":"+f)
			cmd.Dir = top
			b, err := cmd.Output()
			if err != nil {
				continue
			}
			dst := filepath.Join(tmp, f)
			os.MkdirAll(filepath.Dir(dst), 0o755)
			os.WriteFile(dst, b, 0o644)
			files = append(files, dst)
		}
	}
	pr := &Project{Path: tmp}
	parseProject(pr, files)
	return pr.Components
}

// diffKeys holds the components whose version changed, filled by manifestDiff
// and read by the --strict check.
var diffKeys = map[string]bool{}

func compKey(c Component) string {
	file, _, _ := strings.Cut(c.Source, " (")
	return c.Eco + "|" + c.Name + "|" + filepath.Dir(file)
}

func manifestDiff(top, oldRev, newRev string, changed []string) ([]string, int) {
	dirSet := map[string]bool{}
	for _, f := range changed {
		dirSet[filepath.Dir(f)] = true
	}
	var dirs []string
	for d := range dirSet {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	or := oldRev
	if or == "" {
		or = "-"
	}
	before := index(componentsAt(top, or, dirs))
	after := index(componentsAt(top, newRev, dirs))

	var lines []string
	indirect := 0
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	var sorted []string
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		a, inA := before[k]
		b, inB := after[k]
		c := b
		if !inB {
			c = a
		}
		if inA && inB && a.Version == b.Version {
			continue
		}
		if c.Eco == "runtime" && c.FromImage {
			continue // reported through its image line
		}
		if !c.Direct {
			indirect++
			continue
		}
		diffKeys[k] = true
		where := filepath.Dir(strings.SplitN(c.Source, " (", 2)[0])
		label := c.Name
		if c.Eco == "image" {
			label = "image " + c.Name
		}
		if len(dirs) > 1 && where != "." {
			label += col(cDim, " ("+where+")")
		}
		switch {
		case !inA:
			lines = append(lines, col(cGreen, "+ ")+label+" "+b.Version)
		case !inB:
			lines = append(lines, col(cRed, "− ")+label+col(cDim, " "+a.Version))
		default:
			arrow := "→"
			if cmpVersion(b.Version, a.Version) < 0 {
				arrow = col(cYellow, "↓")
			}
			lines = append(lines, "  "+label+" "+col(cDim, a.Version)+" "+arrow+" "+b.Version)
		}
	}
	return lines, indirect
}

func index(cs []Component) map[string]Component {
	m := map[string]Component{}
	for _, c := range cs {
		if c.Declared {
			// A lock file gives the real version; skip constraints when the
			// same package also comes from a lock.
			if _, ok := m[compKey(c)]; ok {
				continue
			}
		}
		m[compKey(c)] = c
	}
	return m
}

func installHints(changed []string) []string {
	var hints []string
	seen := map[string]bool{}
	for _, f := range changed {
		dir := filepath.Dir(f)
		var h string
		switch filepath.Base(f) {
		case "composer.lock":
			h = "composer install"
		case "package-lock.json":
			h = "npm install"
		case "go.mod":
			h = "go mod download"
		default:
			if strings.Contains(filepath.Base(f), "ockerfile") || strings.Contains(filepath.Base(f), "compose") {
				h = "docker compose up -d --build"
			}
		}
		if h == "" {
			continue
		}
		if dir != "." && !strings.HasPrefix(h, "docker") {
			h = "cd " + dir + " && " + h
		}
		if !seen[h] {
			seen[h] = true
			hints = append(hints, h)
		}
	}
	return hints
}

// eolIntroduced lists the end-of-life components touched by the commit.
func eolIntroduced(pr *Project, changed map[string]bool) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range pr.Components {
		if c.Status != "eol" {
			continue
		}
		k := compKey(c)
		if c.FromImage {
			// The image line carries the change.
			repo := strings.TrimSuffix(strings.SplitN(c.Source, " (", 2)[1], ")")
			k = "image|" + repo + "|" + filepath.Dir(strings.SplitN(c.Source, " (", 2)[0])
		}
		if changed[k] && !seen[c.Name+c.Version] {
			seen[c.Name+c.Version] = true
			out = append(out, c.Name+" "+c.Version)
		}
	}
	return out
}
