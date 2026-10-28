// stackscan lists the versions used by a project (runtimes, Docker images,
// composer / npm / go packages) and compares them with the latest releases
// and end-of-life dates.
//
//	cd my-project && stackscan
//	stackscan -all ~/Development/wecoprod/birdie-repo
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var useColor = true

// version is set at build time: -ldflags "-X main.version=v1.0.0"
var version = "dev"

const (
	cReset  = "\033[0m"
	cBold   = "\033[1m"
	cDim    = "\033[2m"
	cRed    = "\033[31m"
	cYellow = "\033[33m"
	cGreen  = "\033[32m"
	cCyan   = "\033[36m"
)

func col(code, s string) string {
	if !useColor || s == "" {
		return s
	}
	return code + s + cReset
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hook" {
		hookMain(os.Args[2:])
		return
	}
	all := flag.Bool("all", false, "include indirect (transitive) dependencies")
	verbose := flag.Bool("v", false, "also list up-to-date packages")
	offline := flag.Bool("offline", false, "no network: use the local cache and built-in data only")
	asJSON := flag.Bool("json", false, "JSON output")
	noColor := flag.Bool("no-color", false, "disable colors")
	fail := flag.Bool("fail", false, "exit with code 2 when something is end-of-life (for CI)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	brief := flag.Bool("brief", false, "short report: only what is at or near end of life (fast)")
	depth := flag.Int("depth", 4, "maximum directory depth to search for manifests")
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: stackscan [options] [dir]\n       stackscan hook install|uninstall [--strict] [--all dir]\n\nScans a project (current directory by default) and compares its versions\nwith the latest releases and end-of-life dates.\n\nOptions:\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		v := version
		if v == "dev" {
			// go install github.com/wecoprod/stackscan@v0.1.0 records the module version.
			if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
				v = bi.Main.Version
			}
		}
		fmt.Println("stackscan", v)
		return
	}

	dir := "."
	if flag.NArg() > 0 {
		dir = flag.Arg(0)
	}
	if st, err := os.Stdout.Stat(); *noColor || os.Getenv("NO_COLOR") != "" || err != nil || st.Mode()&os.ModeCharDevice == 0 {
		useColor = false
	}

	pr, err := scanProject(dir, *depth)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if len(pr.Components) == 0 {
		fmt.Fprintln(os.Stderr, "No composer.json, package.json, Dockerfile, compose file or go.mod found in", pr.Path)
		os.Exit(1)
	}

	reg := NewRegistry(*offline)
	if *brief {
		reg.EnrichLifecycle(pr)
		fmt.Println(col(cBold, pr.Name))
		if renderBrief(pr) == 0 {
			fmt.Println(col(cGreen, "  ✓ nothing at or near end of life"))
		}
	} else {
		reg.Enrich(pr, *all)
	}

	if *brief {
		// already printed
	} else if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(pr)
	} else {
		render(pr, reg, *all, *verbose)
	}

	if *fail {
		for _, c := range pr.Components {
			if c.Status == "eol" {
				os.Exit(2)
			}
		}
	}
}

var severity = map[string]int{"eol": 0, "eol-soon": 1, "major": 2, "floor": 3, "minor": 4, "unknown": 5, "ok": 6, "": 7}

func icon(status string) string {
	switch status {
	case "eol":
		return col(cRed, "✗")
	case "eol-soon":
		return col(cYellow, "!")
	case "major":
		return col(cYellow, "↑")
	case "minor", "floor":
		return col(cCyan, "·")
	case "ok":
		return col(cGreen, "✓")
	}
	return col(cDim, "?")
}

func statusText(c Component) string {
	switch c.Status {
	case "eol":
		return col(cRed, "end of life")
	case "eol-soon":
		d, _ := time.Parse("2006-01-02", c.EOL)
		return col(cYellow, fmt.Sprintf("EOL in %d days", int(time.Until(d).Hours()/24)+1))
	case "major":
		return col(cYellow, "new major")
	case "minor":
		return col(cCyan, "update available")
	case "floor":
		return col(cCyan, "raise minimum")
	case "ok":
		return col(cGreen, "up to date")
	}
	return col(cDim, "unknown")
}

func eolText(c Component) string {
	switch {
	case c.EOL == "":
		return col(cDim, "—")
	case c.EOL == "0001-01-01":
		return col(cRed, "passed")
	case c.Status == "eol":
		return col(cRed, fmtDate(c.EOL))
	case c.Status == "eol-soon":
		return col(cYellow, fmtDate(c.EOL))
	}
	return fmtDate(c.EOL)
}

func fmtDate(d string) string {
	t, err := time.Parse("2006-01-02", d)
	if err != nil {
		return d
	}
	return t.Format("2006-01-02")
}

func render(pr *Project, reg *Registry, all, verbose bool) {
	fmt.Println()
	fmt.Print(col(cBold, pr.Name))
	var meta []string
	if pr.Remote != "" {
		meta = append(meta, pr.Remote)
	}
	if pr.LastCommit != "" {
		meta = append(meta, "last commit "+fmtDate(pr.LastCommit))
	}
	if len(meta) > 0 {
		fmt.Print(col(cDim, "  "+strings.Join(meta, " · ")))
	}
	fmt.Println()

	counts := map[string]int{}

	// ---- runtimes: one line per (product, version), sources merged ----
	type rtLine struct {
		c       Component
		sources []string
		float   bool
	}
	rt := map[string]*rtLine{}
	var keys []string
	for _, c := range pr.Components {
		if c.Eco != "runtime" {
			continue
		}
		k := c.Name + "|" + c.Version
		if l, ok := rt[k]; ok {
			l.sources = append(l.sources, c.Source)
			l.float = l.float || c.Floating
			continue
		}
		rt[k] = &rtLine{c: c, sources: []string{c.Source}, float: c.Floating}
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := rt[keys[i]].c, rt[keys[j]].c
		if severity[a.Status] != severity[b.Status] {
			return severity[a.Status] < severity[b.Status]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > 0 {
		section("Runtimes and services")
		perProduct := map[string]int{}
		for _, k := range keys {
			perProduct[rt[k].c.Name]++
		}
		var rows [][]string
		anyFloat := false
		for _, k := range keys {
			l := rt[k]
			counts[l.c.Status]++
			ver := l.c.Version
			if l.float {
				ver += col(cYellow, " *")
				anyFloat = true
			}
			name := l.c.Name
			if perProduct[l.c.Name] > 1 {
				name += col(cDim, " ("+shortSources(l.sources)+")")
			}
			rows = append(rows, []string{icon(l.c.Status), name, ver, latestText(l.c), ltsText(l.c), eolText(l.c), statusText(l.c), col(cDim, strings.Join(uniq(l.sources), ", "))})
		}
		printTable([]string{"", "Component", "Version", "Latest", "Latest LTS", "End of life", "Status", "Source"}, rows, 4, 7)
		if anyFloat {
			fmt.Println(col(cDim, "* floating tag: can change without a commit (redis:7, latest...)"))
		}
	}

	// ---- packages: one table per ecosystem and folder ----
	// A project can hold several independent parts (app/, a WordPress plugin,
	// a front-end...): each folder with its own manifest gets its own table.
	ecoTitles := []struct{ eco, title string }{{"composer", "Composer packages"}, {"npm", "npm packages"}, {"go", "Go modules"}}
	for _, e := range ecoTitles {
		byDir := map[string][]Component{}
		for _, c := range pr.Components {
			if c.Eco != e.eco || (!all && !c.Direct) {
				continue
			}
			file, _, _ := strings.Cut(c.Source, " (")
			d := filepath.Dir(file)
			byDir[d] = append(byDir[d], c)
		}
		dirs := make([]string, 0, len(byDir))
		for d := range byDir {
			dirs = append(dirs, d)
		}
		sort.Strings(dirs)
		for _, d := range dirs {
			comps := byDir[d]
			var list []Component
			hidden := 0
			seen := map[string]bool{}
			total := 0
			for _, c := range comps {
				if seen[c.Name] {
					continue
				}
				seen[c.Name] = true
				total++
				counts[c.Status]++
				if !verbose && (c.Status == "ok" || c.Status == "unknown") && c.Product == "" {
					hidden++
					continue
				}
				list = append(list, c)
			}
			scope := "direct"
			if all {
				scope = "all"
			}
			title := fmt.Sprintf("%s (%d %s)", e.title, total, scope)
			if len(dirs) > 1 || d != "." {
				title = fmt.Sprintf("%s — %s (%d %s)", e.title, d, total, scope)
			}
			section(title)
			sort.Slice(list, func(i, j int) bool {
				if severity[list[i].Status] != severity[list[j].Status] {
					return severity[list[i].Status] < severity[list[j].Status]
				}
				return list[i].Name < list[j].Name
			})
			var rows [][]string
			for _, c := range list {
				name := c.Name
				if c.Dev {
					name += col(cDim, " (dev)")
				}
				ver := c.Version
				if c.Declared {
					ver += col(cDim, " (declared)")
				}
				rows = append(rows, []string{icon(c.Status), name, ver, latestText(c), ltsText(c), eolText(c), statusText(c)})
			}
			printTable([]string{"", "Package", "Installed", "Latest", "Latest LTS", "End of life", "Status"}, rows, 4, 5)
			if hidden > 0 {
				fmt.Println(col(cDim, fmt.Sprintf("+ %d up to date (-v to list them)", hidden)))
			}
		}
	}

	// ---- summary ----
	fmt.Println()
	var parts []string
	if n := counts["eol"]; n > 0 {
		parts = append(parts, col(cRed, fmt.Sprintf("%d end of life", n)))
	}
	if n := counts["eol-soon"]; n > 0 {
		parts = append(parts, col(cYellow, fmt.Sprintf("%d reaching end of life", n)))
	}
	if n := counts["major"]; n > 0 {
		parts = append(parts, col(cYellow, fmt.Sprintf("%d major version(s) behind", n)))
	}
	if n := counts["floor"]; n > 0 {
		parts = append(parts, col(cCyan, fmt.Sprintf("%d minimum constraint(s) to raise", n)))
	}
	if n := counts["minor"]; n > 0 {
		parts = append(parts, col(cCyan, fmt.Sprintf("%d minor update(s)", n)))
	}
	if len(parts) == 0 {
		parts = append(parts, col(cGreen, "everything is up to date"))
	}
	fmt.Println(col(cBold, "Summary: ") + strings.Join(parts, " · "))
	if u := reg.Unreachable(); len(u) > 0 {
		fmt.Println(col(cYellow, "Unreachable: "+strings.Join(u, ", ")+" (partial results; built-in end-of-life data used as fallback)."))
	}
	if reg.offline {
		fmt.Println(col(cDim, "Offline mode: local cache and built-in data."))
	}
	fmt.Println()
}

func latestText(c Component) string {
	if c.Latest == "" {
		return col(cDim, "—")
	}
	return c.Latest
}

func ltsText(c Component) string {
	if c.LatestLTS == "" {
		return col(cDim, "—")
	}
	return c.LatestLTS
}

// shortSources turns "app/composer.json (config.platform)" into
// "app/composer.json platform" and "compose.yaml (postgres)" into "compose.yaml".
func shortSources(sources []string) string {
	var out []string
	for _, s := range sources {
		file, detail, _ := strings.Cut(s, " (")
		label := file
		if strings.HasPrefix(detail, "config.platform") {
			label += " platform"
		} else if strings.HasPrefix(detail, "engines") {
			label += " engines"
		}
		out = append(out, label)
	}
	out = uniq(out)
	if len(out) > 1 {
		// The Source column lists them all; keep the label short.
		return fmt.Sprintf("%s +%d", out[0], len(out)-1)
	}
	return strings.Join(out, ", ")
}

func section(t string) {
	fmt.Println()
	fmt.Println(col(cBold, t))
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

var reANSI = strings.NewReplacer(cReset, "", cBold, "", cDim, "", cRed, "", cYellow, "", cGreen, "", cCyan, "")

func visibleLen(s string) int {
	return utf8.RuneCountInString(reANSI.Replace(s))
}

// termWidth returns the terminal width, or 0 when unknown (output piped).
func termWidth() int {
	if n, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && n > 0 {
		return n
	}
	if !useColor { // not a terminal, or colors disabled on purpose
		if st, err := os.Stdout.Stat(); err != nil || st.Mode()&os.ModeCharDevice == 0 {
			return 0
		}
	}
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return 0
	}
	defer tty.Close()
	cmd := exec.Command("stty", "size")
	cmd.Stdin = tty
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	f := strings.Fields(string(out))
	if len(f) != 2 {
		return 0
	}
	n, _ := strconv.Atoi(f[1])
	return n
}

var reEsc = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string { return reEsc.ReplaceAllString(s, "") }

// truncate shortens a cell to w visible characters, keeping its colour
// codes only when it already fits.
func truncate(s string, w int) string {
	if visibleLen(s) <= w {
		return s
	}
	r := []rune(stripANSI(s))
	if w <= 1 {
		return string(r[:w])
	}
	return string(r[:w-1]) + "…"
}

// printTable draws a bordered table that fits the terminal: the columns
// listed in drop are hidden (last listed first), then the widest cells are
// truncated.
func printTable(headers []string, rows [][]string, drop ...int) {
	if len(rows) == 0 {
		return
	}
	// Same columns for every project: a column is only hidden when the
	// terminal is too narrow, never because it happens to be empty.
	keep := make([]bool, len(headers))
	for i := range keep {
		keep[i] = true
	}
	widths := make([]int, len(headers))
	calc := func() int {
		total := 1
		for i, h := range headers {
			widths[i] = visibleLen(h)
			for _, r := range rows {
				if w := visibleLen(r[i]); w > widths[i] {
					widths[i] = w
				}
			}
			if keep[i] {
				total += widths[i] + 3
			}
		}
		return total
	}
	total := calc()
	max := termWidth()
	for j := len(drop) - 1; max > 0 && total > max && j >= 0; j-- {
		keep[drop[j]] = false
		total = calc()
	}
	// Still too wide: shrink the widest column, one character at a time.
	for max > 0 && total > max {
		wi := -1
		for i := range widths {
			if keep[i] && i > 0 && (wi < 0 || widths[i] > widths[wi]) {
				wi = i
			}
		}
		if wi < 0 || widths[wi] <= 6 {
			break
		}
		widths[wi]--
		total--
		for _, r := range rows {
			r[wi] = truncate(r[wi], widths[wi])
		}
		headers[wi] = truncate(headers[wi], widths[wi])
	}

	line := func(l, mid, r string) string {
		var b strings.Builder
		b.WriteString(l)
		first := true
		for i, w := range widths {
			if !keep[i] {
				continue
			}
			if !first {
				b.WriteString(mid)
			}
			first = false
			b.WriteString(strings.Repeat("─", w+2))
		}
		b.WriteString(r)
		return col(cDim, b.String())
	}
	bar := col(cDim, "│")
	rowStr := func(cells []string) string {
		var b strings.Builder
		b.WriteString(bar)
		for i, cell := range cells {
			if !keep[i] {
				continue
			}
			b.WriteString(" " + cell + strings.Repeat(" ", widths[i]-visibleLen(cell)) + " ")
			b.WriteString(bar)
		}
		return b.String()
	}
	fmt.Println(line("┌", "┬", "┐"))
	hs := make([]string, len(headers))
	for i, h := range headers {
		hs[i] = col(cBold, h)
	}
	fmt.Println(rowStr(hs))
	fmt.Println(line("├", "┼", "┤"))
	for _, r := range rows {
		fmt.Println(rowStr(r))
	}
	fmt.Println(line("└", "┴", "┘"))
}
