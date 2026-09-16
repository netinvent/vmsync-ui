/*
	Copyright (C) 2026  Orsiris de Jong <ozy@netpower.fr>

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

// vmsync-ui is the control-plane console for vmsync replication.
//
// It serves two audiences over one HTTPS listener: agents, which enrol and
// report over /api/v1, and operators, who read the availability pages. It
// holds no hypervisor credentials, reaches into no host, and never touches
// libvirt -- agents dial in, and everything it knows comes from what they
// report.
//
// Phase 2: the console is read-only about replication itself. The only
// actions are enrolling and revoking agents.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"vmsync-ui/internal/api"
	"vmsync-ui/internal/auth"
	"vmsync-ui/internal/store"
	"vmsync-ui/internal/web"
)

// Config is the on-disk configuration. JSON because it is stdlib, and
// because this file is edited rarely and read by a program that must not
// guess at its meaning.
type Config struct {
	// Listen is the address to serve on, e.g. ":8443".
	Listen string `json:"listen"`
	// TLSCert/TLSKey are required unless Insecure is set.
	TLSCert string `json:"tls_cert"`
	TLSKey  string `json:"tls_key"`
	// Insecure serves plain HTTP. For local development only -- agents
	// refuse a non-https UI outright, so this cannot be used in anger.
	Insecure bool `json:"insecure,omitempty"`
	// StateDir holds agents, reports and the audit log.
	StateDir string `json:"state_dir"`
	// GrafanaURL, when set, adds a "Trends" link. This console deliberately
	// does not draw time series; Grafana already does that well.
	GrafanaURL string `json:"grafana_url,omitempty"`
	// SessionHours bounds how long a sign-in lasts.
	SessionHours int `json:"session_hours,omitempty"`

	Accounts []auth.Account `json:"accounts"`
}

func main() {
	var (
		configPath   = flag.String("config", "/etc/vmsync-ui/vmsync-ui.conf", "Path to the configuration file")
		hashPassword = flag.Bool("hash-password", false, "Read a password from stdin and print its hash for the config file, then exit")
		debug        = flag.Bool("debug", false, "Enable debug logging")
	)
	flag.Parse()
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "vmsync-ui: unexpected argument(s) %v\n", flag.Args())
		os.Exit(2)
	}

	if *hashPassword {
		if err := runHashPassword(); err != nil {
			fmt.Fprintf(os.Stderr, "vmsync-ui: %v\n", err)
			os.Exit(1)
		}
		return
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if err := run(*configPath, log); err != nil {
		log.Error("vmsync-ui stopped", "error", err)
		os.Exit(1)
	}
}

// runHashPassword exists so nobody ever has to paste a plaintext password
// into the config and hope something hashes it later.
func runHashPassword() error {
	fmt.Fprint(os.Stderr, "Password: ")
	var password string
	if _, err := fmt.Fscanln(os.Stdin, &password); err != nil {
		return fmt.Errorf("read password: %w", err)
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	fmt.Println(hash)
	return nil
}

func run(configPath string, log *slog.Logger) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	st, err := store.Open(cfg.StateDir)
	if err != nil {
		return err
	}
	am, err := auth.NewManager(cfg.Accounts, time.Duration(cfg.SessionHours)*time.Hour)
	if err != nil {
		return fmt.Errorf("accounts: %w", err)
	}
	wsrv, err := web.New(st, am, log, !cfg.Insecure, cfg.GrafanaURL)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	(&api.Server{Store: st, Log: log, PollInterval: time.Second}).Routes(mux)
	wsrv.Routes(mux)

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: mux,
		// ReadHeaderTimeout guards against a slowloris holding connections
		// open. There is deliberately NO WriteTimeout: agents long-poll the
		// config endpoint for up to five minutes, and a write deadline would
		// cut those off mid-hold and turn the whole long-poll design into a
		// stream of client-side errors.
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("vmsync-ui listening", "addr", cfg.Listen, "tls", !cfg.Insecure, "accounts", len(cfg.Accounts))
		if cfg.Insecure {
			errCh <- srv.ListenAndServe()
			return
		}
		errCh <- srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		// Bounded: an in-flight long poll may be held for minutes, and
		// waiting it out on shutdown would look like a hang. Agents retry.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := Config{
		Listen:       ":8443",
		StateDir:     "/var/lib/vmsync-ui",
		SessionHours: 12,
	}
	// DisallowUnknownFields so a mistyped key is a startup error rather than
	// a setting that silently does nothing. In a file that carries TLS paths
	// and account roles, "quietly ignored" is the worst possible handling of
	// a typo.
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}

	if len(cfg.Accounts) == 0 {
		return Config{}, fmt.Errorf("config %s defines no accounts: nobody could sign in", path)
	}
	for i, a := range cfg.Accounts {
		if strings.HasPrefix(a.PasswordHash, "pbkdf2_sha256$") {
			continue
		}
		// Refusing outright rather than hashing it here: a plaintext
		// password in a config file is a thing someone copies, backs up and
		// forgets. Fail loudly with the exact command that fixes it.
		return Config{}, fmt.Errorf("account %d (%q): password_hash is not a hash -- generate one with: vmsync-ui -hash-password", i, a.Username)
	}
	if !cfg.Insecure && (cfg.TLSCert == "" || cfg.TLSKey == "") {
		return Config{}, fmt.Errorf("config %s: tls_cert and tls_key are required (or set \"insecure\": true for local development, which agents will refuse to talk to)", path)
	}
	return cfg, nil
}
