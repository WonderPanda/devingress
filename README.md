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
