// registry-mirrors-relay is a container registry mirror that only relays requests and never writes
// image data to disk.
//
//	registry-mirrors-relay [serve] [-config file]   start the service (the default subcommand)
//	registry-mirrors-relay check [-config file] [-print]
//	registry-mirrors-relay hash-password [-user name] [-cost 12]
//	registry-mirrors-relay admin create|password|list [-config file] [-name name] [-role admin|viewer]
//	registry-mirrors-relay probe [-url http://127.0.0.1:8080/healthz] [-wait 60s]
//	registry-mirrors-relay version
package main

import (
	"bufio"
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"

	"github.com/openlinkos/registry-mirrors-relay/internal/app"
	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/mgmt"
	"github.com/openlinkos/registry-mirrors-relay/internal/ops"
	"github.com/openlinkos/registry-mirrors-relay/internal/relay"
	"github.com/openlinkos/registry-mirrors-relay/internal/server"
	"github.com/openlinkos/registry-mirrors-relay/internal/store"
	"github.com/openlinkos/registry-mirrors-relay/internal/version"
)

// defaultConfigPath is the configuration file read when there is neither -config nor RELAY_CONFIG. When it
// does not exist, the service starts with defaults only.
const defaultConfigPath = "/etc/registry-mirrors-relay/config.yaml"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return cmdServe(args, stderr)
	case "check":
		return cmdCheck(args, stdout, stderr)
	case "hash-password":
		return cmdHashPassword(args, stdin, stdout, stderr)
	case "admin":
		return cmdAdmin(args, stdin, stdout, stderr)
	case "probe":
		return cmdProbe(args, stderr)
	case "version", "-version", "--version":
		fmt.Fprintln(stdout, version.String())
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	}
	fmt.Fprintf(stderr, "Unknown subcommand %q.\n\n", cmd)
	usage(stderr)
	return 2
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  registry-mirrors-relay [serve] [-config file]         Start the service (default)
  registry-mirrors-relay check [-config file] [-print]  Validate the configuration; -print prints the effective configuration (secrets hidden)
  registry-mirrors-relay hash-password [-user name]     Read a password from standard input and print its bcrypt hash (htpasswd format)
  registry-mirrors-relay admin create -name name [-role admin|viewer]   Create a console administrator (password read from standard input)
  registry-mirrors-relay admin password -name name      Reset a console administrator's password (and enable the account)
  registry-mirrors-relay admin list                     List the console administrators
                                                        The admin subcommands need the service stopped first:
                                                        only one process can open the operational database at a time
  registry-mirrors-relay probe [-config file] [-url address] [-wait duration]
                                                        Health check for the container's HEALTHCHECK; uses the configured port by default
  registry-mirrors-relay version

The configuration file is taken from -config, then the environment variable RELAY_CONFIG, then `+defaultConfigPath+`;
when there is none, only the defaults are used.
`)
}

// loadConfig looks for the configuration in -config, RELAY_CONFIG and the default path, in that order. An
// explicitly given file that does not exist is an error; when the default path does not exist, only the
// defaults are used (so docker run works without mounting a configuration).
func loadConfig(flagPath string) (*config.Result, string, error) {
	path, explicit := flagPath, flagPath != ""
	if path == "" {
		if env := os.Getenv("RELAY_CONFIG"); env != "" {
			path, explicit = env, true
		} else {
			path = defaultConfigPath
		}
	}
	res, err := config.Load(path)
	if errors.Is(err, fs.ErrNotExist) && !explicit {
		res, err = config.LoadDefaults()
		return res, "", err
	}
	return res, path, err
}

func newLogger(level string, w io.Writer) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: l}))
}

func cmdServe(args []string, stderr io.Writer) int {
	fl := flag.NewFlagSet("serve", flag.ContinueOnError)
	fl.SetOutput(stderr)
	cfgPath := fl.String("config", "", "configuration file")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	res, path, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	log := newLogger(res.Config.Log.Level, stderr)
	if path == "" {
		log.Warn("no configuration file found; starting with defaults only", "looked_for", defaultConfigPath)
	} else {
		log.Info("configuration loaded", "path", path)
	}
	for _, w := range res.Warnings {
		log.Warn(w)
	}
	log.Info("starting", "version", version.Version)
	server.SetMemoryLimit(log)

	var raw []byte
	if path != "" {
		raw, _ = os.ReadFile(path) //nolint:gosec // the configuration file path is set by the operator
	}
	a, err := app.New(res, raw, app.Options{ConfigPath: path, Log: log})
	if err != nil {
		log.Error("failed to start", "err", err)
		return 1
	}
	defer a.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go a.Watch(ctx)          // reload automatically when the configuration file changes
	go a.Stats.Run(ctx)      // save the statistics to the operational database every hour
	go a.WatchUpstreams(ctx) // probe each upstream's /v2/ every minute
	watchSignals(ctx, a, log)

	m, err := mgmt.New(a, log)
	if err != nil {
		log.Error("failed to start", "err", err)
		return 1
	}
	cfg := res.Config
	var pull http.Handler = relay.New(relay.Deps{App: a, Log: log})
	var extras []server.Listener
	if cfg.Listen.Admin == "" {
		pull = m.Shared(pull) // all in one: the console, API, metrics and pulls share one port
	} else {
		extras = append(extras, server.Listener{Name: "management entrance", Addr: cfg.Listen.Admin, Handler: m.Standalone(log)})
	}
	log.Info("management entrance", "listen", cmp.Or(cfg.Listen.Admin, "shared with the pull entrance"), "public", cfg.Admin.Public, "allow", cfg.Admin.Allow,
		"proxied_allow", cfg.Admin.ProxiedAllow, "metrics", cfg.Metrics.Enabled)
	srv := server.New(cfg, log, pull, extras...)
	srv.ACMEHosts(func() []string { return a.Runtime().Config.TLS.ACME.Hosts })
	if err := srv.Run(ctx); err != nil {
		log.Error("exiting", "err", err)
		return 1
	}
	log.Info("exited")
	return 0
}

func cmdCheck(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("check", flag.ContinueOnError)
	fl.SetOutput(stderr)
	cfgPath := fl.String("config", "", "configuration file")
	print := fl.Bool("print", false, "print the effective configuration (secrets hidden)")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	res, path, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if path == "" {
		fmt.Fprintf(stderr, "No configuration file found (%s); checking the defaults.\n", defaultConfigPath)
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(stderr, "Warning:", w)
	}
	if *print {
		b, err := res.Config.Print()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		_, _ = stdout.Write(b)
	} else {
		fmt.Fprintln(stdout, "The configuration is valid.")
	}
	return 0
}

func cmdHashPassword(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("hash-password", flag.ContinueOnError)
	fl.SetOutput(stderr)
	user := fl.String("user", "", "username; when given, the output is username:hash, ready to append to an htpasswd file")
	cost := fl.Int("cost", 12, "bcrypt cost (10–14)")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if *cost < 10 || *cost > 14 {
		fmt.Fprintln(stderr, "-cost must be between 10 and 14.")
		return 2
	}
	pw, err := readPassword(stdin, stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), *cost)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *user != "" {
		if strings.ContainsAny(*user, ":\r\n") {
			fmt.Fprintln(stderr, "The username must not contain a colon or a line break.")
			return 2
		}
		fmt.Fprintf(stdout, "%s:%s\n", *user, h)
	} else {
		fmt.Fprintln(stdout, string(h))
	}
	return 0
}

// readPassword reads the password on a terminal without echoing it (typed twice); when reading from a
// pipe, it reads only the first line.
func readPassword(stdin io.Reader, stderr io.Writer) (string, error) {
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(stderr, "Password: ")
		a, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(stderr)
		if err != nil {
			return "", err
		}
		fmt.Fprint(stderr, "Password again: ")
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(stderr)
		if err != nil {
			return "", err
		}
		if string(a) != string(b) {
			return "", errors.New("the two passwords do not match")
		}
		return checkPassword(string(a))
	}
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return checkPassword(strings.TrimRight(line, "\r\n"))
}

func checkPassword(pw string) (string, error) {
	switch {
	case len(pw) < 12:
		return "", errors.New("the password must be at least 12 characters long")
	case len(pw) > 72:
		return "", errors.New("the password must be at most 72 bytes long (the bcrypt limit)")
	}
	return pw, nil
}

// probeURL derives the local /healthz address from the listen address: when only a port is given (:8080)
// or the service listens on all addresses, it uses 127.0.0.1.
func probeURL(l config.Listen) (url string, insecure bool, err error) {
	scheme, addr := "http", l.HTTP
	if addr == "" {
		scheme, addr, insecure = "https", l.HTTPS, true
	}
	if addr == "" {
		return "", false, errors.New("neither listen.http nor listen.https is enabled; give the address to probe with -url")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", false, fmt.Errorf("malformed listen address %q", addr)
	}
	if ip, perr := netip.ParseAddr(host); host == "" || (perr == nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return scheme + "://" + net.JoinHostPort(host, port) + "/healthz", insecure, nil
}

// cmdAdmin manages the console administrators while the service is stopped: create, password, list. It
// opens the operational database directly (bbolt can be opened by only one process at a time, so with the
// service running it fails after 2 seconds). Operations are written to the audit log with the actor
// cli:<system user>.
func cmdAdmin(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, "Usage: registry-mirrors-relay admin create|password|list [-config file] [-name name] [-role admin|viewer]")
		return 2
	}
	sub, args := args[0], args[1:]
	fl := flag.NewFlagSet("admin "+sub, flag.ContinueOnError)
	fl.SetOutput(stderr)
	cfgPath := fl.String("config", "", "configuration file")
	name := fl.String("name", "", "the administrator's username")
	role := fl.String("role", "admin", "role: admin (can make changes) or viewer (read-only)")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	res, _, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	cfg := res.Config
	if cfg.Admin.Store.Driver != "bolt" {
		fmt.Fprintln(stderr, "Operational data is read-only (admin.store.driver: none): the administrator comes from admin.initial_admin in the configuration.")
		return 1
	}
	if sub != "list" && !ops.ValidName(*name) {
		fmt.Fprintln(stderr, "Give the username with -name: letters, digits, dots, underscores and hyphens, starting with a letter or digit, at most 64 characters.")
		return 2
	}
	st, err := store.OpenBolt(cfg.Admin.Store.Path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		fmt.Fprintln(stderr, "If the service is running, stop it first, or make the change in the web console.")
		return 1
	}
	defer func() { _ = st.Close() }()
	actor := "cli:" + cmp.Or(os.Getenv("USER"), os.Getenv("USERNAME"), "unknown")

	switch sub {
	case "list":
		for _, a := range st.Data().Admins {
			status := "active"
			if a.Disabled {
				status = "disabled"
			}
			fmt.Fprintln(stdout, strings.Join([]string{a.Name, a.Role, status, a.Created.Format(time.RFC3339)}, "  "))
		}
		return 0
	case "create", "password":
	default:
		fmt.Fprintf(stderr, "Unknown admin subcommand %q (create, password, list).\n", sub)
		return 2
	}
	if *role != "admin" && *role != "viewer" {
		fmt.Fprintln(stderr, "-role must be admin or viewer.")
		return 2
	}
	pw, err := readPassword(stdin, stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	hash, err := ops.HashPassword(pw)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	err = st.Update(func(d *store.Data) error {
		for i := range d.Admins {
			if d.Admins[i].Name != *name {
				continue
			}
			if sub == "create" {
				return fmt.Errorf("administrator %s already exists; use admin password to reset the password", *name)
			}
			d.Admins[i].Hash, d.Admins[i].Disabled, d.Admins[i].MustChange = hash, false, false
			return nil
		}
		if sub == "password" {
			return fmt.Errorf("there is no administrator %s; use admin create to create one", *name)
		}
		d.Admins = append(d.Admins, store.Admin{Name: *name, Hash: hash, Role: *role, Created: time.Now().UTC()})
		return nil
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	action := "admin.create"
	if sub == "password" {
		action = "admin.update"
	}
	_ = st.Audit(store.AuditEntry{Actor: actor, Action: action, Target: *name, Detail: "cli"})
	if sub == "create" {
		fmt.Fprintf(stdout, "Created the administrator %s (%s).\n", *name, *role)
	} else {
		fmt.Fprintf(stdout, "Reset the password of %s.\n", *name)
	}
	return 0
}

// cmdProbe is the container's HEALTHCHECK: it requests /healthz from the local pull entrance. Without -url
// it builds the address from listen.http in the configuration (or listen.https when that is off, without
// verifying the certificate), so changing the port does not require changing the health check.
// With -wait, a failure is retried every second until it succeeds or the time is up: helm test uses this,
// because a newly created Service takes a while to become reachable.
func cmdProbe(args []string, stderr io.Writer) int {
	fl := flag.NewFlagSet("probe", flag.ContinueOnError)
	fl.SetOutput(stderr)
	cfgPath := fl.String("config", "", "configuration file (used to find the port when -url is not given)")
	url := fl.String("url", "", "address to probe; defaults to the configured listen.http or listen.https")
	timeout := fl.Duration("timeout", 3*time.Second, "timeout for each request")
	wait := fl.Duration("wait", 0, "on failure, retry every second for up to this long")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	client := http.DefaultClient
	if *url == "" {
		res, _, err := loadConfig(*cfgPath)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		u, insecure, err := probeURL(res.Config.Listen)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		*url = u
		if insecure {
			// the probe goes to 127.0.0.1 on this machine, which is not on the certificate; it only checks
			// that the service is up
			client = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // see above
		}
	}
	if _, err := http.NewRequestWithContext(context.Background(), http.MethodGet, *url, nil); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	deadline := time.Now().Add(*wait)
	for {
		err := probeOnce(client, *url, *timeout)
		if err == nil {
			return 0
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(stderr, err)
			return 1
		}
		time.Sleep(time.Second)
	}
}

func probeOnce(client *http.Client, url string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
