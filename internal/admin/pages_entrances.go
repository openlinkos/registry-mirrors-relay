package admin

import (
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/i18n"
	"github.com/openlinkos/registry-mirrors-relay/internal/ops"
)

// ---- Entrances: the domains clients pull from. Each has its own page with its settings and the client
// configuration for it. The list is one console override (entrances): saving one entrance replaces it in
// the list in effect.

// entranceRow is one entrance as shown and edited.
type entranceRow struct {
	Index                                              int
	Name, Hosts, Upstream, DefaultUpstream, Auth, Page string
	Title                                              string // the name, or the first host
}

func rowOf(i int, e config.Entrance) entranceRow {
	r := entranceRow{Index: i, Name: e.Name, Hosts: strings.Join(e.Hosts, ", "), Upstream: e.Upstream,
		DefaultUpstream: e.DefaultUpstream, Auth: e.Auth, Page: e.Page, Title: e.Name}
	if r.Title == "" && len(e.Hosts) > 0 {
		r.Title = e.Hosts[0]
	}
	return r
}

type entrancesData struct {
	Editable bool
	Rows     []entranceRow
	Override *ops.OverrideView
}

func (h *Handler) entrancesPage(c *ctx) error {
	rt := h.app.Runtime()
	d := entrancesData{Editable: h.canEditConfig(c), Override: overrideOf(h.ops.Overrides(), "entrances")}
	for i, e := range rt.Config.Entrances {
		d.Rows = append(d.Rows, rowOf(i, e))
	}
	return h.render(c, "entrances", &view{Title: tr(c.lang, "nav.entrances"), Nav: "entrances", Data: d})
}

// entranceFingerprint identifies an entrance's settings, so a save notices when the list changed since the
// form was shown.
func entranceFingerprint(e config.Entrance) string {
	return hashID(strings.Join([]string{e.Name, strings.Join(e.Hosts, ","), e.Upstream, e.DefaultUpstream, e.Auth, e.Page}, "\x00"))[:16]
}

type entranceData struct {
	Editable  bool
	New       bool
	Row       entranceRow
	Orig      string   // the fingerprint of the entrance the form was made from
	Upstreams []string // the upstreams that can be chosen (those not disabled)
	Override  *ops.OverrideView
	// Client configuration
	Hosts    []string // the entrance's host names (without "*")
	Host     string   // the host the configuration is generated for
	Scheme   string
	Snippets []snippetView
	PageURL  string // the usage page, when this entrance has one
}

var snippetHostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?$`)

// entrancePage shows one entrance (?i=<index>) or the form for a new one (?new=1).
func (h *Handler) entrancePage(c *ctx) error {
	rt := h.app.Runtime()
	q := c.r.URL.Query()
	d := entranceData{Editable: h.canEditConfig(c), Scheme: "https", Override: overrideOf(h.ops.Overrides(), "entrances")}
	for n, u := range rt.Config.Upstreams {
		if !u.Disabled {
			d.Upstreams = append(d.Upstreams, n)
		}
	}
	sort.Strings(d.Upstreams)
	d.New = q.Get("new") != "" || (c.form != nil && c.form.Get("new") != "")
	idx := q.Get("i")
	if c.form != nil && c.form.Has("i") {
		idx = c.form.Get("i")
	}
	var e config.Entrance
	if !d.New {
		i, err := strconv.Atoi(idx)
		if err != nil || i < 0 || i >= len(rt.Config.Entrances) {
			return fail(http.StatusNotFound, "error.notFound")
		}
		e = rt.Config.Entrances[i]
		d.Row, d.Orig = rowOf(i, e), entranceFingerprint(e)
	} else {
		if !d.Editable {
			return fail(http.StatusForbidden, "error.viewer")
		}
		d.Row = entranceRow{Index: -1, Auth: "none", Page: "public"}
	}
	if f := c.form; f != nil && f.Has("hosts") {
		// A form error is being shown again: keep what was entered
		d.Row.Name, d.Row.Hosts, d.Row.Upstream = f.Get("name"), f.Get("hosts"), f.Get("upstream")
		d.Row.DefaultUpstream, d.Row.Auth, d.Row.Page = f.Get("default_upstream"), f.Get("auth"), f.Get("page")
		d.Orig = f.Get("orig")
	}

	title := d.Row.Title
	if d.New {
		title = tr(c.lang, "entrances.new")
	} else {
		for _, host := range e.Hosts {
			if host != "*" {
				d.Hosts = append(d.Hosts, host)
			}
		}
		d.Host = "mirror.example.com"
		if len(d.Hosts) > 0 {
			d.Host = d.Hosts[0]
		}
		if hq := strings.ToLower(strings.TrimSpace(q.Get("host"))); snippetHostRe.MatchString(hq) {
			d.Host = hq
		}
		if q.Get("scheme") == "http" {
			d.Scheme = "http"
		}
		if e.Upstream != "" {
			d.Snippets = fixedSnippets(c.lang, rt.Config, e, d.Scheme, d.Host)
		} else {
			d.Snippets = snippets(c.lang, rt.Pool.Names(), rt.Config, d.Scheme, d.Host, e.Auth == "basic")
		}
		if e.Auth == "basic" {
			d.Snippets = append([]snippetView{{ID: "snippet-login", Label: tr(c.lang, "entrance.loginLabel"),
				Code: "docker login " + d.Host, Help: tr(c.lang, "entrance.loginHelp")}}, d.Snippets...)
		}
		if e.Page != "off" && len(d.Hosts) > 0 {
			d.PageURL = d.Scheme + "://" + d.Hosts[0] + "/"
		}
	}
	return h.render(c, "entrance", &view{Title: title, Nav: "entrances", Crumbs: []crumb{{tr(c.lang, "nav.entrances"), "/admin/entrances"}}, Data: d})
}

// entranceMaps is the entrance list in effect as form values, for FormEntrances.
func entranceMaps(list []config.Entrance) []map[string]string {
	out := make([]map[string]string, 0, len(list))
	for _, e := range list {
		out = append(out, map[string]string{"name": e.Name, "hosts": strings.Join(e.Hosts, ", "), "upstream": e.Upstream,
			"default_upstream": e.DefaultUpstream, "auth": e.Auth, "page": e.Page})
	}
	return out
}

// entranceAt finds the entrance a form refers to (i and orig), or an error to show with the form.
func (h *Handler) entranceAt(c *ctx) (int, error) {
	list := h.app.Runtime().Config.Entrances
	i, err := strconv.Atoi(c.form.Get("i"))
	if err != nil || i < 0 || i >= len(list) {
		return 0, fail(http.StatusBadRequest, "error.badForm")
	}
	if entranceFingerprint(list[i]) != c.form.Get("orig") {
		return 0, ops.NewError(http.StatusConflict, "entrance.changed")
	}
	return i, nil
}

func (h *Handler) entranceSave(c *ctx) error {
	list := entranceMaps(h.app.Runtime().Config.Entrances)
	row := map[string]string{}
	for _, f := range config.OverrideFields("entrances") {
		row[f] = c.form.Get(f)
	}
	if row["upstream"] != "" {
		row["default_upstream"] = "" // an entrance with a fixed upstream has no default
	}
	at := len(list)
	if c.form.Get("new") != "" {
		list = append(list, row)
	} else {
		i, err := h.entranceAt(c)
		if err != nil {
			return c.retry(err, h.entrancePage)
		}
		list[i], at = row, i
	}
	o, err := config.FormEntrances(list)
	if err != nil {
		return c.retry(badConfig(err), h.entrancePage)
	}
	return h.saveConfig(c, "entrances", o.Value, "/admin/entrances/view?i="+strconv.Itoa(at), h.entrancePage)
}

func (h *Handler) entranceDelete(c *ctx) error {
	i, err := h.entranceAt(c)
	if err != nil {
		return c.retry(err, h.entrancePage)
	}
	list := entranceMaps(h.app.Runtime().Config.Entrances)
	list = slices.Delete(list, i, i+1)
	o, err := config.FormEntrances(list)
	if err == nil && len(list) == 0 {
		err = i18n.Errorf("at least one entrance is needed (entrances)")
	}
	if err != nil {
		return c.retry(badConfig(err), h.entrancePage)
	}
	return h.saveConfig(c, "entrances", o.Value, "/admin/entrances", h.entrancesPage)
}

// ---- Client configuration

type snippetView struct {
	ID, Label, Code, Help string
}

// snippets generates the configuration that points each kind of client at a single-domain entrance.
// containerd adds ?ns=<upstream> to its requests, Podman's mirror location is written as <domain>/<upstream>
// (a path prefix), and Docker's registry-mirrors only applies to Docker Hub (the root path).
// When the entrance requires sign-in (login), Docker's registry-mirrors is left out, since Docker sends no
// credentials to a mirror, and the others carry a client account.
func snippets(lang i18n.Lang, names []string, cfg *config.Config, scheme, host string, login bool) []snippetView {
	base := scheme + "://" + host
	var out []snippetView

	if !login {
		daemon := "{\n  \"registry-mirrors\": [\"" + base + "\"]"
		if scheme == "http" {
			daemon += ",\n  \"insecure-registries\": [\"" + host + "\"]"
		}
		daemon += "\n}"
		out = append(out, snippetView{ID: "snippet-docker", Label: "/etc/docker/daemon.json", Code: daemon, Help: tr(lang, "usage.dockerHelp")})
	}

	var ctr strings.Builder
	for _, n := range names {
		u := cfg.Upstreams[n]
		server := "https://" + u.Registry
		if n == config.DefaultUpstream {
			server = "https://registry-1.docker.io"
		}
		ctr.WriteString(hostsToml(n, server, base, login) + "\n")
	}
	out = append(out, snippetView{ID: "snippet-containerd", Label: "containerd hosts.toml", Code: strings.TrimSuffix(ctr.String(), "\n"), Help: loginHelp(lang, login, "usage.containerdHelp", "entrance.containerdLoginHelp", host)})

	var k3s strings.Builder
	k3s.WriteString("mirrors:\n")
	for _, n := range names {
		k3s.WriteString("  " + n + ":\n    endpoint:\n      - \"" + base + "\"\n")
	}
	out = append(out, snippetView{ID: "snippet-k3s", Label: "/etc/rancher/k3s/registries.yaml", Code: strings.TrimSuffix(k3s.String(), "\n") + k3sAuth(login, host), Help: loginHelp(lang, login, "usage.k3sHelp", "entrance.k3sLoginHelp", host)})

	var pod strings.Builder
	for i, n := range names {
		if i > 0 {
			pod.WriteString("\n")
		}
		loc := host + "/" + n
		if n == config.DefaultUpstream {
			loc = host
		}
		pod.WriteString("[[registry]]\nprefix = \"" + n + "\"\nlocation = \"" + n + "\"\n\n[[registry.mirror]]\nlocation = \"" + loc + "\"\n")
		if scheme == "http" {
			pod.WriteString("insecure = true\n")
		}
	}
	out = append(out, snippetView{ID: "snippet-podman", Label: "/etc/containers/registries.conf", Code: strings.TrimSuffix(pod.String(), "\n"), Help: loginHelp(lang, login, "usage.podmanHelp", "entrance.podmanLoginHelp", host)})

	pull := "docker pull " + host + "/library/nginx:latest\ndocker pull " + host + "/ghcr.io/<owner>/<image>:<tag>\nnerdctl pull " + host + "/quay.io/<owner>/<image>:<tag>"
	out = append(out, snippetView{ID: "snippet-pull", Label: tr(lang, "usage.pullLabel"), Code: pull, Help: tr(lang, "usage.pullHelp")})
	return out
}

// fixedSnippets generates the configuration for an entrance with a fixed upstream: pulls name the entrance's
// host instead of the upstream's, and containerd, k3s and Podman can use it as a mirror of that upstream.
// Sign-in is handled as in snippets.
func fixedSnippets(lang i18n.Lang, cfg *config.Config, e config.Entrance, scheme, host string) []snippetView {
	base := scheme + "://" + host
	n, login := e.Upstream, e.Auth == "basic"
	server, prefix := "https://"+n, n
	if u := cfg.Upstreams[n]; u != nil {
		server = "https://" + u.Registry
	}
	if n == config.DefaultUpstream {
		server = "https://registry-1.docker.io"
	}
	example := "<owner>/<image>:<tag>"
	if n == config.DefaultUpstream {
		example = "library/nginx:latest"
	}
	out := []snippetView{{ID: "snippet-pull", Label: tr(lang, "usage.pullLabel"), Code: "docker pull " + host + "/" + example,
		Help: tr(lang, "entrance.fixedPullHelp", "upstream", n)}}
	if n == config.DefaultUpstream && !login {
		daemon := "{\n  \"registry-mirrors\": [\"" + base + "\"]\n}"
		out = append(out, snippetView{ID: "snippet-docker", Label: "/etc/docker/daemon.json", Code: daemon, Help: tr(lang, "usage.dockerHelp")})
	}
	out = append(out, snippetView{ID: "snippet-containerd", Label: "containerd hosts.toml", Code: hostsToml(n, server, base, login),
		Help: loginHelp(lang, login, "usage.containerdHelp", "entrance.containerdLoginHelp", host)})
	out = append(out, snippetView{ID: "snippet-k3s", Label: "/etc/rancher/k3s/registries.yaml",
		Code: "mirrors:\n  " + n + ":\n    endpoint:\n      - \"" + base + "\"" + k3sAuth(login, host),
		Help: loginHelp(lang, login, "usage.k3sHelp", "entrance.k3sLoginHelp", host)})
	pod := "[[registry]]\nprefix = \"" + prefix + "\"\nlocation = \"" + prefix + "\"\n\n[[registry.mirror]]\nlocation = \"" + host + "\""
	if scheme == "http" {
		pod += "\ninsecure = true"
	}
	out = append(out, snippetView{ID: "snippet-podman", Label: "/etc/containers/registries.conf", Code: pod,
		Help: loginHelp(lang, login, "usage.podmanHelp", "entrance.podmanLoginHelp", host)})
	return out
}

// hostsToml writes containerd's hosts.toml for one upstream (server) mirrored at base. An entrance requiring
// sign-in gets the client account in an authorization header.
func hostsToml(upstream, server, base string, login bool) string {
	dir := "/etc/containerd/certs.d/" + upstream
	s := "mkdir -p " + dir + "\ncat > " + dir + "/hosts.toml <<'EOF'\nserver = \"" + server + "\"\n\n[host.\"" + base + "\"]\n  capabilities = [\"pull\", \"resolve\"]\n"
	if login {
		s += "  [host.\"" + base + "\".header]\n    authorization = \"Basic <base64 of user:password>\"\n"
	}
	return s + "EOF"
}

// k3sAuth is the configs section of registries.yaml with the client account, for an entrance requiring sign-in.
func k3sAuth(login bool, host string) string {
	if !login {
		return ""
	}
	return "\nconfigs:\n  \"" + host + "\":\n    auth:\n      username: <username>\n      password: <password>"
}

// loginHelp is a snippet's hint: the sign-in variant (which names host) when the entrance requires it.
func loginHelp(lang i18n.Lang, login bool, key, loginKey, host string) string {
	if login {
		return tr(lang, loginKey, "host", host)
	}
	return tr(lang, key)
}
