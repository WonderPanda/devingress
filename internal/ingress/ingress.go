// Package ingress reconciles registered owners into the running server and runs
// that server.
package ingress

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/caddyserver/caddy/v2"

	"github.com/WonderPanda/devingress/internal/config"
	"github.com/WonderPanda/devingress/internal/localdns"
	"github.com/WonderPanda/devingress/internal/remote"
	"github.com/WonderPanda/devingress/internal/render"
	"github.com/WonderPanda/devingress/internal/routefile"
	"github.com/WonderPanda/devingress/internal/state"
)

type Result struct {
	Owners  []routefile.Owner
	Pruned  []routefile.Owner // owners dropped because their directory is gone
	Applied bool              // false when the server is not running; routes apply on its next start
	config  []byte
}

// Reconcile applies mutate to the live owners, validates the merged config, and
// loads it. Owners whose directory is gone are pruned on every call. Nothing is
// persisted unless the new config validated and (when the server is up) loaded,
// so a rejected change leaves every other owner's routes as they were.
func Reconcile(mutate func([]routefile.Owner) []routefile.Owner) (Result, error) {
	cfg, err := config.Load()
	if err != nil {
		return Result{}, err
	}
	store := state.New(config.StateDir())
	unlock, err := store.Lock()
	if err != nil {
		return Result{}, err
	}
	defer unlock()

	before, err := store.List()
	if err != nil {
		return Result{}, err
	}
	var res Result
	var alive []routefile.Owner
	for _, o := range before {
		if _, err := os.Stat(o.Dir); err != nil {
			res.Pruned = append(res.Pruned, o)
			continue
		}
		alive = append(alive, o)
	}
	res.Owners = mutate(alive)

	cfgJSON, err := build(cfg, res.Owners)
	if err != nil {
		return Result{}, err
	}
	res.config = cfgJSON
	if Running() {
		if err := load(cfgJSON); err != nil {
			return Result{}, fmt.Errorf("server rejected the config: %w", cfg.Redact(err))
		}
		res.Applied = true
	}

	type id struct{ dir, name string }
	keep := map[id]bool{}
	for _, o := range res.Owners {
		keep[id{o.Dir, o.Name}] = true
		if err := store.Save(o); err != nil {
			return res, err
		}
	}
	for _, o := range before {
		if !keep[id{o.Dir, o.Name}] {
			if err := store.Remove(o.Dir, o.Name); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}

func build(cfg config.Config, owners []routefile.Owner) ([]byte, error) {
	text, err := render.Caddyfile(cfg.Render(), owners)
	if err != nil {
		return nil, err
	}
	cfgJSON, err := render.Adapt(text)
	return cfgJSON, cfg.Redact(err)
}

// Caddyfile renders the current state without changing anything.
func Caddyfile() (string, error) {
	cfg, err := config.Load()
	if err != nil {
		return "", err
	}
	owners, err := state.New(config.StateDir()).List()
	if err != nil {
		return "", err
	}
	return render.Caddyfile(cfg.Render(), owners)
}

// Provision validates the registered routes the way up does, without loading
// them into a running server. With no ca_root, provisioning creates Caddy's
// root in config.CaddyDir if it is not there yet.
func Provision() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	owners, err := state.New(config.StateDir()).List()
	if err != nil {
		return err
	}
	_, err = build(cfg, owners)
	return err
}

func adminClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", config.AdminSocket())
			},
		},
	}
}

func Running() bool {
	c, err := net.DialTimeout("unix", config.AdminSocket(), time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func load(cfgJSON []byte) error {
	req, err := http.NewRequest(http.MethodPost, "http://localhost/load", bytes.NewReader(cfgJSON))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := adminClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(body))
	}
	return nil
}

// Serve runs Caddy with every live owner's routes, the DNS listener when dns_listen is set, and in
// remote mode the updater that keeps *.<suffix> pointed at the tailnet address, until
// SIGINT/SIGTERM.
func Serve() error {
	if Running() {
		return fmt.Errorf("devingress is already serving on %s", config.AdminSocket())
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := log.New(os.Stderr, "devingress: ", log.LstdFlags)
	for _, w := range cfg.Warnings {
		logger.Printf("warning: %s", w)
	}
	if err := checkSocketPath(config.AdminSocket(), runtime.GOOS); err != nil {
		return err
	}
	if err := os.MkdirAll(config.RuntimeDir(), 0o700); err != nil {
		return err
	}
	// A socket file left by a crash makes the admin listener fail to bind.
	_ = os.Remove(config.AdminSocket())

	if cfg.DNSListen != "" {
		dns, err := localdns.Listen(cfg.DNSListen, localdns.Routed)
		if err != nil {
			return err
		}
		defer dns.Close()
	}

	// Prune before the first load so a dead owner's hosts never come up.
	res, err := Reconcile(func(o []routefile.Owner) []routefile.Owner { return o })
	if err != nil {
		return err
	}
	if err := caddy.Load(res.config, true); err != nil {
		return cfg.Redact(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if cfg.Remote != nil {
		u := &remote.Updater{
			Suffix:     cfg.Remote.Suffix,
			TokenFile:  cfg.Remote.TokenFile,
			Interfaces: remote.Interfaces,
			GOOS:       runtime.GOOS,
			Logf:       func(format string, args ...any) { logger.Printf("remote: "+format, args...) },
		}
		go u.Run(ctx)
	}
	<-ctx.Done()
	return caddy.Stop()
}

// checkSocketPath refuses an admin socket path longer than goos's sun_path holds, less its NUL:
// 107 bytes on Linux, 103 on macOS.
func checkSocketPath(sock, goos string) error {
	limit := 107
	if goos == "darwin" {
		limit = 103
	}
	if len(sock) > limit {
		return fmt.Errorf("admin socket path %s is %d bytes; Unix sockets allow %d (set a shorter XDG_RUNTIME_DIR)", sock, len(sock), limit)
	}
	return nil
}
