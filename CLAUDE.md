# CLAUDE.md

devingress is a single Go binary with Caddy built in: one process owns :80/:443 and serves HTTPS
routes that each checkout registers from its `devingress.toml`. `README.md` describes the usage and
the route file format.

## Layout

- `cmd/devingress/`: the CLI (`serve`, `up`, `down`, `status`, `routes`, `doctor`, `caddyfile`,
  `trust`, `service`, `dns`, `version`), each command's help text next to it. `devingress help
  routefile` lives in `internal/routefile/help.go`.
- `internal/routefile/`: parses `devingress.toml` and expands `${VARS}`.
- `internal/render/`: merges every owner's routes into one Caddy config.
- `internal/state/`: one JSON file per registered owner (checkout plus `up --name`), under
  `$XDG_STATE_HOME/devingress`.
- `internal/ingress/`: runs Caddy and reloads it through the admin socket.
- `internal/service/`: the systemd system unit `devingress service install` writes on Linux, and its
  commands.
- `internal/launchd/`: the LaunchAgent `devingress service install` writes on macOS, and its
  launchctl commands.
- `internal/localdns/`: serve's DNS listener (A 127.0.0.1, AAAA ::1 for every name under a domain
  `devingress dns setup` routed to it, REFUSED for every other name).
- `internal/resolved/`: the systemd-resolved drop-in `devingress dns setup` writes on Linux, and its
  commands.
- `internal/macresolver/`: the `/etc/resolver/<domain>` file `devingress dns setup` writes on macOS,
  and its commands.
- `internal/ingress/lo0_darwin.go`: the Caddy network that lets a non-root serve bind loopback
  :80/:443 on macOS (wildcard address, socket bound to lo0). `config.Render` routes loopback bind
  addresses through it on darwin.
- `internal/config/`: host settings (`~/.config/devingress/config.toml`: `bind`, `http_port`,
  `https_port`, `ca_root`, `dns_listen`, `[remote]`) and the XDG paths.
- `internal/remote/`: remote mode: the twin name of each site of a `remote = true` route file, the
  tailnet address (`tailscale0`, or `utun*` on macOS), the Cloudflare token file (format checked,
  never printed) and the updater serve runs to keep the `*.<suffix>` A record on that address.
- `internal/probe/`: dials route upstreams in parallel (up, down or unknown) for `status` and
  `doctor`.
- `internal/doctor/`: the read-only checks behind `devingress doctor`, each one a function taking
  its own injectable dependencies (resolver, dialer, cert pool, file reads).
- `.claude-plugin/marketplace.json` and `plugins/devingress/`: the Claude Code and Codex plugin. Its
  `setup` skill teaches agents in other projects to write and register a `devingress.toml`.
  `internal/routefile/skill_test.go` loads every `toml` block in its `SKILL.md` and checks the
  manifests point at each other.

## Commands

```sh
go build -o bin/devingress ./cmd/devingress
go test ./...
go test -tags e2e ./e2e/...   # builds the binary, runs an isolated serve on free ports, drives it over TLS
go vet ./...
go vet -tags e2e ./...
gofmt -l .               # must print nothing; `gofmt -w .` fixes it
claude plugin validate .   # when claude is installed; the only warning is the missing version
claude plugin validate plugins/devingress
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
  printf 'bind = ["127.0.0.1"]\nhttp_port = 18080\nhttps_port = 18443\ndns_listen = "127.0.0.1:18053"\n' \
    > $XDG_CONFIG_HOME/devingress/config.toml
  ```
- Never run `sudo` or `setcap`.
- Never run `devingress service install` or `uninstall` without `--print`: they run `sudo`,
  `systemctl` and `loginctl` (`launchctl` on macOS).
- Never run `devingress dns setup` or `remove` without `--print`: they run `sudo` and `systemctl`
  and write `/etc/systemd/resolved.conf.d/` (`/etc/resolver/` and `killall` on macOS). Never run a
  `resolvectl` subcommand that changes state.
- Never run a `tailscale` command that changes state (`serve`, `set`, `up`, `down`), read or
  print a real Cloudflare token (`~/.config/devingress/cloudflare-token`), or call the Cloudflare
  API or an ACME CA. Tests use fake tokens in `t.TempDir()`, a fake Cloudflare API and
  `tailscale serve status --json` fixtures.
- Never run `launchctl`, `security` or `killall`. Tests replace every function that would, and
  write nothing outside `t.TempDir()`. The darwin-only code (`_darwin.go`) is checked by
  `GOOS=darwin go vet ./...` and the CI `macos` job.
- A change to the route file format (`internal/routefile/help.go`) or to a flag or command the
  `setup` skill names updates `plugins/devingress/skills/setup/SKILL.md`.

## Pull requests

Commits and PR titles use conventional prefixes (`feat:`, `fix:`, `docs:`, `chore:`). Open the PR
with `gh pr create`; its body says what changed and why. CI (`.github/workflows/ci.yml`) runs
`go build`, `go vet`, `gofmt -l` and `go test` on every PR and on pushes to `main`, plus separate
jobs that run the e2e tests, `go build`/`go vet`/`go test` and the e2e tests on `macos-latest`, and a
`goreleaser release --snapshot --clean` build.

## Releases

`.goreleaser.yaml` builds `linux/amd64`, `linux/arm64`, `darwin/amd64` and `darwin/arm64` archives
and stamps `main.version` with the pushed tag. `.github/workflows/release.yml` runs `goreleaser
release --clean` and publishes a GitHub Release when a `v*` tag is pushed. Agents never push a tag or create a release; the owner
cuts them.
