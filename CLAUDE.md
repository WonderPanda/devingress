# CLAUDE.md

devingress is a single Go binary with Caddy built in: one process owns :80/:443 and serves HTTPS
routes that each checkout registers from its `devingress.toml`. `README.md` describes the usage and
the route file format.

## Layout

- `cmd/devingress/`: the CLI (`serve`, `up`, `down`, `status`, `routes`, `caddyfile`, `trust`,
  `service`), each command's help text next to it. `devingress help routefile` lives in
  `internal/routefile/help.go`.
- `internal/routefile/`: parses `devingress.toml` and expands `${VARS}`.
- `internal/render/`: merges every owner's routes into one Caddy config.
- `internal/state/`: one JSON file per registered checkout, under `$XDG_STATE_HOME/devingress`.
- `internal/ingress/`: runs Caddy and reloads it through the admin socket.
- `internal/service/`: the systemd system unit `devingress service install` writes, and its commands.
- `internal/config/`: host settings (`~/.config/devingress/config.toml`) and the XDG paths.

## Commands

```sh
go build -o bin/devingress ./cmd/devingress
go test ./...
go test -tags e2e ./e2e/...   # builds the binary, runs an isolated serve on free ports, drives it over TLS
go vet ./...
go vet -tags e2e ./...
gofmt -l .               # must print nothing; `gofmt -w .` fixes it
```

Before opening a PR, `go build ./...`, `go test ./...`, `go test -tags e2e ./e2e/...`, `go vet ./...`
(also with `-tags e2e`) and `gofmt -l .` all pass.

## Hard rules

- Never run `systemctl` against `devingress`, and never stop, restart or signal a devingress you did
  not start: it serves every checkout's routes on this machine.
- Never run `devingress up`, `down` or `serve` against the real state, config or admin socket. To
  exercise them, point every XDG directory at a scratch dir and use unprivileged ports:
  ```sh
  d=$(mktemp -d)
  export XDG_STATE_HOME=$d/state XDG_CONFIG_HOME=$d/config XDG_DATA_HOME=$d/data XDG_RUNTIME_DIR=$d/run
  mkdir -p $XDG_CONFIG_HOME/devingress $XDG_RUNTIME_DIR
  printf 'bind = ["127.0.0.1"]\nhttp_port = 18080\nhttps_port = 18443\n' > $XDG_CONFIG_HOME/devingress/config.toml
  ```
- Never run `sudo` or `setcap`.
- Never run `devingress service install` or `uninstall` without `--print`: they run `sudo`,
  `systemctl` and `loginctl`.

## Pull requests

Commits and PR titles use conventional prefixes (`feat:`, `fix:`, `docs:`, `chore:`). Open the PR
with `gh pr create`; its body says what changed and why. CI (`.github/workflows/ci.yml`) runs
`go build`, `go vet`, `gofmt -l` and `go test` on every PR and on pushes to `main`, plus a
separate job that runs the e2e tests.
