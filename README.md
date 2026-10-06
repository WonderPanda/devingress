# devingress

One local HTTPS ingress for every project and worktree on a machine. A single
Go binary with Caddy built in: `devingress serve` owns :80/:443, and each checkout
registers its routes from a `devingress.toml` it carries.

```sh
devingress serve                            # run the proxy (normally under systemd)
devingress up [dir] [--file F] [--slug S]   # register dir's routes (default ./devingress.toml)
devingress down [dir]                       # remove dir's routes
devingress status                           # owners and their hosts
devingress caddyfile                        # the rendered Caddyfile, for debugging
```

## How it works

- **Owners.** Routes belong to the directory that registered them. Every
  `up`/`down` drops the routes of owners whose directory no longer exists, so a
  worktree deleted with `rm -rf` or `git worktree remove` loses its routes on the
  next call without any teardown hook having run.
- **Validate before apply.** Each call renders every live owner into one
  Caddyfile, adapts and provisions it in-process, and only then loads it into the
  running server over the admin socket. A conflict (two owners claiming one
  host or path) or an invalid raw snippet fails that call and leaves everyone
  else's routes untouched. State is only written after the config is accepted.
- **No daemon of devingress's own.** `serve` is Caddy; the CLI reconciles
  synchronously under a file lock, so concurrent worktree hooks serialize.
- **Certificates.** Caddy's internal CA issues a certificate per host. When an
  mkcert root exists (`$CAROOT` or `mkcert -CAROOT`) it signs those leaves
  directly, so anything that already trusts mkcert — browsers,
  `NODE_EXTRA_CA_CERTS` in containers — trusts devingress. One cert per host also
  means browsers never coalesce connections across hosts, so pinning one host to
  HTTP/1.1 cannot affect the others.

## devingress.toml

```toml
domain = "eluve.dev"             # appended to every route host
env = [".env.worktree"]          # ${VARS} resolve from these, then the process env

[[route]]
host = "web-${slug}"             # ${slug}: --slug, else the git branch slugified
to = "${VITE_DEV_PORT}"          # bare port = 127.0.0.1:<port>; otherwise passed to Caddy as-is
retry = "5s"                     # retry refused connections (dev servers restarting)

[[route]]
host = "medplum-${slug}"
to = "${MEDPLUM_HOST_PORT}"
when = "${ENABLE_MEDPLUM}"       # 1/true/yes/on; unset = off
protocols = ["h1"]               # ALPN pin
caddy = '''                      # raw site body; replaces the generated reverse_proxy
@options method OPTIONS
handle @options {
	header Access-Control-Allow-Origin "{http.request.header.Origin}"
	respond 204
}
reverse_proxy ${upstream} {
	header_down -Access-Control-Allow-Origin
}
'''

[[path]]                         # on the plain-HTTP listener, for containers
prefix = "/supertokens-${slug}"  # calling host.docker.internal:80
to = "${SUPERTOKENS_HOST_PORT}"
strip = true
proxy = 'header_down Cache-Control "max-age=45, must-revalidate"'   # raw, inside reverse_proxy
```

devingress variables are `${NAME}`; Caddy's own `{placeholder}` and `{$ENV}` syntax
passes through untouched. Env files win over the process env, so a stale
exported variable cannot silently redirect a route.

## Host config

`~/.config/devingress/config.toml`, all optional:

```toml
bind = ["127.0.0.1", "172.17.0.1"]   # default: loopback + the docker0 address if present
http_port = 80
https_port = 443
ca_root = "/path/to/mkcert/root"     # default: $CAROOT or `mkcert -CAROOT`
```

State lives in `$XDG_STATE_HOME/devingress`, Caddy's storage in
`$XDG_DATA_HOME/devingress/caddy`, the admin socket in `$XDG_RUNTIME_DIR/devingress`.

DNS is not devingress's job: point `*.<domain>` at 127.0.0.1 (dnsmasq, or
systemd-resolved forwarding to it).

## Build

```sh
go build -o bin/devingress ./cmd/devingress
go test ./...
```

Binding :80/:443 as a normal user needs
`sudo setcap cap_net_bind_service=+ep bin/devingress` (lost on every rebuild).

## Not yet

- `devingress install`: systemd user unit + the setcap step.
- Binding the docker0 address fails if Docker is not up when `serve` starts.
- The binary carries all of Caddy's standard modules (~80 MB).
