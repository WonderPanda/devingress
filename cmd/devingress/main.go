// devingress: one local HTTPS ingress for every project and worktree on a machine.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/WonderPanda/devingress/internal/config"
	"github.com/WonderPanda/devingress/internal/ingress"
	"github.com/WonderPanda/devingress/internal/routefile"
	"github.com/WonderPanda/devingress/internal/state"
)

const usage = `devingress: local HTTPS routing for projects and worktrees

  devingress serve                             run the proxy (normally under systemd)
  devingress up [dir] [--file F] [--slug S]    register dir's routes (default ./devingress.toml)
  devingress down [dir]                        remove dir's routes
  devingress status                            list owners and their hosts
  devingress caddyfile                         print the rendered Caddyfile

Every up/down also drops routes whose owner directory no longer exists.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "serve":
		err = ingress.Serve()
	case "up":
		err = up(args)
	case "down":
		err = down(args)
	case "status":
		err = status()
	case "caddyfile":
		var text string
		if text, err = ingress.Caddyfile(); err == nil {
			fmt.Print(text)
		}
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "devingress: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "devingress: %v\n", err)
		os.Exit(1)
	}
}

// dirArg parses flags that may come before or after the positional dir.
func dirArg(fs *flag.FlagSet, args []string) (string, error) {
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return "", err
		}
		args = fs.Args()
		if len(args) > 0 {
			pos, args = append(pos, args[0]), args[1:]
		}
	}
	if len(pos) > 1 {
		return "", fmt.Errorf("expected at most one directory, got %v", pos)
	}
	dir := "."
	if len(pos) == 1 {
		dir = pos[0]
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	// Resolve symlinks so one checkout is one owner however it was reached;
	// a directory that is already gone keeps its cleaned path.
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	return abs, nil
}

func up(args []string) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
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
	report(res)
	for _, s := range owner.Sites {
		fmt.Printf("  https://%s -> %s\n", s.Host, s.Upstream)
	}
	for _, p := range owner.Paths {
		fmt.Printf("  http://<host>%s/* -> %s\n", p.Prefix, p.Upstream)
	}
	return nil
}

func down(args []string) error {
	fs := flag.NewFlagSet("down", flag.ContinueOnError)
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
	report(res)
	if !found && !slices.Contains(res.Pruned, dir) {
		fmt.Printf("  %s had no routes\n", dir)
	} else if found {
		fmt.Printf("  removed %s\n", dir)
	}
	return nil
}

func report(res ingress.Result) {
	for _, d := range res.Pruned {
		fmt.Printf("  pruned %s (directory is gone)\n", d)
	}
	if !res.Applied {
		fmt.Println("  devingress is not serving; routes apply when `devingress serve` starts")
	}
}

func status() error {
	owners, err := state.New(config.StateDir()).List()
	if err != nil {
		return err
	}
	if ingress.Running() {
		fmt.Printf("serving (admin %s)\n", config.AdminSocket())
	} else {
		fmt.Println("not serving")
	}
	for _, o := range owners {
		gone := ""
		if _, err := os.Stat(o.Dir); err != nil {
			gone = "  [gone; pruned on next up/down]"
		}
		fmt.Printf("\n%s  (%s, slug %s)%s\n", o.Dir, o.File, o.Slug, gone)
		for _, s := range o.Sites {
			fmt.Printf("  https://%s -> %s\n", s.Host, s.Upstream)
		}
		for _, p := range o.Paths {
			fmt.Printf("  http://<host>%s/* -> %s\n", p.Prefix, p.Upstream)
		}
	}
	return nil
}
