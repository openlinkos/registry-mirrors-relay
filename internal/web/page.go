package web

import (
	"html/template"
	"slices"
	"sort"
	"strings"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
)

// known are the registries the page has words and examples for, in the page's order. Other upstreams in the
// configuration follow, under their own name.
var known = []struct{ name, label, example string }{
	{"docker.io", "Docker Hub", "library/nginx:1.27"},
	{"gcr.io", "gcr.io", "distroless/static:nonroot"},
	{"ghcr.io", "ghcr.io", "containerd/busybox:1.36"},
	{"registry.k8s.io", "registry.k8s.io", "pause:3.10"},
	{"quay.io", "quay.io", "coreos/etcd:v3.5.0"},
}

// upstream is one registry as the page shows it.
type upstream struct {
	Name    string   `json:"name"`              // as in image names and status.json, such as docker.io
	Label   string   `json:"label"`             // such as Docker Hub
	Example string   `json:"example"`           // what follows the registry in an example image
	Domain  string   `json:"domain,omitempty"`  // the host of an entrance fixed to it
	Login   bool     `json:"login,omitempty"`   // pulls through it need an account
	Root    bool     `json:"root,omitempty"`    // on the single-domain entrance, it needs no prefix
	Aliases []string `json:"aliases,omitempty"` // other names of the registry
	Library bool     `json:"library,omitempty"` // single-segment names get library/
	Server  string   `json:"-"`                 // the registry's own address, for containerd
}

// single is the single-domain entrance, where the registry follows the host as a path prefix.
type single struct {
	Hosts   []string `json:"hosts"`           // the entrance's hosts; none means any host, the page's own
	Login   bool     `json:"login,omitempty"` // pulls through it need an account
	Default string   `json:"default"`         // the upstream at the root
}

// model is what the page knows from the configuration.
type model struct {
	Upstreams []upstream
	Single    *single
}

// buildModel works out how each enabled upstream is reached: through an entrance fixed to it (its own host),
// or through the first single-domain entrance (a path prefix after its host). Upstreams reached neither way
// are left out.
func buildModel(cfg *config.Config) model {
	var m model
	fixed := map[string]config.Entrance{}
	for _, e := range cfg.Entrances {
		if e.Upstream == "" {
			if m.Single == nil {
				s := &single{Login: e.Auth == "basic", Default: e.DefaultUpstream}
				for _, h := range e.Hosts {
					if h == "*" {
						s.Hosts = nil
						break
					}
					s.Hosts = append(s.Hosts, h)
				}
				m.Single = s
			}
			continue
		}
		if _, seen := fixed[e.Upstream]; seen {
			continue
		}
		for _, h := range e.Hosts {
			if h != "*" {
				fixed[e.Upstream] = config.Entrance{Hosts: []string{h}, Auth: e.Auth}
				break
			}
		}
	}

	var names []string
	for n, u := range cfg.Upstreams {
		if !u.Disabled {
			names = append(names, n)
		}
	}
	rank := func(n string) int {
		for i, k := range known {
			if k.name == n {
				return i
			}
		}
		return len(known)
	}
	sort.Slice(names, func(i, j int) bool {
		if ri, rj := rank(names[i]), rank(names[j]); ri != rj {
			return ri < rj
		}
		return names[i] < names[j]
	})
	for _, n := range names {
		u := cfg.Upstreams[n]
		v := upstream{Name: n, Label: n, Example: "<image>:<tag>", Aliases: u.Aliases, Library: u.LibraryPrefix, Server: "https://" + u.Registry}
		if n != config.DefaultUpstream {
			v.Server = "https://" + n
		}
		if i := rank(n); i < len(known) {
			v.Label, v.Example = known[i].label, known[i].example
		}
		switch e, ok := fixed[n]; {
		case ok:
			v.Domain, v.Login = e.Hosts[0], e.Auth == "basic"
		case m.Single != nil:
			v.Login, v.Root = m.Single.Login, m.Single.Default == n
		default:
			continue
		}
		m.Upstreams = append(m.Upstreams, v)
	}
	return m
}

func (m model) find(name string) *upstream {
	for i := range m.Upstreams {
		if m.Upstreams[i].Name == name {
			return &m.Upstreams[i]
		}
	}
	return nil
}

// anyFixed: some upstream has a host of its own.
func (m model) anyFixed() bool {
	return slices.ContainsFunc(m.Upstreams, func(u upstream) bool { return u.Domain != "" })
}

// loginHosts are the hosts to sign in to before pulling, in the page's order.
func (m model) loginHosts() []hostRef {
	var out []hostRef
	seenSingle := false
	for _, u := range m.Upstreams {
		switch {
		case !u.Login:
		case u.Domain != "":
			out = append(out, hostRef{name: u.Domain})
		case !seenSingle:
			seenSingle = true
			out = append(out, m.singleHost())
		}
	}
	return out
}

// ---- Code shown on the page

// hostRef is a host in code: a fixed host is written as is; the single-domain entrance's host is marked so
// the script can put the page's own host there (any host, or one of its hosts).
type hostRef struct {
	name   string
	single bool
}

// placeholderHost stands for the page's own host where only the script knows it.
const placeholderHost = "<host>"

func (m model) singleHost() hostRef {
	if m.Single != nil && len(m.Single.Hosts) > 0 {
		return hostRef{name: m.Single.Hosts[0], single: true}
	}
	return hostRef{name: placeholderHost, single: true}
}

// code builds the markup of a code block: text is escaped, hosts and schemes are marked for the script, and
// parts only plain-HTTP sites need are hidden until the script finds the page served over http. With soft,
// a long address may break after each slash.
type code struct {
	b    strings.Builder
	soft bool
}

func (c *code) text(s string) *code {
	s = template.HTMLEscapeString(s)
	if c.soft {
		s = strings.ReplaceAll(s, "/", "/<wbr>")
	}
	c.b.WriteString(s)
	return c
}

func (c *code) host(h hostRef) *code {
	if !h.single {
		return c.text(h.name)
	}
	c.b.WriteString(`<span data-single>` + template.HTMLEscapeString(h.name) + `</span>`)
	return c
}

func (c *code) scheme() *code { c.b.WriteString(`<span data-scheme>https</span>`); return c }

// http adds text shown only when the page is served over plain HTTP.
func (c *code) http(f func(*code)) *code {
	c.b.WriteString(`<span data-http hidden>`)
	f(c)
	c.b.WriteString(`</span>`)
	return c
}

// mirror writes where an upstream is pulled from: its host, or the single-domain host and its prefix.
func (c *code) mirror(m model, u *upstream) *code {
	if u.Domain != "" {
		return c.text(u.Domain)
	}
	c.host(m.singleHost())
	if !u.Root {
		c.text("/" + u.Name)
	}
	return c
}

// endpoint is the address of the host that serves an upstream, for configuration that names a host
// (containerd, k3s): containerd's ns parameter picks the upstream on the single-domain entrance.
func (c *code) endpoint(m model, u *upstream) *code {
	c.scheme().text("://")
	if u.Domain != "" {
		return c.text(u.Domain)
	}
	return c.host(m.singleHost())
}

func (c *code) html() template.HTML {
	return template.HTML(strings.TrimSuffix(c.b.String(), "\n")) //nolint:gosec // built from escaped text and fixed markup
}

// snippets is the code of the Get started tabs.
type snippets struct {
	// Docker: DockerMode is mirror (registry-mirrors for Docker Hub), direct (put the mirror in image names:
	// the Docker Hub mirror needs a sign-in, which registry-mirrors does not send, or there is no Docker Hub
	// mirror at a root path) or none (nothing to pull)
	DockerMode string
	NoHub      bool
	Login      template.HTML // docker login …
	Daemon     template.HTML
	Restart    template.HTML
	Pull       template.HTML
	Direct     template.HTML

	ContainerdConfig, ContainerdConfig1, ContainerdHosts, ContainerdRestart template.HTML
	K3s, K3sRestart                                                         template.HTML
	Podman, PodmanLogin, PodmanApply                                        template.HTML
	Kubeadm, Kind                                                           template.HTML
	AnyLogin                                                                bool
}

func buildSnippets(m model) snippets {
	var s snippets
	if len(m.Upstreams) == 0 {
		s.DockerMode = "none"
		return s
	}
	logins := m.loginHosts()
	s.AnyLogin = len(logins) > 0
	if len(logins) > 0 {
		c := &code{}
		for _, h := range logins {
			c.text("docker login ").host(h).text("\n")
		}
		s.Login = c.html()
	}

	hub := m.find(config.DefaultUpstream)
	switch {
	case hub != nil && (hub.Domain != "" || hub.Root) && !hub.Login:
		s.DockerMode = "mirror"
		host := hostRef{name: hub.Domain}
		if hub.Domain == "" {
			host = m.singleHost()
		}
		c := &code{}
		c.text("{\n  \"registry-mirrors\": [\"").scheme().text("://").host(host).text("\"]")
		c.http(func(c *code) { c.text(",\n  \"insecure-registries\": [\"").host(host).text("\"]") })
		c.text("\n}")
		s.Daemon = c.html()
		s.Restart = (&code{}).text("sudo systemctl restart docker").html()
		s.Pull = (&code{}).text("docker pull nginx:1.27").html()
	default:
		s.DockerMode = "direct"
		s.NoHub = hub == nil
	}
	c := &code{}
	for i := range m.Upstreams {
		u := &m.Upstreams[i]
		c.text("docker pull ").mirror(m, u).text("/" + u.Example + "\n")
	}
	s.Direct = c.html()

	s.ContainerdConfig = (&code{}).text("[plugins.\"io.containerd.cri.v1.images\".registry]\n  config_path = \"/etc/containerd/certs.d\"").html()
	s.ContainerdConfig1 = (&code{}).text("[plugins.\"io.containerd.grpc.v1.cri\".registry]\n  config_path = \"/etc/containerd/certs.d\"").html()
	c = &code{}
	for i := range m.Upstreams {
		u := &m.Upstreams[i]
		if i > 0 {
			c.text("\n")
		}
		dir := "/etc/containerd/certs.d/" + u.Name
		c.text("sudo mkdir -p "+dir+"\nsudo tee "+dir+"/hosts.toml >/dev/null <<'EOF'\nserver = \""+u.Server+"\"\n\n[host.\"").
			endpoint(m, u).text("\"]\n  capabilities = [\"pull\", \"resolve\"]\n")
		if u.Login {
			c.text("  [host.\"").endpoint(m, u).text("\".header]\n    authorization = \"Basic <base64 of user:password>\"\n")
		}
		c.text("EOF\n")
	}
	s.ContainerdHosts = c.html()
	s.ContainerdRestart = (&code{}).text("sudo systemctl restart containerd").html()

	c = &code{}
	c.text("mirrors:\n")
	for i := range m.Upstreams {
		u := &m.Upstreams[i]
		c.text("  "+u.Name+":\n    endpoint:\n      - \"").endpoint(m, u).text("\"\n")
	}
	if len(logins) > 0 {
		c.text("configs:\n")
		for _, h := range logins {
			c.text("  \"").host(h).text("\":\n    auth:\n      username: <username>\n      password: <password>\n")
		}
	}
	s.K3s = c.html()
	s.K3sRestart = (&code{}).text("sudo systemctl restart k3s        # server\nsudo systemctl restart k3s-agent  # agent").html()

	c = &code{}
	for i := range m.Upstreams {
		u := &m.Upstreams[i]
		if i > 0 {
			c.text("\n")
		}
		c.text("[[registry]]\nprefix = \""+u.Name+"\"\nlocation = \""+u.Name+"\"\n\n[[registry.mirror]]\nlocation = \"").mirror(m, u).text("\"\n")
		c.http(func(c *code) { c.text("insecure = true\n") })
	}
	s.Podman = c.html()
	if len(logins) > 0 {
		c = &code{}
		for _, h := range logins {
			c.text("podman login ").host(h).text("\n")
		}
		s.PodmanLogin = c.html()
	}
	s.PodmanApply = (&code{}).text("sudo systemctl restart crio").html()

	if k8s := m.find("registry.k8s.io"); k8s != nil {
		c = &code{}
		c.text("sudo kubeadm init --image-repository ").mirror(m, k8s)
		s.Kubeadm = c.html()
	}
	s.Kind = (&code{}).text("kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\ncontainerdConfigPatches:\n  - |-\n    [plugins.\"io.containerd.grpc.v1.cri\".registry]\n      config_path = \"/etc/containerd/certs.d\"\nnodes:\n  - role: control-plane\n    extraMounts:\n      - hostPath: /etc/containerd/certs.d\n        containerPath: /etc/containerd/certs.d").html()
	return s
}

// mirrorHTML is an upstream's mirror address as markup, for the cards.
func (m model) mirrorHTML(u *upstream) template.HTML {
	return (&code{soft: true}).mirror(m, u).html()
}

// exampleHTML is the command that pulls an upstream's example through the mirror.
func (m model) exampleHTML(u *upstream) template.HTML {
	return (&code{soft: true}).text("docker pull ").mirror(m, u).text("/" + u.Example).html()
}
