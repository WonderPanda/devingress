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
| DNS | `*.localhost`, or `/etc/hosts` entries it keeps in sync, which needs root on every route change | A built-in wildcard DNS listener for any domain; `devingress dns setup` needs root once |
| Runtime | Node.js 24+, installed with npm | One Go binary |
| Running at boot | `portless service install` | `devingress service install` |

Both bind ports 80 and 443, so run one per machine, or move devingress with
`http_port`/`https_port` in its host config.

## Install

Download the latest release (`linux/amd64` or `linux/arm64`) and check it against its checksum:

```sh
arch=amd64   # or arm64
curl -fsSLO "https://github.com/WonderPanda/devingress/releases/latest/download/devingress_linux_$arch.tar.gz"
curl -fsSLO "https://github.com/WonderPanda/devingress/releases/latest/download/checksums.txt"
grep "devingress_linux_$arch.tar.gz" checksums.txt | sha256sum -c -
tar -xzf "devingress_linux_$arch.tar.gz" -C ~/.local/bin devingress

# Run serve at boot, as you, with the capability to bind :80/:443
devingress service install
```

Building from source with a Go toolchain stays an option:
`go install github.com/WonderPanda/devingress/cmd/devingress@latest`.

`service install` writes `/etc/systemd/system/devingress.service`, a system
unit that runs `devingress serve` as your user after Docker, turns on lingering
so `/run/user/<uid>` exists at boot, and starts it. It prints each `sudo`
command before running it; `devingress service install --print` shows the unit
and the commands without running anything. systemd grants the bind capability,
so reinstalling the binary only needs `sudo systemctl restart devingress`.
`devingress service uninstall` removes the unit.

If you set devingress up from an earlier version of this README, with a user
unit in `~/.config/systemd/user/devingress.service` and `setcap`, stop the user
unit first with `systemctl --user disable --now devingress`; `service install`
reminds you when that file exists.

**Certificates:** run `devingress trust` once. It installs devingress's own
root CA into the system store, NSS (Chrome, Firefox) and Java, asking for
`sudo` for the system store. If you already use
[mkcert](https://github.com/FiloSottile/mkcert) (`mkcert -install`), you can
skip this: devingress signs with mkcert's root, so everything that already
trusts mkcert trusts devingress. Node.js ignores the system store; point it at
the root with `NODE_EXTRA_CA_CERTS=$(devingress trust --path)`.

**DNS:** run `devingress dns setup example.dev` once. `serve` runs a wildcard
DNS listener (`dns_listen`, default `127.0.0.1:15353`) that answers loopback
for every name, and `dns setup` routes your dev domain to it through a
systemd-resolved drop-in, asking for `sudo`; `--print` shows the drop-in and the
commands without running anything. If resolved has global DNS servers (e.g.
from `omarchy dns`), they are asked first and answer NXDOMAIN, so `setup`
refuses; `--reset-global-dns` clears them, which affects every lookup on the
machine. dnsmasq (`address=/example.dev/127.0.0.1`) and `*.localhost`, which
resolved, browsers and curl already resolve to loopback, still work instead.

Containers don't use the host's resolved; give them `extra_hosts`. To share
the routing with them, a resolved drop-in with `DNSStubListenerExtra=172.17.0.1`
makes resolved's stub listen on docker0, and containers given
`dns: 172.17.0.1` get the same per-domain routing.

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
devingress status      # every registered checkout and its URLs, each marked up or down
devingress routes      # every registered route, grouped by checkout; --dir . for this checkout's, --json for scripts
devingress doctor      # a URL doesn't load? diagnose serve, ports, DNS, certificates, upstreams
devingress dns setup example.dev   # once: resolve *.example.dev to loopback
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
dns_listen = "127.0.0.1:15353"       # serve's DNS listener; "" turns it off
```

`devingress caddyfile` prints the generated Caddy config.

## Status

Early, and only run on Linux so far.
