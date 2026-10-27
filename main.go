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
	"runtime/debug"
	"sort"
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
	all := flag.Bool("all", false, "include indirect (transitive) dependencies")
	verbose := flag.Bool("v", false, "also list up-to-date packages")
	offline := flag.Bool("offline", false, "no network: use the local cache and built-in data only")
	asJSON := flag.Bool("json", false, "JSON output")
	noColor := flag.Bool("no-color", false, "disable colors")
	fail := flag.Bool("fail", false, "exit with code 2 when something is end-of-life (for CI)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	depth := flag.Int("depth", 4, "maximum directory depth to search for manifests")
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: stackscan [options] [dir]\n\nScans a project (current directory by default) and compares its versions\nwith the latest releases and end-of-life dates.\n\nOptions:\n")
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
	reg.Enrich(pr, *all)

	if *asJSON {
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
		var rows [][]string
		for _, k := range keys {
			l := rt[k]
			counts[l.c.Status]++
			ver := l.c.Version
			if l.c.Declared && strings.ContainsAny(ver, "<>=^~*|") {
				ver = ver + " (constraint)"
			}
			float := ""
			if l.float {
				float = col(cYellow, "yes")
			}
			rows = append(rows, []string{icon(l.c.Status), l.c.Name, ver, latestText(l.c), eolText(l.c), statusText(l.c), float, col(cDim, strings.Join(uniq(l.sources), ", "))})
		}
		printTable([]string{"", "Component", "Version", "Latest", "End of life", "Status", "Floating", "Source"}, rows)
	}

	// ---- packages per ecosystem ----
	ecoTitles := []struct{ eco, title string }{{"composer", "Composer packages"}, {"npm", "npm packages"}, {"go", "Go modules"}}
	for _, e := range ecoTitles {
		var list []Component
		hidden := 0
		total := 0
		for _, c := range pr.Components {
			if c.Eco != e.eco || (!all && !c.Direct) {
				continue
			}
			total++
			counts[c.Status]++
			if !verbose && (c.Status == "ok" || c.Status == "unknown") && c.Product == "" {
				hidden++
				continue
			}
			list = append(list, c)
		}
		if total == 0 {
			continue
		}
		scope := "direct"
		if all {
			scope = "all"
		}
		section(fmt.Sprintf("%s (%d %s)", e.title, total, scope))
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
			rows = append(rows, []string{icon(c.Status), name, ver, latestText(c), eolText(c), statusText(c)})
		}
		printTable([]string{"", "Package", "Installed", "Latest", "End of life", "Status"}, rows)
		if hidden > 0 {
			fmt.Println(col(cDim, fmt.Sprintf("+ %d up to date (-v to list them)", hidden)))
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

// printTable draws a bordered table. Cells may contain ANSI colours.
func printTable(headers []string, rows [][]string) {
	if len(rows) == 0 {
		return
	}
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = visibleLen(h)
	}
	for _, r := range rows {
		for i, cell := range r {
			if w := visibleLen(cell); w > widths[i] {
				widths[i] = w
			}
		}
	}
	line := func(l, mid, r string) string {
		var b strings.Builder
		b.WriteString(l)
		for i, w := range widths {
			b.WriteString(strings.Repeat("─", w+2))
			if i < len(widths)-1 {
				b.WriteString(mid)
			}
		}
		b.WriteString(r)
		return col(cDim, b.String())
	}
	bar := col(cDim, "│")
	rowStr := func(cells []string) string {
		var b strings.Builder
		b.WriteString(bar)
		for i, cell := range cells {
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
