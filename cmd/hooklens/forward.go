package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/DinithiPramodya/hooklens/internal/tunnel"
)

// cliConfig is what lives in ~/.hooklens/config.json.
//
// Keyed by server URL rather than holding one inbox, because someone running
// a local hooklens for development and a hosted one for real work has two
// unrelated inboxes and no reason for the second to evict the first.
type cliConfig struct {
	Inboxes map[string]savedInbox `json:"inboxes"`
}

type savedInbox struct {
	Slug  string `json:"slug"`
	Token string `json:"token"`
}

// configPerms is 0600: owner read/write, nobody else.
//
// The file holds tokens that grant full access to an inbox's captured
// traffic, which routinely includes other people's secrets in webhook
// payloads. A world-readable file in a home directory is the sort of thing
// that is fine until the machine is shared, backed up, or synced.
const (
	configDirPerms  = 0o700
	configFilePerms = 0o600
)

func configPath() (string, error) {
	// UserConfigDir, not a hardcoded ~/.hooklens: it is %AppData% on Windows
	// and respects XDG_CONFIG_HOME on Linux, so the file lands where each
	// platform's tooling expects to find it.
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate config directory: %w", err)
	}
	return filepath.Join(dir, "hooklens", "config.json"), nil
}

func loadConfig() (*cliConfig, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		// A missing config is the first run, not a failure.
		return &cliConfig{Inboxes: map[string]savedInbox{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg cliConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		// Deliberately fatal rather than silently starting fresh. Overwriting
		// a corrupted file would destroy the token, and the token cannot be
		// recovered -- the server keeps only its hash.
		return nil, fmt.Errorf("parse %s: %w (delete it to start over, but the token cannot be recovered)", path, err)
	}
	if cfg.Inboxes == nil {
		cfg.Inboxes = map[string]savedInbox{}
	}
	return &cfg, nil
}

func saveConfig(cfg *cliConfig) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), configDirPerms); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	// Write to a temp file and rename. A rename within a directory is atomic,
	// so a crash mid-write leaves the old config intact rather than a
	// half-written one -- and half-written here means an unrecoverable token.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, configFilePerms); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// createInbox asks the server for a new inbox. Used on first run.
func createInbox(ctx context.Context, server string) (savedInbox, error) {
	url := strings.TrimSuffix(server, "/") + "/api/endpoints"
	req, err := http.NewRequestWithContext(ctx, "POST", url,
		strings.NewReader(`{"name":"cli"}`))
	if err != nil {
		return savedInbox{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return savedInbox{}, fmt.Errorf("create inbox: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return savedInbox{}, fmt.Errorf("create inbox: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out struct {
		Slug  string `json:"slug"`
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return savedInbox{}, fmt.Errorf("create inbox: %w", err)
	}
	if out.Slug == "" || out.Token == "" {
		return savedInbox{}, fmt.Errorf("create inbox: server returned no slug or token")
	}
	return savedInbox{Slug: out.Slug, Token: out.Token}, nil
}

// runForward is the `hooklens forward` subcommand.
func runForward(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("forward", flag.ContinueOnError)
	to := fs.String("to", "", "local address to forward to, e.g. localhost:3000")
	server := fs.String("server", envOr("HOOKLENS_SERVER", "http://localhost:8080"),
		"hooklens server URL")
	newInbox := fs.Bool("new", false, "create a fresh inbox instead of reusing the saved one")
	verbose := fs.Bool("v", false, "log every forwarded request")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: hooklens forward --to localhost:3000 [--server URL] [--new] [-v]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *to == "" {
		fs.Usage()
		return errors.New("--to is required")
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	key := strings.TrimSuffix(*server, "/")
	inbox, have := cfg.Inboxes[key]
	if *newInbox || !have {
		created, err := createInbox(ctx, key)
		if err != nil {
			return err
		}
		inbox = created
		cfg.Inboxes[key] = inbox
		// Saved BEFORE connecting. A token that exists on the server but not
		// on disk is unrecoverable -- the server stores only its hash -- so
		// the order here is not stylistic.
		if err := saveConfig(cfg); err != nil {
			return err
		}
		fmt.Printf("created a new inbox: %s\n", inbox.Slug)
	}

	level := slog.LevelWarn
	if *verbose {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	client, err := tunnel.NewClient(tunnel.ClientOptions{
		ServerURL: key,
		Slug:      inbox.Slug,
		Token:     inbox.Token,
		Target:    *to,
		Log:       log,
		OnConnect: func(publicURL string) {
			fmt.Printf("\n  forwarding  %s  ->  %s\n", publicURL, *to)
			fmt.Printf("  inbox       %s\n", inbox.Slug)
			fmt.Printf("  inspect     %s/\n\n", key)
			fmt.Printf("  ctrl-c to stop\n\n")
		},
	})
	if err != nil {
		return err
	}

	err = client.Run(ctx)
	if ctx.Err() != nil {
		// Interrupted. Not an error, and printing one on ctrl-c is noise.
		fmt.Println("\nstopped")
		return nil
	}
	return err
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
