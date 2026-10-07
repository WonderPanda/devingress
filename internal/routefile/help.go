package routefile

// Help is the devingress.toml reference printed by `devingress help routefile`.
// TestHelpCoversEveryKey fails when a key of `file` is missing from it.
const Help = `devingress.toml: the routes one checkout registers

"devingress up [dir]" reads <dir>/devingress.toml (another name with --file), resolves it and
stores the result in the state dir. Variables are expanded and "when" is evaluated at that moment,
so after changing the file or the env files it names, run "devingress up" again.

Top-level keys:

  domain      string, default "" (hosts are used as written)
              Appended to every [[route]] host as ".<domain>".
  env         array of strings, default []
              Env files to read ${VARS} from, relative to the checkout. Each line is KEY=VALUE;
              blank lines, "#" comments and an "export " prefix are allowed, and one pair of
              matching quotes around the value is removed. A later file overrides an earlier one.

[[route]]: one HTTPS site; repeat the table for more.

  host        string, required
              Host name before ".<domain>" is appended. Served at https://<host>.<domain>.
  to          string, required
              Upstream. A bare port means 127.0.0.1:<port>; anything else (host:port, a URL) is
              passed to Caddy's reverse_proxy as written.
  when        string, default "" (always on)
              Register the route only if this expands to 1, true, yes or on (any case). An
              undefined variable here means off, not an error.
  retry       string, a Caddy duration such as "5s", default "" (no retry)
              Retry a refused upstream connection for this long instead of answering 502, to ride
              out dev-server restarts.
  protocols   array of strings, default [] (h1 and h2)
              Pin the TLS ALPN protocols offered for this host only: "h1", "h2".
  proxy       string, default ""
              Raw Caddyfile directives placed inside this site's reverse_proxy block.
  caddy       string, default ""
              Raw Caddyfile site body that replaces the generated reverse_proxy. Cannot be
              combined with proxy or retry.

[[path]]: one prefix on the plain-HTTP listener (http_port), for containers that call the host
(host.docker.internal) instead of a host name; repeat the table for more.

  prefix      string, required
              Must start with "/"; a trailing "/" is dropped. Matches <prefix>/* on any host.
  to          string, required
              Upstream, as for [[route]].
  when        string, default "" (always on)
              As for [[route]].
  strip       boolean, default false
              Remove the prefix from the request path before proxying.
  proxy       string, default ""
              Raw Caddyfile directives placed inside this path's reverse_proxy block.

Two checkouts cannot claim the same host or prefix: the second "up" fails and the first keeps
its routes.

Variables:

  ${NAME} is expanded in every string value. A name is looked up in this order:
    1. ${upstream}: the route's resolved "to", only inside proxy and caddy
    2. ${slug}: the --slug flag of "devingress up", else the checkout's git branch, lowercased,
       with each run of characters outside a-z and 0-9 replaced by "-" (feat/Login -> feat-login)
    3. the env files, in the order listed
    4. the environment of "devingress up"
  An undefined variable is an error, except in "when". Caddy's own {placeholder} and {$ENV}
  syntax passes through to Caddy untouched.

Example:

  domain = "example.dev"
  env = [".env.local"]             # ${VARS} come from here, then the environment

  [[route]]
  host = "web-${slug}"             # ${slug} = the current git branch, slugified
  to = "${WEB_PORT}"               # a bare port means 127.0.0.1:<port>
  retry = "5s"                     # ride out dev-server restarts instead of 502ing

  [[route]]
  host = "realtime-${slug}"
  to = "${REALTIME_PORT}"
  when = "${ENABLE_REALTIME}"      # skip unless 1/true/yes/on
  protocols = ["h1"]               # pin ALPN for this host only
  caddy = '''
  header Access-Control-Allow-Origin "{http.request.header.Origin}"
  reverse_proxy ${upstream}
  '''

  [[path]]
  prefix = "/auth-${slug}"
  to = "${AUTH_PORT}"
  strip = true
  proxy = 'header_down Cache-Control "max-age=45"'
`
