package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// stackscan hook install adds a pre-commit hook that runs `stackscan -brief`
// on every commit. It never blocks the commit.

const (
	markBegin = "# >>> stackscan >>>"
	markEnd   = "# <<< stackscan <<<"
)

func hookMain(args []string) {
	if st, err := os.Stdout.Stat(); err != nil || st.Mode()&os.ModeCharDevice == 0 || os.Getenv("NO_COLOR") != "" {
		useColor = false
	}
	if len(args) == 0 || (args[0] != "install" && args[0] != "uninstall") {
		fmt.Fprint(os.Stderr, "Usage: stackscan hook install|uninstall\n\nAdds (or removes) a pre-commit hook that runs `stackscan -brief` on every commit.\nIt never blocks the commit.\n")
		os.Exit(1)
	}
	path, err := preCommitPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if args[0] == "install" {
		err = installHook(path)
	} else {
		err = uninstallHook(path)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func preCommitPath() (string, error) {
	git := func(a ...string) string {
		out, _ := exec.Command("git", a...).Output()
		return strings.TrimSpace(string(out))
	}
	top := git("rev-parse", "--show-toplevel")
	if top == "" {
		return "", fmt.Errorf("not a git repository")
	}
	if hp := git("config", "core.hooksPath"); hp != "" {
		return "", fmt.Errorf("hooks are managed by core.hooksPath (%s): add this line to your pre-commit hook:\n  command -v stackscan >/dev/null && stackscan -brief || true", hp)
	}
	dir := git("rev-parse", "--git-path", "hooks")
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(top, dir)
	}
	os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, "pre-commit"), nil
}

func installHook(path string) error {
	self, _ := os.Executable()
	// Prefer stackscan from PATH, fall back to the binary that installed the
	// hook (GUI git clients often run hooks with a minimal PATH). Never fail.
	block := fmt.Sprintf(`%s
S=$(command -v stackscan 2>/dev/null || echo %q)
[ -x "$S" ] && "$S" -brief || true
%s
`, markBegin, self, markEnd)

	content := "#!/bin/sh\n" + block
	if old, err := os.ReadFile(path); err == nil {
		// Keep the existing hook; add (or refresh) our block at the end.
		first := strings.SplitN(string(old), "\n", 2)[0]
		if !strings.HasPrefix(first, "#!") || !strings.Contains(first, "sh") {
			return fmt.Errorf("%s exists and is not a shell script; add `stackscan -brief || true` to it yourself", path)
		}
		s := removeBlock(string(old))
		if !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		content = s + block
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		return err
	}
	fmt.Println(col(cGreen, "✓") + " pre-commit hook installed: stackscan -brief runs on every commit")
	return nil
}

func uninstallHook(path string) error {
	old, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(old), markBegin) {
		fmt.Println("No stackscan hook found.")
		return nil
	}
	rest := removeBlock(string(old))
	if strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), "#!/bin/sh")) == "" {
		err = os.Remove(path)
	} else {
		err = os.WriteFile(path, []byte(rest), 0o755)
	}
	if err == nil {
		fmt.Println(col(cGreen, "✓") + " stackscan hook removed")
	}
	return err
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
