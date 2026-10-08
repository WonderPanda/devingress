package config

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/WonderPanda/devingress/internal/render"
	"github.com/WonderPanda/devingress/internal/routefile"
)

func TestRenderBind(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	orig := goos
	t.Cleanup(func() { goos = orig })
	c := Config{Bind: []string{"127.0.0.1", "::1", "172.17.0.1"}, HTTPPort: 18080, HTTPSPort: 18443}
	owners := []routefile.Owner{{Dir: "/src/app", Sites: []routefile.Site{{Host: "web.example.dev", Upstream: "127.0.0.1:3000"}}}}

	for _, tc := range []struct {
		goos, bind string
		listen     []string
	}{
		{"linux", "default_bind 127.0.0.1 ::1 172.17.0.1",
			[]string{"127.0.0.1:18443", "172.17.0.1:18443", "[::1]:18443"}},
		{"darwin", "default_bind devingress-lo0/127.0.0.1 devingress-lo0/::1 172.17.0.1",
			[]string{"172.17.0.1:18443", "devingress-lo0/127.0.0.1:18443", "devingress-lo0/[::1]:18443"}},
	} {
		goos = tc.goos
		text, err := render.Caddyfile(c.Render(), owners)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(text, "\t"+tc.bind+"\n") {
			t.Errorf("%s: Caddyfile lacks %q:\n%s", tc.goos, tc.bind, text)
		}
		cfgJSON, err := render.Adapt(text)
		if err != nil {
			t.Fatalf("%s: adapt: %v", tc.goos, err)
		}
		var cfg struct {
			Apps struct {
				HTTP struct {
					Servers map[string]struct{ Listen []string } `json:"servers"`
				} `json:"http"`
			} `json:"apps"`
		}
		if err := json.Unmarshal(cfgJSON, &cfg); err != nil {
			t.Fatal(err)
		}
		var listen []string
		for _, srv := range cfg.Apps.HTTP.Servers {
			listen = append(listen, srv.Listen...)
		}
		if slices.Sort(listen); !slices.Equal(listen, tc.listen) {
			t.Errorf("%s: servers listen on %q, want %q", tc.goos, listen, tc.listen)
		}
	}
	if c.Bind[0] != "127.0.0.1" {
		t.Errorf("Render changed cfg.Bind: %q", c.Bind)
	}
}
