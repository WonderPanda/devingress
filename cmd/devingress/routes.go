package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"text/tabwriter"

	"github.com/WonderPanda/devingress/internal/config"
	"github.com/WonderPanda/devingress/internal/state"
)

const routesHelp = `List registered routes, one per line: URL, upstream, owner directory, and "[gone]" when the
owner's directory no longer exists (it is pruned on the next up or down). Sites come first, sorted
by host, then path routes, sorted by prefix.

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
  gone booleans. dir is the owner's directory, file its route file and slug its ${slug}. No
  match prints [].

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
	sites, paths := []siteRoute{}, []pathRoute{}
	for _, o := range owners {
		if dir != "" && o.Dir != dir {
			continue
		}
		_, statErr := os.Stat(o.Dir)
		gone := statErr != nil
		for _, s := range o.Sites {
			if *host != "" && s.Host != *host {
				continue
			}
			sites = append(sites, siteRoute{Kind: "site", URL: siteURL(s.Host, cfg.HTTPSPort), Host: s.Host, Upstream: s.Upstream,
				Retry: s.Retry, Protocols: append([]string{}, s.Protocols...), Dir: o.Dir, File: o.File, Slug: o.Slug, Gone: gone})
		}
		if *host != "" {
			continue // --host selects a site; path routes have no host
		}
		for _, p := range o.Paths {
			paths = append(paths, pathRoute{Kind: "path", Prefix: p.Prefix, Upstream: p.Upstream, Strip: p.Strip,
				Dir: o.Dir, File: o.File, Slug: o.Slug, Gone: gone})
		}
	}
	slices.SortFunc(sites, func(a, b siteRoute) int { return cmp.Or(cmp.Compare(a.Host, b.Host), cmp.Compare(a.Dir, b.Dir)) })
	slices.SortFunc(paths, func(a, b pathRoute) int { return cmp.Or(cmp.Compare(a.Prefix, b.Prefix), cmp.Compare(a.Dir, b.Dir)) })

	if *asJSON {
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
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		for _, s := range sites {
			fmt.Fprintf(tw, "%s\t%s\t%s%s\n", s.URL, s.Upstream, s.Dir, goneMark(s.Gone))
		}
		for _, p := range paths {
			fmt.Fprintf(tw, "http://<host>%s/*\t%s\t%s%s\n", p.Prefix, p.Upstream, p.Dir, goneMark(p.Gone))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	if len(sites)+len(paths) > 0 {
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

func goneMark(gone bool) string {
	if gone {
		return "  [gone]"
	}
	return ""
}
