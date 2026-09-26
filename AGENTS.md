# AGENTS.md

## Code Navigation

- Prefer language-server tooling for semantic code navigation when available.
- Use `ast-grep` (`sg`) for structural and syntax-aware code searches.
- Use `rg` (ripgrep) for text searches.
- Do not use `grep` when `rg` can perform the same task.

## Go

- Make not to use CGO unless absolutely required. You want the code to be as Golang native as possible.
- Use `gopls` MCP tools for definitions, references, symbols, type information, diagnostics, and semantic code navigation.
- Prefer `gopls` semantic information over text-based inference.
- Use `ast-grep` for structural searches not covered by `gopls`.
- Run `gofmt` on modified Go files.
- Run `gopls` diagnostics after modifying Go code. - Run relevant Go tests when practical.

## Front End

- Always use Vanilla Javascript with HTML and CSS native functinality
- Use HTMX v4.x when possible
- Always use Apache eCharts for charts, diagrams, etc.

## Database

- You must use SQLite3 and handle migrations with .sql files.

## Semantic Versioning

- There is a VERSION file containing the single source of truth for version handling

## Build and Deploy

- Building must be performed with Makefile
- Deployment is performed with Docker Compose

