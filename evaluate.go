package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// stackscan evaluate computes statuses for a list of versions, with exactly
// the same logic as a project scan. stackscan cloud calls it once a day on
// every distinct version in use, so there is a single source of truth.
//
//	echo '[{"eco":"runtime","name":"postgresql","version":"14"}]' | stackscan evaluate

type evalInput struct {
	Eco      string `json:"eco"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	Declared bool   `json:"declared,omitempty"`
}

type evalOutput struct {
	Eco       string `json:"eco"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Declared  bool   `json:"declared,omitempty"`
	Product   string `json:"product,omitempty"`    // endoflife.date product, when tracked
	Cycle     string `json:"cycle,omitempty"`      // release cycle the version belongs to
	Latest    string `json:"latest,omitempty"`     // newest supported release
	LatestLTS string `json:"latest_lts,omitempty"` // newest supported LTS
	EOL       string `json:"eol,omitempty"`        // YYYY-MM-DD; 0001-01-01 = ended, date unknown
	Status    string `json:"status"`               // eol | eol-soon | major | floor | minor | ok | unknown
}

const maxEvalItems = 100000

func evaluateMain(args []string) {
	fs := flag.NewFlagSet("evaluate", flag.ExitOnError)
	offline := fs.Bool("offline", false, "no network: local cache and built-in data only")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: stackscan evaluate [-offline] < versions.json

Reads a JSON array of {"eco","name","version"[,"declared"]} on standard input
and writes the same array on standard output with latest, latest_lts, eol and
status, computed like a project scan. eco: runtime, composer, npm or go.
`)
		fs.PrintDefaults()
	}
	fs.Parse(args)

	data, err := io.ReadAll(io.LimitReader(os.Stdin, 64<<20))
	if err != nil {
		fail("evaluate: cannot read input: %v", err)
	}
	var in []evalInput
	if err := json.Unmarshal(data, &in); err != nil {
		fail("evaluate: input must be a JSON array: %v", err)
	}
	if len(in) > maxEvalItems {
		fail("evaluate: at most %d items per call", maxEvalItems)
	}

	pr := &Project{Components: make([]Component, 0, len(in))}
	for i, it := range in {
		it.Eco = strings.TrimSpace(it.Eco)
		if it.Name == "" || it.Version == "" {
			fail("evaluate: item %d needs a name and a version", i)
		}
		switch it.Eco {
		case "runtime", "composer", "npm", "go":
		default:
			fail("evaluate: item %d has an unsupported eco %q", i, it.Eco)
		}
		pr.Components = append(pr.Components, Component{
			Eco: it.Eco, Name: it.Name, Version: it.Version, Declared: it.Declared, Direct: true,
		})
	}

	reg := NewRegistry(*offline)
	reg.Enrich(pr, true)

	out := make([]evalOutput, len(pr.Components))
	for i, c := range pr.Components {
		out[i] = evalOutput{
			Eco: c.Eco, Name: c.Name, Version: c.Version, Declared: c.Declared,
			Product: c.Product, Cycle: c.Cycle, Latest: c.Latest, LatestLTS: c.LatestLTS,
			EOL: c.EOL, Status: c.Status,
		}
		if out[i].Status == "" {
			out[i].Status = "unknown"
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		fail("evaluate: %v", err)
	}
	if u := reg.Unreachable(); len(u) > 0 {
		fmt.Fprintln(os.Stderr, "evaluate: unreachable: "+strings.Join(u, ", ")+" (built-in data used where possible)")
	}
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
