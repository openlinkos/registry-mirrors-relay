package config

import (
	"bytes"
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/openlinkos/registry-mirrors-relay/internal/i18n"
)

// Result is the outcome of loading: the effective configuration, and things that do not prevent startup but
// deserve a warning.
type Result struct {
	Config   *Config
	Warnings []i18n.Message
}

// Load reads the configuration file, merges it on top of the defaults and the built-in upstreams, validates it
// and resolves secrets. When validation fails, all errors are returned at once.
func Load(path string) (*Result, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the configuration file path is chosen by the operator
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// LoadDefaults reads no file and uses only the defaults (for when there is no configuration file, such as a
// plain docker run).
func LoadDefaults() (*Result, error) { return Parse(nil) }

// Parse parses configuration content. Empty content means defaults only.
func Parse(b []byte) (*Result, error) { return ParseWithOverrides(b, nil) }

// ParseWithOverrides parses configuration content and applies the console overrides on top (see Override); like
// Parse, it merges the defaults, validates and resolves secrets.
func ParseWithOverrides(b []byte, ovs []Override) (*Result, error) {
	cfg := Defaults()
	userUpstreams := map[string][]byte{}
	var warnings []i18n.Message

	top := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if len(bytes.TrimSpace(b)) > 0 {
		var root yaml.Node
		if err := yaml.Unmarshal(b, &root); err != nil {
			return nil, i18n.Errorf("the configuration is not valid YAML: %w", err)
		}
		if root.Kind != yaml.DocumentNode || len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
			return nil, i18n.Errorf("the top level of the configuration must be a mapping")
		}
		top = root.Content[0]
		removedWarn, err := checkRemoved(top)
		if err != nil {
			return nil, err
		}
		warnings = append(warnings, removedWarn...)
	}
	if err := applyOverrides(top, ovs); err != nil {
		return nil, err
	}
	if len(top.Content) > 0 {
		// upstreams are handled separately: each upstream overrides fields of the built-in definition and
		// cannot replace it as a whole
		for i := 0; i+1 < len(top.Content); i += 2 {
			if top.Content[i].Value != "upstreams" {
				continue
			}
			ups := top.Content[i+1]
			if ups.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(ups.Content); j += 2 {
					raw, err := yaml.Marshal(ups.Content[j+1])
					if err != nil {
						return nil, err
					}
					userUpstreams[strings.ToLower(ups.Content[j].Value)] = raw
				}
			} else if ups.Tag != "!!null" {
				return nil, i18n.Errorf("line %d: upstreams must be a mapping", ups.Line)
			}
			top.Content = append(top.Content[:i], top.Content[i+2:]...)
			break
		}
		rest, err := yaml.Marshal(top)
		if err != nil {
			return nil, err
		}
		if err := strictDecode(rest, cfg); err != nil {
			return nil, err
		}
	}

	ups, err := mergeUpstreams(userUpstreams)
	if err != nil {
		return nil, err
	}
	cfg.Upstreams = ups

	envWarn := applyEnv(cfg)
	normalize(cfg)
	res := &Result{Config: cfg, Warnings: warnings}
	if !envWarn.IsZero() {
		res.Warnings = append(res.Warnings, envWarn)
	}
	if errs := validate(cfg, res); len(errs) > 0 {
		return nil, joinErrors(errs)
	}
	if errs := resolveSecrets(cfg); len(errs) > 0 {
		return nil, joinErrors(errs)
	}
	return res, nil
}

// applyEnv lets a container set the first administrator without changing the configuration file
// (RELAY_ADMIN_PASSWORD_FILE; the user name defaults to admin and can be changed with RELAY_ADMIN_USERNAME). Only
// a password file is accepted, that is, a Docker, Compose or k8s secret; there is no environment variable for the
// password itself, as the password would then show up in docker inspect, the process environment and
// orchestration files. When the configuration sets initial_admin, the configuration wins.
//
// The password file is used only once, to create the first administrator, and should be deleted afterwards; so a
// missing file only gives a warning, not an error, so that the service still starts after the file is deleted
// while the environment variable remains.
func applyEnv(c *Config) (warning i18n.Message) {
	f := os.Getenv("RELAY_ADMIN_PASSWORD_FILE")
	if f == "" || c.Admin.InitialAdmin.Username != "" {
		return i18n.Message{}
	}
	if _, err := os.Stat(f); err != nil { //nolint:gosec // the path is set by the operator in an environment variable
		return i18n.M("RELAY_ADMIN_PASSWORD_FILE points to %s, which cannot be read; ignored "+
			"(once the first administrator exists, the variable can go too)", f)
	}
	c.Admin.InitialAdmin = InitialAdmin{Username: cmp.Or(os.Getenv("RELAY_ADMIN_USERNAME"), "admin"), Password: Secret{File: f}, FromEnv: true}
	return i18n.Message{}
}

// checkRemoved handles the keys removed in v0.2.0: a value is an error that explains what to write instead; an
// empty value (which meant off anyway) only gives a warning and is deleted from the node tree, so that strict
// decoding does not report it as an unknown field.
func checkRemoved(top *yaml.Node) (warnings []i18n.Message, err error) {
	var errs []error
	for i := 0; i+1 < len(top.Content); i += 2 {
		k, v := top.Content[i], top.Content[i+1]
		switch {
		case k.Value == "listen" && v.Kind == yaml.MappingNode:
			for j := 0; j+1 < len(v.Content); j += 2 {
				if v.Content[j].Value != "metrics" {
					continue
				}
				if val := v.Content[j+1]; val.Kind == yaml.ScalarNode && val.Value == "" {
					warnings = append(warnings, i18n.M("line %d: listen.metrics was removed; delete this line", val.Line))
				} else {
					errs = append(errs, i18n.Errorf("line %d: listen.metrics was removed in v0.2.0: metrics are served on the management "+
						"entrance (listen.admin, or the pull port when empty); set metrics.enabled: true and point Prometheus at /metrics there", val.Line))
				}
				v.Content = append(v.Content[:j], v.Content[j+2:]...)
				break
			}
		case k.Value == "entrances" && v.Kind == yaml.SequenceNode:
			for _, e := range v.Content {
				if e.Kind != yaml.MappingNode {
					continue
				}
				for j := 0; j+1 < len(e.Content); j += 2 {
					if e.Content[j].Value != "listen" {
						continue
					}
					if val := e.Content[j+1]; val.Kind == yaml.ScalarNode && val.Value == "" {
						warnings = append(warnings, i18n.M("line %d: the listen key of entrances was removed; delete this line", val.Line))
					} else {
						errs = append(errs, i18n.Errorf("line %d: entrances by port (entrances[].listen) were removed in v0.2.0: use hosts "+
							"with upstream (one host name per upstream), or the /v2/<upstream>/ prefix on a single-host entrance", val.Line))
					}
					e.Content = append(e.Content[:j], e.Content[j+2:]...)
					break
				}
			}
		}
	}
	if len(errs) > 0 {
		return nil, joinErrors(errs)
	}
	return warnings, nil
}

// strictDecode fails on unknown fields, so that misspelt settings are not silently ignored.
func strictDecode(b []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return i18n.Errorf("invalid configuration: %w", err)
	}
	return nil
}

func mergeUpstreams(user map[string][]byte) (map[string]*Upstream, error) {
	ups := BuiltinUpstreams()
	names := make([]string, 0, len(user))
	for n := range user {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		base, ok := ups[name]
		if !ok {
			base = &Upstream{}
		}
		if err := strictDecode(user[name], base); err != nil {
			return nil, i18n.Errorf("upstream %s: %w", name, err)
		}
		ups[name] = base
	}
	return ups, nil
}

// normalize normalises case and fills in the defaults that depend on other fields.
func normalize(c *Config) {
	normalizeEntrances(c.Entrances)
	for _, u := range c.Upstreams {
		u.Registry = strings.ToLower(u.Registry)
		for i, a := range u.Aliases {
			u.Aliases[i] = strings.ToLower(a)
		}
		if u.UnlistedRedirect == "" {
			u.UnlistedRedirect = "pass"
		}
		if u.Credentials != nil {
			u.Credentials.RealmHost = strings.ToLower(u.Credentials.RealmHost)
		}
	}
	if c.TLS.Mode == "" {
		c.TLS.Mode = "off"
	}
	if c.TLS.ACME.CacheDir == "" {
		c.TLS.ACME.CacheDir = filepath.Join(c.DataDir, "acme")
	}
	if c.TLS.Mode == "acme" && len(c.TLS.ACME.Hosts) == 0 {
		c.TLS.ACME.Hosts = entranceHosts(c)
	}
	if c.Admin.Store.Driver == "" {
		c.Admin.Store.Driver = "bolt"
	}
	if c.Admin.Store.Path == "" {
		c.Admin.Store.Path = filepath.Join(c.DataDir, "state.db")
	}
	if c.Site.Links == nil {
		c.Site.Links = map[string]string{}
	}
}

// normalizeEntrances normalises the case of the entrances and fills in defaults.
func normalizeEntrances(es []Entrance) {
	for i := range es {
		e := &es[i]
		for j, h := range e.Hosts {
			e.Hosts[j] = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
		}
		e.Upstream = strings.ToLower(e.Upstream)
		e.DefaultUpstream = strings.ToLower(e.DefaultUpstream)
		if e.Upstream == "" && e.DefaultUpstream == "" {
			e.DefaultUpstream = DefaultUpstream
		}
		if e.Auth == "" {
			e.Auth = "none"
		}
		if e.Page == "" {
			e.Page = "public"
		}
		if e.Name == "" {
			switch {
			case len(e.Hosts) > 0:
				e.Name = e.Hosts[0]
			default:
				e.Name = fmt.Sprintf("entrance-%d", i+1)
			}
		}
	}
}

// entranceHosts returns the domains named in the entrances (excluding *), deduplicated and sorted alphabetically.
func entranceHosts(c *Config) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range c.Entrances {
		for _, h := range e.Hosts {
			if h != "*" && !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	sort.Strings(out)
	return out
}

func resolveSecrets(c *Config) []error {
	var errs []error
	names := make([]string, 0, len(c.Upstreams))
	for n := range c.Upstreams {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		u := c.Upstreams[n]
		if u.Credentials == nil || u.Disabled {
			continue
		}
		if err := u.Credentials.Password.resolve(); err != nil {
			errs = append(errs, i18n.Errorf("upstream %s: credentials password: %w", n, err))
		}
	}
	for i := range c.Admin.APITokens {
		tk := &c.Admin.APITokens[i]
		if err := tk.Token.resolve(); err != nil {
			errs = append(errs, i18n.Errorf("admin.api_tokens: the token of %s: %w", tk.Name, err))
		} else if len(tk.Token.Reveal()) < 32 {
			errs = append(errs, i18n.Errorf("admin.api_tokens: the token of %s is too short: use at least 32 characters "+
				"(openssl rand -hex 32 makes one)", tk.Name))
		}
	}
	if c.Admin.InitialAdmin.Password.IsSet() {
		if err := c.Admin.InitialAdmin.Password.resolve(); err != nil {
			errs = append(errs, i18n.Errorf("admin.initial_admin.password: %w", err))
		} else if pw := c.Admin.InitialAdmin.Password.Reveal(); len(pw) < 12 || len(pw) > 72 {
			errs = append(errs, i18n.Errorf("admin.initial_admin.password must be 12 to 72 bytes long"))
		} else if strings.EqualFold(pw, c.Admin.InitialAdmin.Username) {
			errs = append(errs, i18n.Errorf("admin.initial_admin.password must not be the username"))
		}
	}
	return errs
}

func joinErrors(errs []error) error { return Errors(errs) }

// Errors are the problems found in a configuration, reported together.
type Errors []error

func (e Errors) Error() string   { return e.In(i18n.EN) }
func (e Errors) Unwrap() []error { return e }

// In lists the problems in l.
func (e Errors) In(l i18n.Lang) string {
	var sb strings.Builder
	sb.WriteString(i18n.T(l, "configuration errors (%d):", len(e)))
	for _, err := range e {
		sb.WriteString("\n  - ")
		sb.WriteString(i18n.Localize(err, l))
	}
	return sb.String()
}

// Print outputs the effective configuration (YAML), showing only the source of each secret.
func (c *Config) Print() ([]byte, error) {
	type printable struct {
		Config    `yaml:",inline"`
		Upstreams map[string]*Upstream `yaml:"upstreams"`
	}
	return yaml.Marshal(printable{Config: *c, Upstreams: c.Upstreams})
}
