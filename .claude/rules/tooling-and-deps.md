---
paths:
  - "go.mod"
  - "go.sum"
  - ".golangci.yml"
  - ".github/**"
  - "Makefile"
  - "sqlc.yaml"
---

# Dependencies, lint, and CI

- After changing `go.mod`, run `go mod tidy`; CI checks `go mod tidy -diff`.
- Before upgrading a module, inspect the target version's `go.mod` Go directive. If it raises the repository's minimum version, also update:
  - both READMEs and CI;
  - golangci-lint in `.github/workflows/ci.yml` and `AGENTS.md`: it must be built with Go >= the repository's Go directive.
- If govulncheck flags a module, upgrade to its reported "Fixed in" version and handle the consequences above.
- `.golangci.yml`:
  - do not disable linters or add exclusions to bypass a specific error; fix the code;
  - module path changes must also update depguard package paths (`grep -c github.com/example/userapi .golangci.yml` counts them; stale paths silently disable rules);
  - after changing depguard rules, temporarily add a violating import, confirm lint rejects it, then remove it. Import a **real** package, or typecheck fails before depguard runs;
  - `forbidigo` and `gochecknoglobals` apply only to `internal/core` (`path-except` exclusion); this defines scope rather than bypassing lint.
- sqlc is a code generator, not a module dependency: its version is pinned once in the Makefile (`SQLC`) and run with `go run` by `make sqlc-generate`/`make sqlc-check` locally and in CI. Do not add it as a `tool` in `go.mod` (its dependency graph would enter `go.mod` and could raise shared runtime dependencies). To upgrade, check the release's Go directive as for modules, change `SQLC`, run `make sqlc-generate`, and review the generated diff. Dependabot does not see it: check for sqlc releases when upgrading pgx or Go.
- CI runs for every PR, pushes to `main`, and weekly (govulncheck); all jobs must pass before merge.
- Pin `.github/workflows` actions by commit SHA with a version comment (`@<sha> # v7.0.1`). Choose releases at least 2 weeks old and update both SHA and comment. Dependabot (`.github/dependabot.yml`) proposes these upgrades.
- CI uses the latest Go `1.26.x` (`check-latest`), matching `golang:1.26-alpine` in Dockerfile. The `go.mod` directive is the minimum, not the exact CI version.
