---
name: setup
description: Set up devingress for a project, writing or editing its devingress.toml, registering its routes with `devingress up` and checking them. Use when asked to set up devingress, add or change a devingress.toml or a devingress route, or give a project's dev servers stable https URLs (e.g. https://web-my-branch.example.dev) across git worktrees.
---

# /devingress:setup

devingress is one process on this machine that owns :80/:443 and serves HTTPS routes. Each checkout
lists the routes it needs in a committed `devingress.toml` and registers them with `devingress up`.
Your job: write that file for the current project, register it, and check the URLs work.

## Rules

Machine setup belongs to the user. Never run `devingress serve`, `devingress service install` or
`uninstall`, `devingress dns setup` or `remove`, `devingress trust`, `sudo`, `devingress down --all`,
or `devingress down` on any checkout other than the current one. When `devingress doctor` says DNS
or the root CA is missing, tell the user to run `devingress dns setup <domain>` or
`devingress trust` themselves.

## Steps

1. **Check devingress is installed**: `devingress version`. If the command is not found, point the
   user at https://github.com/WonderPanda/devingress#install and stop.
2. **Read the reference**: run `devingress help routefile`, and `devingress help up` before using
   any `up` flag. Their output is the source of truth for keys, defaults and variable rules; this
   skill holds only the procedure and the gotchas.
3. **Inspect the project** for its dev servers and their ports: `package.json` scripts and
   framework configs (vite, next, ...), `docker-compose*.yml` `ports:` and their `${VAR:-default}`
   defaults, `Procfile`s, `.env*` files and whatever writes them, and worktree hooks
   (`.config/wt.toml` for worktrunk, or setup scripts) that allocate per-worktree ports. If a
   `devingress.toml` already exists, edit it rather than replace it.
4. **Pick hostnames and the domain**:
   - One host per service, `<service>-${slug}` (`web-${slug}`, `api-${slug}`), so each worktree
     gets its own names. `${slug}` is the git branch, slugified.
   - Domain: reuse the one other checkouts already use (the hosts in `devingress routes --json`),
     since its DNS is already set up. If there is none, ask the user. `localhost` needs no DNS
     setup at all: `*.localhost` resolves to loopback.
   - Keep names the project already uses (env files, CORS allowlists, OAuth redirect URIs, cookie
     domains) when they fit `<host>.<domain>`.
5. **Ports**: read them from the env file the project's setup already writes, listed in
   `env = [...]`, with `${VAR:-default}` set to today's default port, so the file works in a
   single checkout now and per worktree once the setup allocates ports. A bare port means
   `127.0.0.1:<port>`. Do not invent a port-allocation scheme: if the project has none and the user
   wants several worktrees running at once, say what is needed and leave it to them.
6. **Write `devingress.toml`** at the checkout root, to be committed. Use `retry = "5s"` on routes
   to dev servers that restart. For example:

   ```toml
   domain = "example.dev"
   env = [".env.local"]            # must exist whenever `devingress up` runs

   [[route]]
   host = "web-${slug}"
   to = "${WEB_PORT:-5173}"        # the port in .env.local, else today's default
   retry = "5s"

   [[route]]
   host = "api-${slug}"
   to = "${API_PORT:-3000}"
   retry = "5s"
   ```

   Look up the rest in `devingress help routefile` only when the project needs it: `when` for
   optional services, `protocols` for HTTP/1.1-only hosts, `caddy`/`proxy` for CORS, header and
   rewrite rules, `[[path]]` for containers that call `host.docker.internal`.
7. **Register and verify**: run `devingress up` from the checkout. It prints every URL it
   registered: check each expected host is there. Then `devingress status`: each URL is marked up
   or down, and down means nothing listens on the upstream yet, so start the dev server. When a
   URL does not load, run `devingress doctor`. For the URLs the app needs (env files, OAuth
   redirects), use `devingress routes --dir . --json`; its `url` carries a non-443 port when one
   is configured.
8. **Hooks**: when the project already has worktree hooks, add `devingress up` to setup, after
   the env files are written (and optionally `devingress down` to teardown). With worktrunk, in
   `.config/wt.toml`:

   ```
   pre-remove = "devingress down"

   [[pre-start]]
   routes = "devingress up"
   ```

   Otherwise tell the user to rerun `devingress up` after changing `devingress.toml` or its env
   files.
9. **Named sets**: a tool or agent that adds routes at runtime to a checkout that already
   registered some uses `devingress up --name <n> --file <other.toml>` and
   `devingress down --name <n>`, so it neither replaces nor removes the checkout's own set.

## Gotchas

- `up` resolves variables and `when` at the moment it runs: after changing the file or its env
  files, run `devingress up` again.
- An env file listed in `env` that does not exist makes `up` fail. List only files that exist
  whenever `up` runs, e.g. one the setup hook writes before calling `up`.
- An undefined `${VAR}` without a default is an error, except in `when`, where it means off.
- Unknown keys are silently ignored, so a typo such as `port =` instead of `to =` registers nothing
  for that key: check the URLs `up` printed.
- Two checkouts cannot claim the same host or prefix: the second `up` fails and the first keeps its
  routes. Hence `${slug}` in every host.
- `${slug}` comes from the git branch; outside a git checkout `up` fails unless given `--slug`.
- Node.js ignores the system trust store: server-side calls to the https URLs need
  `NODE_EXTRA_CA_CERTS=$(devingress trust --path)` (`--path` only prints the root CA's path).
- Caddy's own `{placeholder}` syntax passes through untouched; only `${...}` is devingress's.
