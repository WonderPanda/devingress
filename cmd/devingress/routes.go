package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/WonderPanda/devingress/internal/config"
	"github.com/WonderPanda/devingress/internal/state"
)

const routesHelp = `List registered routes, grouped by owner directory: one block per owner, sorted by directory,
with a blank line between blocks. Each block starts with the directory (your home shortened to
~), followed by "[gone]" when it no longer exists (it is pruned on the next up or down). Under it,
indented, one route per line, URL then upstream: sites sorted by host, then path routes sorted by
prefix. --json prints a flat array instead, in the order documented below.

Flags:
  --dir D     only routes owned by D, resolved as up resolves dir (absolute, symlinks resolved)
  --host H    only the site whose host is exactly H
  --json      print a JSON array instead, one object per route (below)

Reads the state dir and config.toml: no lock and no admin socket, so it works whether or not
serve is running. It shows what the last up or down registered; serve must be running for the
URLs to answer.

JSON fields, always present:
  site  {"kind": "site", "url", "host", "upstream", "retry", "protocols", "dir", "file", "slug",
         "gone"}
  path  {"kind": "path", "prefix", "upstream", "strip", "dir", "file", "slug", "gone"}
  url is ready to use: "https://<host>", plus ":<https_port>" when config.toml sets one other
  than 443. retry is "" when unset, protocols a list ([] for the default h1 and h2), strip and
  gone booleans. dir is the owner's absolute directory, file its route file and slug its
  ${slug}. Sites come first, sorted by host then dir, then path routes, sorted by prefix then
  dir. No match prints [].

Exit codes:
  0  printed; with no filter, an empty registry prints nothing (or [])
  1  --dir or --host matched nothing (reason on stderr), or the state dir or config.toml could
     not be read
  2  bad flag or argument

Examples:
  devingress routes --dir . --json           # this checkout's URLs
  devingress routes --host web-x.example.dev >/dev/null && echo registered
`

type siteRoute struct {
	Kind      string   `json:"kind"`
	URL       string   `json:"url"`
	Host      string   `json:"host"`
	Upstream  string   `json:"upstream"`
	Retry     string   `json:"retry"`
	Protocols []string `json:"protocols"`
	Dir       string   `json:"dir"`
	File      string   `json:"file"`
	Slug      string   `json:"slug"`
	Gone      bool     `json:"gone"`
}

type pathRoute struct {
	Kind     string `json:"kind"`
	Prefix   string `json:"prefix"`
	Upstream string `json:"upstream"`
	Strip    bool   `json:"strip"`
	Dir      string `json:"dir"`
	File     string `json:"file"`
	Slug     string `json:"slug"`
	Gone     bool   `json:"gone"`
}

// ownerRoutes is one owner's routes that pass the filters.
type ownerRoutes struct {
	dir   string
	gone  bool
	sites []siteRoute
	paths []pathRoute
}

func routes(args []string, stdout io.Writer) error {
	fs := flags("routes")
	dirFlag := fs.String("dir", "", "only routes owned by this directory")
	host := fs.String("host", "", "only the site with exactly this host")
	asJSON := fs.Bool("json", false, "print a JSON array")
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Errorf("unexpected argument %q", fs.Arg(0))}
	}
	dir := ""
	if *dirFlag != "" {
		var err error
		if dir, err = ownerDir(*dirFlag); err != nil {
			return err
		}
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	owners, err := state.New(config.StateDir()).List()
	if err != nil {
		return err
	}
	// One group per owner with matching routes, in the dir order List returns.
	var groups []ownerRoutes
	for _, o := range owners {
		if dir != "" && o.Dir != dir {
			continue
		}
		_, statErr := os.Stat(o.Dir)
		g := ownerRoutes{dir: o.Dir, gone: statErr != nil}
		for _, s := range o.Sites {
			if *host != "" && s.Host != *host {
				continue
			}
			g.sites = append(g.sites, siteRoute{Kind: "site", URL: siteURL(s.Host, cfg.HTTPSPort), Host: s.Host, Upstream: s.Upstream,
				Retry: s.Retry, Protocols: append([]string{}, s.Protocols...), Dir: o.Dir, File: o.File, Slug: o.Slug, Gone: g.gone})
		}
		if *host == "" { // --host selects a site; path routes have no host
			for _, p := range o.Paths {
				g.paths = append(g.paths, pathRoute{Kind: "path", Prefix: p.Prefix, Upstream: p.Upstream, Strip: p.Strip,
					Dir: o.Dir, File: o.File, Slug: o.Slug, Gone: g.gone})
			}
		}
		if len(g.sites)+len(g.paths) > 0 {
			groups = append(groups, g)
		}
	}

	if *asJSON {
		sites, paths := []siteRoute{}, []pathRoute{}
		for _, g := range groups {
			sites, paths = append(sites, g.sites...), append(paths, g.paths...)
		}
		slices.SortFunc(sites, func(a, b siteRoute) int { return cmp.Or(cmp.Compare(a.Host, b.Host), cmp.Compare(a.Dir, b.Dir)) })
		slices.SortFunc(paths, func(a, b pathRoute) int { return cmp.Or(cmp.Compare(a.Prefix, b.Prefix), cmp.Compare(a.Dir, b.Dir)) })
		all := make([]any, 0, len(sites)+len(paths))
		for _, s := range sites {
			all = append(all, s)
		}
		for _, p := range paths {
			all = append(all, p)
		}
		out, err := json.MarshalIndent(all, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s\n", out)
	} else {
		// A header line has no tab, so it ends tabwriter's column block: each
		// owner's routes align among themselves.
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		home, _ := os.UserHomeDir()
		for i, g := range groups {
			if i > 0 {
				fmt.Fprintln(tw)
			}
			fmt.Fprintf(tw, "%s%s\n", tildeHome(g.dir, home), goneMark(g.gone))
			slices.SortFunc(g.sites, func(a, b siteRoute) int { return cmp.Compare(a.Host, b.Host) })
			slices.SortFunc(g.paths, func(a, b pathRoute) int { return cmp.Compare(a.Prefix, b.Prefix) })
			for _, s := range g.sites {
				fmt.Fprintf(tw, "  %s\t%s\n", s.URL, s.Upstream)
			}
			for _, p := range g.paths {
				fmt.Fprintf(tw, "  http://<host>%s/*\t%s\n", p.Prefix, p.Upstream)
			}
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	if len(groups) > 0 {
		return nil
	}
	switch {
	case dir != "" && *host != "":
		return fmt.Errorf("no site with host %s is owned by %s", *host, dir)
	case dir != "":
		return fmt.Errorf("no routes are owned by %s", dir)
	case *host != "":
		return fmt.Errorf("no site has host %s", *host)
	}
	return nil
}

func siteURL(host string, httpsPort int) string {
	if httpsPort == 443 {
		return "https://" + host
	}
	return fmt.Sprintf("https://%s:%d", host, httpsPort)
}

// tildeHome shortens home, or a directory under it, to ~ for display. An empty
// home (it could not be determined) leaves dir as is.
func tildeHome(dir, home string) string {
	if home == "" {
		return dir
	}
	home = filepath.Clean(home)
	if dir == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(dir, home+"/"); ok {
		return "~/" + rest
	}
	return dir
}

func goneMark(gone bool) string {
	if gone {
		return "  [gone]"
	}
	return ""
}
