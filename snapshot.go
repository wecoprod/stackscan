package main

// snapshotCycles is a built-in fallback used only when endoflife.date cannot
// be reached and nothing is cached. It covers the products most likely to
// matter; EOL is the end of security support. Refresh it from
// https://endoflife.date when it ages.
var snapshotCycles = map[string][]Cycle{
	"php": {
		{Cycle: "8.5", EOL: "2029-12-31"}, {Cycle: "8.4", EOL: "2028-12-31"},
		{Cycle: "8.3", EOL: "2027-12-31"}, {Cycle: "8.2", EOL: "2026-12-31"},
		{Cycle: "8.1", EOL: "2025-12-31"}, {Cycle: "8.0", EOL: "2023-11-26"},
		{Cycle: "7.4", EOL: "2022-11-28"},
	},
	"symfony": {
		{Cycle: "8.1", EOL: "2027-01-31"}, {Cycle: "8.0", EOL: "2026-07-31"},
		{Cycle: "7.4", EOL: "2029-11-30"}, {Cycle: "7.3", EOL: "2026-01-31"},
		{Cycle: "7.2", EOL: "2025-07-31"}, {Cycle: "7.1", EOL: "2025-01-31"},
		{Cycle: "7.0", EOL: "2024-07-31"}, {Cycle: "6.4", EOL: "2027-11-30"},
		{Cycle: "5.4", EOL: "2025-02-28"},
	},
	"nodejs": {
		{Cycle: "24", EOL: "2028-04-30"}, {Cycle: "22", EOL: "2027-04-30"},
		{Cycle: "20", EOL: "2026-04-30"}, {Cycle: "18", EOL: "2025-04-30"},
		{Cycle: "17", EOL: "2022-06-01"}, {Cycle: "16", EOL: "2023-09-11"},
	},
	"postgresql": {
		{Cycle: "18", EOL: "2030-11-14"}, {Cycle: "17", EOL: "2029-11-08"},
		{Cycle: "16", EOL: "2028-11-09"}, {Cycle: "15", EOL: "2027-11-11"},
		{Cycle: "14", EOL: "2026-11-12"}, {Cycle: "13", EOL: "2025-11-13"},
		{Cycle: "12", EOL: "2024-11-21"},
	},
	"mysql": {
		{Cycle: "8.4", EOL: "2032-04-30"}, {Cycle: "8.0", EOL: "2026-04-30"},
		{Cycle: "5.7", EOL: "2023-10-31"},
	},
	"python": {
		{Cycle: "3.13", EOL: "2029-10-31"}, {Cycle: "3.12", EOL: "2028-10-31"},
		{Cycle: "3.11", EOL: "2027-10-31"}, {Cycle: "3.10", EOL: "2026-10-31"},
	},
}
