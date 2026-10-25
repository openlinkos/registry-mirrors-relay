// registry-mirrors-relay is a container registry mirror that only relays requests and never writes
// image data to disk.
//
//	registry-mirrors-relay [serve] [-config file]   start the service (the default subcommand)
//	registry-mirrors-relay check [-config file] [-print]
//	registry-mirrors-relay hash-password [-user name] [-cost 12]
//	registry-mirrors-relay admin list | create name [-role admin|viewer] | password [name] [--generate]
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
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"

	"github.com/openlinkos/registry-mirrors-relay/internal/app"
	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/ctl"
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
  registry-mirrors-relay admin list                     List the console administrators
  registry-mirrors-relay admin password [name]          Set an administrator's password (asked twice on a terminal,
                                                        or one line on standard input) and enable the account; without
                                                        a name, the only administrator, or a new one called admin
  registry-mirrors-relay admin create name [-role admin|viewer]
                                                        Create a console administrator
                                                        --generate makes a random password and prints it once.
                                                        They work while the service runs (through its local admin
                                                        socket), e.g. docker exec -it relay registry-mirrors-relay admin password
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
		log.Warn(w.String())
	}
	log.Info("starting", "version", version.Version)
	server.SetMemoryLimit(log)

	var raw []byte
	if path != "" {
		raw, _ = os.ReadFile(path) //nolint:gosec // the configuration file path is set by the operator
	}
	a, err := app.New(res, raw, app.Options{ConfigPath: path, Log: log, GenerateAdmin: true})
	if err != nil {
		log.Error("failed to start", "err", err)
		return 1
	}
	defer a.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go a.Watch(ctx)          // reload automatically when the configuration file changes
	go a.Stats.Run(ctx)      // save the statistics to the operational database every hour
	go a.WatchUpstreams(ctx) // probe each upstream's /v2/ every proxy.health_interval
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
	// The local admin socket lets `registry-mirrors-relay admin …` work while the service runs
	if a.Store.Writable() {
		sock := ctl.Path(cfg.Admin.Store.Path)
		switch ln, err := ctl.Listen(sock); {
		case errors.Is(err, errors.ErrUnsupported):
		case err != nil:
			log.Warn("admin socket unavailable; the admin subcommands need the service stopped", "path", sock, "err", err)
		default:
			extras = append(extras, server.Listener{Name: "admin socket", Listener: ln, Handler: ctl.Handler(m.Ops())})
		}
	}
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
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", errors.New("no password on standard input: pipe it in as one line, run the command in a terminal " +
			"(docker exec -it, kubectl exec -it), or use --generate")
	}
	return checkPassword(line)
}

func checkPassword(pw string) (string, error) {
	if err := ops.CheckPassword(pw); err != nil {
		return "", err
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

// cmdAdmin manages the console administrators: list, create, password. While the service runs it makes
// the change through the service's local admin socket, so the change applies at once and the
// administrator's console sessions are signed out; while the service is stopped it opens the
// operational database itself. Changes are written to the audit log with the actor cli:<system user>.
func cmdAdmin(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	const help = "Usage: registry-mirrors-relay admin list | create NAME [-role admin|viewer] [--generate] | password [NAME] [--generate] [-config file]"
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, help)
		return 2
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "list", "create", "password":
	default:
		fmt.Fprintf(stderr, "Unknown admin subcommand %q.\n%s\n", sub, help)
		return 2
	}
	fl := flag.NewFlagSet("admin "+sub, flag.ContinueOnError)
	fl.SetOutput(stderr)
	cfgPath := fl.String("config", "", "configuration file")
	name := fl.String("name", "", "the administrator's username (or give it as the first argument)")
	role := fl.String("role", "admin", "role for create: admin (can make changes) or viewer (read-only)")
	generate := fl.Bool("generate", false, "generate a random password and print it once, instead of reading one")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	// The name may come before or after the flags
	if fl.NArg() > 0 {
		*name = fl.Arg(0)
		if err := fl.Parse(fl.Args()[1:]); err != nil {
			return 2
		}
		if fl.NArg() > 0 {
			fmt.Fprintln(stderr, help)
			return 2
		}
	}
	if *role != "admin" && *role != "viewer" {
		fmt.Fprintln(stderr, "-role must be admin or viewer.")
		return 2
	}
	res, _, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	cfg := res.Config
	if cfg.Admin.Store.Driver != "bolt" {
		fmt.Fprintln(stderr, "The operational data is read-only (admin.store.driver: none), so the administrator comes from the configuration:")
		fmt.Fprintln(stderr, "admin.initial_admin, the file named by RELAY_ADMIN_PASSWORD_FILE, or the Helm value admin.existingSecret.")
		fmt.Fprintln(stderr, "Change the password there, then restart the service (or the pods).")
		return 1
	}
	ctx := context.Background()
	b, closeFn, err := openAdmins(ctx, cfg, sub == "list" || (sub == "password" && *name != ""))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer closeFn()

	admins, err := b.Admins(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if sub == "list" {
		for _, a := range admins {
			status := "active"
			switch {
			case a.Disabled:
				status = "disabled"
			case a.MustChange:
				status = "must-change-password"
			}
			fmt.Fprintln(stdout, strings.Join([]string{a.Name, a.Role, status, a.Created.Format(time.RFC3339)}, "  "))
		}
		if len(admins) == 0 {
			fmt.Fprintln(stderr, "There are no administrators yet. Create one with: registry-mirrors-relay admin password")
		}
		return 0
	}

	// Without a name, password means the only administrator, or a new one called admin when there is none
	if *name == "" && sub == "password" {
		switch len(admins) {
		case 0:
			*name, sub = "admin", "create"
		case 1:
			*name = admins[0].Name
		default:
			names := make([]string, len(admins))
			for i, a := range admins {
				names[i] = a.Name
			}
			fmt.Fprintf(stderr, "There are several administrators (%s); name the one to change.\n", strings.Join(names, ", "))
			return 2
		}
	}
	if !ops.ValidName(*name) {
		fmt.Fprintln(stderr, "Give the administrator's username: letters, digits, dots, underscores and hyphens, starting with a letter or digit, at most 64 characters.")
		return 2
	}
	exists := slices.ContainsFunc(admins, func(a ctl.Admin) bool { return a.Name == *name })
	switch {
	case sub == "create" && exists:
		fmt.Fprintf(stderr, "The administrator %s already exists; set its password with: registry-mirrors-relay admin password %s\n", *name, *name)
		return 1
	case sub == "password" && !exists:
		fmt.Fprintf(stderr, "There is no administrator %s; create it with: registry-mirrors-relay admin create %s\n", *name, *name)
		return 1
	}

	var pw string
	if *generate {
		pw = ops.GeneratePassword()
	} else if pw, err = readPassword(stdin, stderr); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if sub == "create" {
		err = b.CreateAdmin(ctx, *name, pw, *role)
	} else {
		err = b.SetPassword(ctx, *name, pw)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *generate {
		// Only the password goes to standard output, so scripts can capture it
		fmt.Fprintf(stderr, "The new password of %s, shown only this once (keep it in a password manager):\n", *name)
		fmt.Fprintln(stdout, pw)
	}
	switch {
	case sub == "create":
		fmt.Fprintf(stderr, "Created the administrator %s (%s).\n", *name, *role)
	case b.Running():
		fmt.Fprintf(stderr, "Set the password of %s and signed out its console sessions.\n", *name)
	default:
		fmt.Fprintf(stderr, "Set the password of %s.\n", *name)
	}
	return 0
}

// admins is where the admin subcommands make their changes: the running service, or the operational
// database while the service is stopped.
type admins interface {
	Admins(ctx context.Context) ([]ctl.Admin, error)
	CreateAdmin(ctx context.Context, name, password, role string) error
	SetPassword(ctx context.Context, name, password string) error
	Running() bool
}

// openAdmins connects to the running service, or opens the operational database when the service is
// stopped. With mustExist, a missing database is an error rather than a new empty one, so that a wrong
// -config does not go unnoticed; creating the first administrator before the first start makes one.
func openAdmins(ctx context.Context, cfg *config.Config, mustExist bool) (admins, func(), error) {
	sock := ctl.Path(cfg.Admin.Store.Path)
	c, _, err := ctl.Dial(ctx, sock)
	if err == nil {
		return running{c}, func() {}, nil
	}
	if !errors.Is(err, ctl.ErrNotRunning) {
		return nil, nil, err
	}
	path := cfg.Admin.Store.Path
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) && mustExist {
		return nil, nil, fmt.Errorf("there is no operational database at %s; check -config or RELAY_CONFIG", path)
	}
	st, err := store.OpenBolt(path)
	if errors.Is(err, store.ErrLocked) {
		return nil, nil, fmt.Errorf("the service seems to be running, but its admin socket %s is not available "+
			"(see the service log; versions before v0.5.0 have none): change the password in the web console, "+
			"or stop the service and run the command again", sock)
	}
	if err != nil {
		return nil, nil, err
	}
	return stopped{ops.Offline{Store: st}}, func() { _ = st.Close() }, nil
}

type running struct{ *ctl.Client }

func (running) Running() bool { return true }

type stopped struct{ o ops.Offline }

func (stopped) Running() bool { return false }

func (s stopped) Admins(context.Context) ([]ctl.Admin, error) {
	var out []ctl.Admin
	for _, a := range s.o.Admins() {
		out = append(out, ctl.Admin{Name: a.Name, Role: a.Role, Disabled: a.Disabled, MustChange: a.MustChange, Created: a.Created})
	}
	return out, nil
}

func (s stopped) CreateAdmin(_ context.Context, name, password, role string) error {
	return s.o.CreateAdmin(cliActor(), name, password, role)
}

func (s stopped) SetPassword(_ context.Context, name, password string) error {
	return s.o.SetPassword(cliActor(), name, password)
}

func cliActor() ops.Actor { return ops.Actor{Kind: "cli", Name: ctl.CurrentUser()} }

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
