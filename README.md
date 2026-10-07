# devingress

Stable HTTPS URLs for every local dev server, across every project and git
worktree on your machine.

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

## Why

Running several worktrees of the same app at once means each one needs its own
ports, its own hostnames, and real TLS (cookies, OAuth redirects, service
workers and `wss://` all behave differently over plain HTTP). Hand-maintained
proxy configs drift, and a deleted worktree leaves routes pointing at nothing.
devingress makes the routes part of the checkout.

## Compared to portless

[portless](https://github.com/vercel-labs/portless) is the better fit when your
app is a dev server it can launch: `portless run next dev` picks a port, serves
`https://myapp.localhost`, adds the branch as a subdomain in a linked worktree,
and drops the route when the process exits. No config and no hooks.

devingress is for stacks where something else starts the services, typically a
worktree setup hook that allocates ports, writes env files and runs Docker
Compose. portless reaches those services through static aliases, so the hooks
end up maintaining the route list by hand:

```sh
# setup hook: one line per service, branch added by hand
portless alias "api-$BRANCH"  "$API_PORT"
portless alias "auth-$BRANCH" "$AUTH_PORT"
# ...

# teardown hook: the same list again
portless alias --remove "api-$BRANCH"
portless alias --remove "auth-$BRANCH"
# ...
```

Aliases are never removed automatically, so a worktree deleted without running
its teardown hook leaves its routes behind. With devingress the services are
listed once, in a committed `devingress.toml` that reads ports from the env
file setup already writes, and the hooks shrink to one line. With
[worktrunk](https://github.com/max-sixty/worktrunk):

```toml
pre-remove = "devingress down"   # optional: routes of deleted checkouts are dropped on the next call

[[pre-start]]
routes = "devingress up"
```

| | portless | devingress |
|---|---|---|
| Services it doesn't launch | One `alias` per service in setup, one `--remove` per service in teardown | Listed once in `devingress.toml`; `devingress up` in setup |
| Cleanup | Launched apps: when the process exits. Aliases: only when removed | When the checkout's directory is gone, on the next `up`/`down` |
| Ports | Picked from 4000–4999 when the app launches. Fine for traffic through the proxy, but anything that bypasses it (containers calling `host.docker.internal:<port>`, plain-HTTP server-to-server calls) needs the port at setup time, which means pinning `--app-port` per worktree and allocating ports yourself | Allocated once by your setup; env files, Docker Compose and `devingress.toml` all read the same values |
| Hostnames | `<branch>.<app>.localhost`, so existing names in env files, CORS allowlists, cookie domains and OAuth redirect URIs have to be renamed. `portless get` prints the URL ahead of time for wiring | Any template, e.g. `web-${slug}.example.dev`, so existing names stay as they are. `devingress routes --json` lists the registered URLs |
| Per-route settings | App name, script and port | Retry while a dev server restarts, per-host HTTP/1.1 pin, raw Caddy for CORS, header and rewrite rules, plain-HTTP path routes for containers |
| Runtime | Node.js 24+, installed with npm | One Go binary |

Both bind ports 80 and 443, so run one per machine, or move devingress with
`http_port`/`https_port` in its host config.

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

**Certificates:** if you use [mkcert](https://github.com/FiloSottile/mkcert)
(`mkcert -install`), devingress signs with its root, so your browser and
anything else that already trusts mkcert trusts devingress.

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

Then, from the checkout or your worktree setup/teardown hooks:

```sh
devingress up          # register this checkout's routes
devingress down        # remove them
devingress status      # every registered checkout and its URLs
devingress routes      # every registered route; --dir . for this checkout's, --json for scripts
```

Every `up`/`down` also removes the routes of checkouts that no longer exist, so
a worktree deleted with `rm -rf` is cleaned up on the next call.

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
through untouched.

`devingress help routefile` prints the full `devingress.toml` reference, and
`devingress help <command>` each command's flags, files and exit codes.

## How it works

- Each `up`/`down` merges every live checkout's routes into one config and
  validates it with Caddy before touching the running server. A bad snippet, or
  two checkouts claiming the same host, fails that command while everyone
  else's routes keep serving.
- Calls are serialized with a file lock, so parallel worktree hooks are safe.
- Each host gets its own certificate, so pinning one host to HTTP/1.1 can't
  affect connections to the others.

Optional host settings live in `~/.config/devingress/config.toml`:

```toml
bind = ["127.0.0.1", "172.17.0.1"]   # default: loopback + docker0 if present
http_port = 80
https_port = 443
ca_root = "/path/to/ca"              # default: mkcert's CAROOT
```

`devingress caddyfile` prints the generated Caddy config.

## Status

Early, and only run on Linux so far. There's no `devingress install` command
yet, and `serve` can't bind the docker0 address until Docker is up (the unit's
`Restart=on-failure` retries until it is).
