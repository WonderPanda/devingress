// devingress: one local HTTPS ingress for every project and worktree on a machine.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/WonderPanda/devingress/internal/config"
	"github.com/WonderPanda/devingress/internal/ingress"
	"github.com/WonderPanda/devingress/internal/routefile"
	"github.com/WonderPanda/devingress/internal/state"
)

const usage = `devingress: local HTTPS routing for projects and worktrees

  devingress serve                                 run the proxy (normally under systemd)
  devingress up [dir] [--file F] [--slug S]        register dir's routes (default ./devingress.toml)
  devingress down [dir]                            remove dir's routes
  devingress status                                list owners and their hosts
  devingress routes [--dir D] [--host H] [--json]  query registered routes
  devingress doctor [--json]                       diagnose why a URL does not load
  devingress caddyfile                             print the rendered Caddyfile
  devingress trust [--uninstall] [--path]          trust devingress's root CA (mkcert optional)
  devingress service install|uninstall [--print]   run serve at boot as a systemd system unit
  devingress dns setup|remove <domain> [--reset-global-dns] [--print]  route a domain to serve's DNS
  devingress help <command>                        full help for one command
  devingress help routefile                        the devingress.toml reference

Every up/down also drops routes whose owner directory no longer exists.

Worktree hooks: run "devingress up" after the worktree's ports are assigned (they are read from
its env files at that moment) and "devingress down" before the worktree is removed. A worktree
deleted without "down" is pruned by the next up or down.

Files:
  ~/.config/devingress/config.toml          host settings: bind, http_port, https_port, ca_root,
                                            dns_listen
  $XDG_STATE_HOME/devingress                registered routes (default ~/.local/state/devingress)
  $XDG_RUNTIME_DIR/devingress/admin.sock    admin socket of the running serve
`

type command struct {
	synopsis string // after "devingress "
	help     string // after the synopsis: what it does, flags, files, exit codes, examples
	run      func(args []string, stdout io.Writer) error
}

func (c command) text() string { return "usage: devingress " + c.synopsis + "\n\n" + c.help }

var commands = map[string]command{
	"serve":     {synopsis: "serve", help: serveHelp, run: serve},
	"up":        {synopsis: "up [dir] [--file F] [--slug S]", help: upHelp, run: up},
	"down":      {synopsis: "down [dir]", help: downHelp, run: down},
	"status":    {synopsis: "status", help: statusHelp, run: status},
	"routes":    {synopsis: "routes [--dir D] [--host H] [--json]", help: routesHelp, run: routes},
	"doctor":    {synopsis: "doctor [--json]", help: doctorHelp, run: doctorCmd},
	"caddyfile": {synopsis: "caddyfile", help: caddyfileHelp, run: caddyfile},
	"trust":     {synopsis: "trust [--uninstall] [--path]", help: trustHelp, run: trust},
	"service":   {synopsis: "service install|uninstall [--print]", help: serviceHelp, run: serviceCmd},
	"dns":       {synopsis: "dns setup|remove <domain> [--reset-global-dns] [--print]", help: dnsHelp, run: dnsCmd},
}

// usageError is a bad command line: exit 2 with the command's synopsis.
type usageError struct{ error }

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	name, args := args[0], args[1:]
	switch name {
	case "-h", "--help", "help":
		return help(args, stdout, stderr)
	}
	cmd, ok := commands[name]
	if !ok {
		fmt.Fprintf(stderr, "devingress: unknown command %q\n\n%s", name, usage)
		return 2
	}
	err := cmd.run(args, stdout)
	var ue usageError
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprint(stdout, cmd.text())
		return 0
	case errors.Is(err, errDoctorFailed):
		return 1 // doctor already printed every failing check
	case errors.As(err, &ue):
		fmt.Fprintf(stderr, "devingress %s: %v\nusage: devingress %s\nRun \"devingress help %s\" for details.\n",
			name, ue.error, cmd.synopsis, name)
		return 2
	}
	fmt.Fprintf(stderr, "devingress: %v\n", err)
	return 1
}

func help(args []string, stdout, stderr io.Writer) int {
	switch {
	case len(args) == 0:
		fmt.Fprint(stdout, usage)
		return 0
	case len(args) > 1:
		fmt.Fprintf(stderr, "devingress help: expected one topic, got %q\n\n%s", args, usage)
		return 2
	case args[0] == "routefile":
		fmt.Fprint(stdout, routefile.Help)
		return 0
	}
	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "devingress help: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	fmt.Fprint(stdout, cmd.text())
	return 0
}

// flags returns a FlagSet that reports errors to run instead of printing them.
func flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parse parses args into fs, wrapping a bad flag as a usageError. -h and
// --help come back as flag.ErrHelp.
func parse(fs *flag.FlagSet, args []string) error {
	err := fs.Parse(args)
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		return usageError{err}
	}
	return err
}

// noArgs parses the command line of a command that takes no flags or arguments.
func noArgs(name string, args []string) error {
	fs := flags(name)
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Errorf("unexpected argument %q", fs.Arg(0))}
	}
	return nil
}

// positional parses flags that may come before, between or after positional arguments, and
// returns the positional ones.
func positional(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for len(args) > 0 {
		if err := parse(fs, args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) > 0 {
			pos, args = append(pos, args[0]), args[1:]
		}
	}
	return pos, nil
}

// dirArg parses flags that may come before or after the positional dir.
func dirArg(fs *flag.FlagSet, args []string) (string, error) {
	pos, err := positional(fs, args)
	if err != nil {
		return "", err
	}
	if len(pos) > 1 {
		return "", usageError{fmt.Errorf("expected at most one directory, got %v", pos)}
	}
	dir := "."
	if len(pos) == 1 {
		dir = pos[0]
	}
	return ownerDir(dir)
}

// ownerDir makes dir absolute and resolves symlinks so one checkout is one
// owner however it was reached; a directory that is already gone keeps its
// cleaned path.
func ownerDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	return abs, nil
}

const serveHelp = `Run the proxy in the foreground until SIGINT or SIGTERM: Caddy serving every registered route on
http_port and https_port of each bind address. Normally run by the system unit that "devingress
service install" sets up. Only one serve runs per admin socket.

Reads the config file and the state dir, pruning owners whose directory is gone, then creates the
admin socket and keeps certificates under $XDG_DATA_HOME/devingress/caddy. up and down load new
routes into it through the admin socket.

Exit codes:
  0  stopped by SIGINT or SIGTERM
  1  already serving, a listener could not be opened, or the config was rejected
  2  bad flag or argument

Example:
  devingress serve
`

func serve(args []string, _ io.Writer) error {
	if err := noArgs("serve", args); err != nil {
		return err
	}
	return ingress.Serve()
}

const upHelp = `Register dir's routes, replacing any it registered before. dir defaults to the current
directory; it is made absolute and symlinks are resolved, so one checkout is one owner however it
is reached. Flags may come before or after dir.

Flags:
  --file F    route file, relative to dir (default devingress.toml)
  --slug S    value of ${slug} (default: dir's git branch, slugified)

Reads <dir>/<F>, the env files it names and, without --slug, dir's git branch. Takes the state
lock, prunes owners whose directory is gone, and validates every owner's routes together with
Caddy before writing the state dir. If serve is running, the new config is loaded through the
admin socket; if not, the routes are saved and apply when serve starts. A rejected change leaves
every registered route as it was. Prints each URL it registered.
See "devingress help routefile" for the route file format.

Exit codes:
  0  registered, whether or not serve is running
  1  error: invalid route file, a host or prefix another checkout claims, or Caddy rejected it
  2  bad flag or argument

Examples:
  devingress up
  devingress up ../my-worktree --slug pr-123
`

func up(args []string, stdout io.Writer) error {
	fs := flags("up")
	file := fs.String("file", routefile.DefaultName, "route file, relative to dir")
	slug := fs.String("slug", "", "value of ${slug} (default: the git branch, slugified)")
	dir, err := dirArg(fs, args)
	if err != nil {
		return err
	}
	owner, err := routefile.Load(dir, *file, *slug)
	if err != nil {
		return err
	}
	res, err := ingress.Reconcile(func(owners []routefile.Owner) []routefile.Owner {
		owners = slices.DeleteFunc(owners, func(o routefile.Owner) bool { return o.Dir == dir })
		return append(owners, owner)
	})
	if err != nil {
		return err
	}
	report(stdout, res)
	for _, s := range owner.Sites {
		fmt.Fprintf(stdout, "  https://%s -> %s\n", s.Host, s.Upstream)
	}
	for _, p := range owner.Paths {
		fmt.Fprintf(stdout, "  http://<host>%s/* -> %s\n", p.Prefix, p.Upstream)
	}
	return nil
}

const downHelp = `Remove dir's routes. dir defaults to the current directory and is resolved as up resolves it.
Run it before deleting a worktree; a worktree deleted without it is pruned by the next up or down.

Takes the state lock, prunes owners whose directory is gone and removes dir's routes from the
state dir. If serve is running, the remaining routes are loaded through the admin socket; if not,
they apply when serve starts. A dir with no routes is not an error.

Exit codes:
  0  removed, or dir had no routes
  1  error reading the state dir or loading the config
  2  bad flag or argument

Example:
  devingress down
`

func down(args []string, stdout io.Writer) error {
	fs := flags("down")
	dir, err := dirArg(fs, args)
	if err != nil {
		return err
	}
	found := false
	res, err := ingress.Reconcile(func(owners []routefile.Owner) []routefile.Owner {
		return slices.DeleteFunc(owners, func(o routefile.Owner) bool {
			found = found || o.Dir == dir
			return o.Dir == dir
		})
	})
	if err != nil {
		return err
	}
	report(stdout, res)
	if !found && !slices.Contains(res.Pruned, dir) {
		fmt.Fprintf(stdout, "  %s had no routes\n", dir)
	} else if found {
		fmt.Fprintf(stdout, "  removed %s\n", dir)
	}
	return nil
}

func report(stdout io.Writer, res ingress.Result) {
	for _, d := range res.Pruned {
		fmt.Fprintf(stdout, "  pruned %s (directory is gone)\n", d)
	}
	if !res.Applied {
		fmt.Fprintln(stdout, "  devingress is not serving; routes apply when `devingress serve` starts")
	}
}

const statusHelp = `Print whether serve is running, then each registered owner: its directory, route file, slug
and URLs. An owner whose directory no longer exists is marked "[gone; pruned on next up/down]".
For output a program can parse, use "devingress routes --json".

Reads the state dir and checks whether the admin socket accepts connections. Changes nothing, and
works whether or not serve is running.

Exit codes:
  0  printed
  1  the state dir could not be read
  2  bad flag or argument

Example:
  devingress status
`

func status(args []string, stdout io.Writer) error {
	if err := noArgs("status", args); err != nil {
		return err
	}
	owners, err := state.New(config.StateDir()).List()
	if err != nil {
		return err
	}
	if ingress.Running() {
		fmt.Fprintf(stdout, "serving (admin %s)\n", config.AdminSocket())
	} else {
		fmt.Fprintln(stdout, "not serving")
	}
	for _, o := range owners {
		gone := ""
		if _, err := os.Stat(o.Dir); err != nil {
			gone = "  [gone; pruned on next up/down]"
		}
		fmt.Fprintf(stdout, "\n%s  (%s, slug %s)%s\n", o.Dir, o.File, o.Slug, gone)
		for _, s := range o.Sites {
			fmt.Fprintf(stdout, "  https://%s -> %s\n", s.Host, s.Upstream)
		}
		for _, p := range o.Paths {
			fmt.Fprintf(stdout, "  http://<host>%s/* -> %s\n", p.Prefix, p.Upstream)
		}
	}
	return nil
}

const caddyfileHelp = `Print the Caddyfile that config.toml and the registered routes render to: what serve loads,
except that serve first prunes owners whose directory is gone. Reads the config file and the
state dir; changes nothing, and works whether or not serve is running.

Exit codes:
  0  printed
  1  the config or state could not be read, or two owners claim the same host or prefix
  2  bad flag or argument

Example:
  devingress caddyfile
`

func caddyfile(args []string, stdout io.Writer) error {
	if err := noArgs("caddyfile", args); err != nil {
		return err
	}
	text, err := ingress.Caddyfile()
	if err != nil {
		return err
	}
	fmt.Fprint(stdout, text)
	return nil
}
