package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// renderBrief prints a short lifecycle report: only what has reached, or
// is about to reach, its end of life. It is what the git hooks show.
// It returns the number of end-of-life lines printed.
func renderBrief(pr *Project) int {
	return renderBriefOnly(pr, nil)
}

// briefKey identifies a line of the brief report, status included: a version
// going from "near end of life" to "end of life" counts as news.
func briefKey(c Component) string {
	return c.Eco + "|" + c.Name + "|" + c.Version + "|" + c.Status
}

// briefKeys lists what the brief report would show.
func briefKeys(pr *Project) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range pr.Components {
		if c.Eco == "image" || (c.Status != "eol" && c.Status != "eol-soon") {
			continue
		}
		k := briefKey(c)
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// renderBriefOnly prints the brief report, restricted to the given keys
// when only is not nil (and then without the summary lines).
func renderBriefOnly(pr *Project, only map[string]bool) int {
	type line struct {
		c       Component
		sources []string
	}
	byKey := map[string]*line{}
	var keys []string
	majors := 0
	for _, c := range pr.Components {
		if c.Eco == "image" || c.Status == "" {
			continue
		}
		if c.Status == "major" && c.Eco == "runtime" {
			majors++
		}
		if c.Status != "eol" && c.Status != "eol-soon" {
			continue
		}
		if only != nil && !only[briefKey(c)] {
			continue
		}
		k := c.Eco + "|" + c.Name + "|" + c.Version
		if l, ok := byKey[k]; ok {
			l.sources = append(l.sources, c.Source)
			continue
		}
		byKey[k] = &line{c: c, sources: []string{c.Source}}
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := byKey[keys[i]].c, byKey[keys[j]].c
		if a.Status != b.Status {
			return a.Status == "eol"
		}
		return keys[i] < keys[j]
	})

	for _, k := range keys {
		l := byKey[k]
		c := l.c
		var when string
		if c.EOL == "0001-01-01" {
			when = col(cRed, "end of life")
		} else if c.Status == "eol" {
			when = col(cRed, "end of life since "+c.EOL)
		} else {
			d, _ := time.Parse("2006-01-02", c.EOL)
			when = col(cYellow, fmt.Sprintf("end of life on %s (in %d days)", c.EOL, int(time.Until(d).Hours()/24)+1))
		}
		var next []string
		if c.Latest != "" {
			next = append(next, "latest "+c.Latest)
		}
		if c.LatestLTS != "" {
			next = append(next, "LTS "+c.LatestLTS)
		}
		extra := ""
		if len(next) > 0 {
			extra = col(cDim, " · "+strings.Join(next, " · "))
		}
		fmt.Printf("  %s %s %s — %s%s  %s\n", icon(c.Status), col(cBold, c.Name), c.Version, when, extra,
			col(cDim, shortSources(uniq(l.sources))))
	}
	if only != nil {
		return len(keys)
	}
	if len(keys) == 0 {
		fmt.Println(col(cGreen, "  ✓ nothing at or near end of life"))
	}
	if majors > 0 {
		fmt.Println(col(cDim, fmt.Sprintf("  %d runtime(s) have a newer major version — run stackscan for details", majors)))
	}
	return len(keys)
}
