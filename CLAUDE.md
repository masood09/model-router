# CLAUDE.md

@AGENTS.md

## Claude-specific notes

- Run `gofmt -l .`, `go vet ./...` and `go test -race ./...` before declaring a
  change done, and report failures with their output.
- This repository is public. Before committing, check the diff for anything
  environment-specific (hostnames, domains, non-documentation IPs, usernames,
  home paths); see the rules in AGENTS.md.
