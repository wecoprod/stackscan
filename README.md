# stackscan

See at a glance which versions a project runs on, what the latest releases
are, and what is reaching end of life.

```
$ cd my-project && stackscan

Runtimes and services
┌───┬────────────┬─────────┬────────┬─────────────┬────────────────┬─────────────────────────────────┐
│   │ Component  │ Version │ Latest │ End of life │ Status         │ Source                          │
├───┼────────────┼─────────┼────────┼─────────────┼────────────────┼─────────────────────────────────┤
│ ✗ │ nodejs     │ 18 *    │ 24     │ 2025-04-30  │ end of life    │ Dockerfile (node)               │
│ ! │ postgresql │ 14 *    │ 18     │ 2026-11-12  │ EOL in 16 days │ compose.yaml (postgres)         │
│ · │ php        │ >=8.2   │ 8.5    │ 2026-12-31  │ raise minimum  │ composer.json                   │
│ ✓ │ php        │ 8.4 *   │ 8.5    │ 2028-12-31  │ up to date     │ Dockerfile (dunglas/frankenphp) │
└───┴────────────┴─────────┴────────┴─────────────┴────────────────┴─────────────────────────────────┘

Composer packages (4 direct)
┌───┬──────────────────────────┬───────────┬────────┬─────────────┬─────────────┐
│   │ Package                  │ Installed │ Latest │ End of life │ Status      │
├───┼──────────────────────────┼───────────┼────────┼─────────────┼─────────────┤
│ ✗ │ symfony/framework-bundle │ v7.0.4    │ 8.1    │ 2024-07-31  │ end of life │
└───┴──────────────────────────┴───────────┴────────┴─────────────┴─────────────┘
+ 3 up to date (-v to list them)

Summary: 2 end of life · 1 reaching end of life · 1 minimum constraint(s) to raise
```

A single static binary, no runtime, no config. It reads the files already in
your repository and asks public registries for the latest versions.

## Install

**Prebuilt binary** (macOS and Linux): download the archive for your platform
from the [releases page](https://github.com/wecoprod/stackscan/releases),
then put `stackscan` somewhere on your `PATH`:

```
tar xzf stackscan_*_darwin_arm64.tar.gz
mkdir -p ~/.local/bin && mv stackscan ~/.local/bin/
xattr -d com.apple.quarantine ~/.local/bin/stackscan   # macOS only
```

**With Go** (1.22+):

```
go install github.com/wecoprod/stackscan@latest
```

## What it reads

| File | What it finds |
|---|---|
| `composer.json`, `composer.lock` | required PHP version, installed packages |
| `package.json`, `package-lock.json` | required Node version (`engines`), installed packages |
| `Dockerfile*` | `FROM` images, with `ARG` defaults resolved |
| `compose*.yml`, `docker-compose*.yml` | service images, with `${VAR:-default}` resolved |
| `go.mod`, `.nvmrc`, `.php-version`, `.tool-versions` | runtime versions |

`vendor/`, `node_modules/`, `var/` and nested git repositories (worktrees
included) are skipped.

Images are mapped to their product: `dunglas/frankenphp:1-php8.4` is PHP 8.4,
`postgres:16-alpine` is PostgreSQL 16, and so on.

## Where the reference data comes from

- **End of life**: [endoflife.date](https://endoflife.date) — PHP, Symfony,
  Laravel, Node.js, PostgreSQL, MySQL, MariaDB, Redis, Debian, Alpine, Python,
  WordPress and more.
- **Latest versions**: Packagist, the npm registry, the Go module proxy.

Responses are cached for 24 hours. With no network and no cache, a small
built-in dataset covers PHP, Symfony, Node.js, PostgreSQL, MySQL and Python.

## Options

```
-brief     short report: only what is at or near end of life
-v         also list up-to-date packages
-all       include indirect (transitive) dependencies
-json      JSON output, to aggregate several projects elsewhere
-offline   no network: local cache and built-in data only
-fail      exit with code 2 when something is end of life (for CI)
-depth N   maximum directory depth to search (default 4)
-no-color  disable colors (also honours NO_COLOR)
-version   print the version
```

## Reading the output

stackscan states facts and never picks a target for you:

- **Latest** is the newest release that is still supported.
- **Latest LTS** is the newest supported LTS, for products that publish LTS
  lines (Node.js, MySQL, Symfony, Composer…). Whether it is the right target
  depends on the product: MySQL's LTS is the production line, Composer's 2.2
  LTS is a legacy branch for old PHP.
- **Status** only looks at the end of life of the version you use, and flags
  a newer major release.


| Mark | Meaning |
|---|---|
| `✗` | end of life has passed |
| `!` | end of life within 6 months |
| `↑` | a new major version is out |
| `·` | minor update available, or a `>=` constraint whose minimum is reaching end of life |
| `✓` | up to date |
| `?` | unknown |

Every project gets the same columns. On a narrow terminal, `Source` is hidden
first, then `Latest LTS`, then long cells are shortened.

A `*` after a version marks a **floating** reference: it can change without a commit (`redis:7`,
`latest`…). stackscan reports what the repository **declares**, which is not
necessarily what runs in production — check managed databases and running
containers separately.

## Git hook

See what a commit changes in your dependencies, right when you commit:

```
cd my-project && stackscan hook install
stackscan hook install --all ~/Development     # every repository under a folder
```

Every `git commit` checks the project, but the hook only speaks when there is
something new:

- **the commit touches a manifest** (`composer.lock`, `package.json`,
  Dockerfile, compose files…): what moved, and what is at or near end of life;
- **a version reached or neared its end of life** since the last check, even
  without any file change (PostgreSQL 14 on 12 November, say): just that line;
- **otherwise**, at most one reminder line a day, or nothing at all.

```
stackscan · app/composer.lock, compose.yaml changed
    symfony/framework-bundle v7.0.4 → v7.4.16
    image postgres 17-alpine ↓ 12-alpine
  + 2 indirect dependency change(s)
  ✗ postgresql 12 — end of life since 2024-11-21 · latest 18  compose.yaml
```

- The hook **never blocks the commit** and does nothing if `stackscan` is not
  found.
- `--strict` refuses a commit that *brings in* an end-of-life version
  (versions already there do not block). Bypass with `git commit --no-verify`.
- `--pull` also reports after `git pull`, a branch switch or a rebase, with a
  reminder to run `composer install` / `npm install`.
- Existing hooks are kept: stackscan adds a marked block and
  `stackscan hook uninstall` removes only that block.
- With `core.hooksPath` (Husky, CaptainHook…), stackscan prints the line to
  add to your own pre-commit hook instead of touching it.
- `STACKSCAN_SKIP=1` silences the hook for one command.

The same short report is available on demand with `stackscan -brief`.

## Use in CI

```yaml
# .gitlab-ci.yml
stackscan:
  image: golang:1.24
  script:
    - go install github.com/wecoprod/stackscan@latest
    - stackscan -fail
  allow_failure: true
```

## Build from source

```
go build -o stackscan .
```

Releases are built by [GoReleaser](https://goreleaser.com) when a `v*` tag is
pushed.

## License

MIT — see [LICENSE](LICENSE).
