# devingress

Per-worktree HTTPS hostnames for multi-service stacks that your own scripts and
hooks start.

```
https://web-my-branch.example.dev     -> 127.0.0.1:10299
https://api-my-branch.example.dev     -> 127.0.0.1:10298
https://web-other-branch.example.dev  -> 127.0.0.1:11420
```

One process owns ports 80 and 443. Each checkout carries a `devingress.toml`
describing the routes it needs and registers them with `devingress up`; they
disappear when the checkout does. It's a single Go binary with
[Caddy](https://caddyserver.com) built in: no Docker, no separate proxy to
configure.

**Early, and Linux-only so far.** You need a Go toolchain to install it,
[mkcert](https://github.com/FiloSottile/mkcert) for certificates your browser
trusts, and DNS for your dev domain (or `*.localhost`).

## Why

Running several worktrees of the same app at once means each one needs its own
ports, its own hostnames, and real TLS: cookies, OAuth redirects, service
workers and `wss://` all behave differently over plain HTTP. When a setup hook
starts the stack (allocating ports, writing env files, running Docker Compose),
the proxy routes have to come from that same hook and go away with the
worktree.

### Why not a Caddyfile with `import worktrees/*.caddy`?

That's where devingress started. Each hook writes a snippet, Caddy reloads, and
it works until:

- **One bad snippet blocks everyone.** Caddy rejects the whole config if any
  snippet is invalid or two worktrees claim the same host, so nobody's new
  routes apply until someone finds the culprit. devingress validates the merged
  config before applying it and fails only the command that caused the problem,
  naming both owners on a conflict.
- **Deleted worktrees leave routes behind.** A worktree removed with `rm -rf`
  or `git worktree remove` never runs its teardown hook. On every `up`/`down`,
  devingress drops the routes of any checkout whose directory is gone.
- **Parallel hooks race.** Two worktrees set up at once both rewrite and reload.
  devingress serializes calls with a file lock.
- **Snippets come from shell heredocs.** devingress reads a committed TOML file
  with `${VARS}` from your env files, and still lets a route drop down to raw
  Caddy when it needs to.

### devingress or portless?

Use [portless](https://github.com/vercel-labs/portless) if your app is a dev
server it can launch: no config, no hooks, and it runs on macOS and Windows.
Use devingress if something else starts your services, decides their ports, and
already expects particular hostnames. See the
[full comparison](#compared-to-portless).

## Install

```sh
go install github.com/WonderPanda/devingress/cmd/devingress@latest

# Let it bind :80/:443 as your user (repeat after every reinstall)
sudo setcap cap_net_bind_service=+ep "$(go env GOPATH)/bin/devingress"
```

Run it as a systemd user service:

```ini
# ~/.config/systemd/user/devingress.service
[Unit]
Description=devingress local HTTPS ingress

[Service]
ExecStart=%h/go/bin/devingress serve
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
```

```sh
systemctl --user enable --now devingress
```

**Certificates:** run `mkcert -install` once. devingress signs with mkcert's
root, so your browser and anything else that already trusts mkcert trusts
devingress.

**DNS:** point your dev domain at `127.0.0.1`, e.g. with dnsmasq
`address=/example.dev/127.0.0.1`. Or use `*.localhost`, which most browsers and
curl already resolve to loopback.

## Usage

Add a `devingress.toml` to your project:

```toml
domain = "example.dev"
env = [".env.local"]             # ${VARS} come from here, then the environment

[[route]]
host = "web-${slug}"             # ${slug} = the current git branch, slugified
to = "${WEB_PORT}"               # a bare port means 127.0.0.1:<port>
retry = "5s"                     # ride out dev-server restarts instead of 502ing

[[route]]
host = "api-${slug}"
to = "${API_PORT}"
```

Then, from the checkout or your worktree hooks:

```sh
devingress up          # register this checkout's routes
devingress down        # remove them
devingress status      # every registered checkout and its URLs
devingress routes      # every registered route; --dir . for this checkout's, --json for scripts
```

With [worktrunk](https://github.com/max-sixty/worktrunk), that's:

```toml
pre-remove = "devingress down"   # optional: deleted checkouts are pruned on the next call anyway

[[pre-start]]
routes = "devingress up"
```

### Beyond host-to-port

```toml
[[route]]
host = "realtime-${slug}"
to = "${REALTIME_PORT}"
when = "${ENABLE_REALTIME}"      # skip unless 1/true/yes/on
protocols = ["h1"]               # pin ALPN for this host only
caddy = '''                      # raw Caddyfile site body for anything else
header Access-Control-Allow-Origin "{http.request.header.Origin}"
reverse_proxy ${upstream}
'''

[[path]]                         # plain-HTTP path routes, for containers that
prefix = "/auth-${slug}"         # call host.docker.internal instead of a hostname
to = "${AUTH_PORT}"
strip = true
proxy = 'header_down Cache-Control "max-age=45"'   # raw, inside reverse_proxy
```

devingress variables are `${NAME}`; Caddy's own `{placeholder}` syntax passes
through untouched. Each host gets its own certificate, so pinning one host to
HTTP/1.1 can't affect connections to the others.

`devingress help routefile` prints the full `devingress.toml` reference, and
`devingress help <command>` each command's flags, files and exit codes.

### Host settings

Optional, in `~/.config/devingress/config.toml`:

```toml
bind = ["127.0.0.1", "172.17.0.1"]   # default: loopback + docker0 if present
http_port = 80
https_port = 443
ca_root = "/path/to/ca"              # default: mkcert's CAROOT
```

`devingress caddyfile` prints the generated Caddy config.

## Compared to portless

portless is a launcher: `portless run next dev` picks a port, serves
`https://myapp.localhost`, adds the branch as a subdomain in a linked worktree,
and drops the route when the process exits. Services it doesn't launch, such as
anything Docker Compose starts, are registered with `portless alias <name>
<port>` in a setup hook and removed with `portless alias --remove <name>` in
teardown. Aliases are never removed automatically, so a worktree deleted
without its teardown hook leaves them behind.

| | portless | devingress |
|---|---|---|
| Services it doesn't launch | An `alias` per service in setup and a `--remove` per service in teardown | Listed once in `devingress.toml`; `devingress up` in setup |
| Cleanup | Launched apps: when the process exits. Aliases: only when removed | When the checkout's directory is gone, on the next `up`/`down` |
| Ports | Picked from 4000–4999 when an app launches. Fine through the proxy, but traffic that bypasses it (containers calling `host.docker.internal:<port>`, plain-HTTP server-to-server calls) needs the port at setup time, which means pinning `--app-port` per worktree | Allocated by your setup; env files, Docker Compose and `devingress.toml` all read the same values |
| Hostnames | Launched apps get `<branch>.<app>.<tld>`. Aliases take any name and `--tld` accepts a domain you own, so existing names can be kept by aliasing each one. `portless get <name>` (in its CLI help) prints an app's URL for wiring | Any template, e.g. `web-${slug}.example.dev`, from one file. `devingress routes --json` lists the registered URLs |
| Per-route settings | App name, script and port | Retry while a dev server restarts, per-host HTTP/1.1 pin, raw Caddy for CORS, header and rewrite rules, plain-HTTP path routes for containers |
| Certificates and DNS | Creates and trusts its own CA; keeps `/etc/hosts` in sync | Uses mkcert's root; DNS is yours to set up |
| Running at boot | `portless service install` | A systemd unit you write (above) |
| Platforms | macOS, Linux, Windows, WSL | Linux only so far |
| Install | `npm install -g portless` (Node.js 24+) | `go install` (Go toolchain); no prebuilt binaries yet |

Both use port 443 by default, so run one per machine, or move devingress with
`https_port`/`http_port` in its host settings.

## Status

Early. There's no `devingress install` command yet, and `serve` can't bind the
docker0 address until Docker is up (the unit's `Restart=on-failure` retries
until it is).
