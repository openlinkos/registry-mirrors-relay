package admin

import (
	"regexp"
	"strings"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
)

// ---- 上游

func (h *Handler) upstreamsPage(c *ctx) error {
	return h.render(c, "upstreams", &view{Title: tr(c.lang, "nav.upstreams"), Nav: "upstreams", Data: h.ops.Upstreams()})
}

func (h *Handler) upstreamCheck(c *ctx) error {
	name := c.form.Get("upstream")
	hl, err := h.ops.CheckUpstream(c.r.Context(), c.actor(), name)
	if err != nil {
		return c.retry(err, h.upstreamsPage)
	}
	if hl.OK {
		return c.done("/admin/upstreams", "upstream.ok", name)
	}
	return c.done("/admin/upstreams", "upstream.failed", name)
}

func (h *Handler) redirectHostAdd(c *ctx) error {
	host, err := h.ops.AddRedirectHost(c.actor(), c.form.Get("upstream"), c.form.Get("host"))
	if err != nil {
		return c.retry(err, h.upstreamsPage)
	}
	return c.done("/admin/upstreams", "host.added", host)
}

func (h *Handler) redirectHostDelete(c *ctx) error {
	host := c.form.Get("host")
	if err := h.ops.DeleteRedirectHost(c.actor(), c.form.Get("upstream"), host); err != nil {
		return c.retry(err, h.upstreamsPage)
	}
	return c.done("/admin/upstreams", "host.deleted", host)
}

// ---- 入口与用法

type entranceView struct {
	Name, Hosts, Upstream, Auth, Page string
}

type snippetView struct {
	ID, Label, Code, Help string
}

type usageData struct {
	Entrances  []entranceView
	Hosts      []string // 单域名入口的域名，可以选来生成配置
	Host       string
	Scheme     string
	Snippets   []snippetView
	Fixed      []entranceView // 固定上游的入口（每个上游一个子域名）
	Basic      bool
	ShowStatus bool // 用法页上显示服务状态（site.show_status）
}

var snippetHostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?$`)

func (h *Handler) usagePage(c *ctx) error {
	rt := h.app.Runtime()
	cfg := rt.Config
	d := usageData{Scheme: "https", ShowStatus: rt.Site.ShowStatus}
	for _, e := range cfg.Entrances {
		ev := entranceView{Name: e.Name, Hosts: strings.Join(e.Hosts, ", "), Upstream: e.Upstream, Auth: e.Auth, Page: e.Page}
		d.Entrances = append(d.Entrances, ev)
		if e.Auth == "basic" {
			d.Basic = true
		}
		for _, host := range e.Hosts {
			if host == "*" {
				continue
			}
			if e.Upstream == "" {
				d.Hosts = append(d.Hosts, host)
			} else {
				d.Fixed = append(d.Fixed, entranceView{Upstream: e.Upstream, Hosts: host})
			}
		}
	}
	q := c.r.URL.Query()
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
	d.Snippets = snippets(c.lang, rt.Pool.Names(), cfg, d.Scheme, d.Host)
	return h.render(c, "usage", &view{Title: tr(c.lang, "nav.usage"), Nav: "usage", Data: d})
}

// snippets 生成各种客户端指向单域名入口的配置。containerd 会在请求里带 ?ns=<上游>，
// Podman 的镜像地址写成 <域名>/<上游>（路径前缀），Docker 的 registry-mirrors 只对 Docker Hub 生效（根路径）。
func snippets(lang string, names []string, cfg *config.Config, scheme, host string) []snippetView {
	base := scheme + "://" + host
	var out []snippetView

	daemon := "{\n  \"registry-mirrors\": [\"" + base + "\"]"
	if scheme == "http" {
		daemon += ",\n  \"insecure-registries\": [\"" + host + "\"]"
	}
	daemon += "\n}"
	out = append(out, snippetView{ID: "snippet-docker", Label: "/etc/docker/daemon.json", Code: daemon, Help: tr(lang, "usage.dockerHelp")})

	var ctr strings.Builder
	for _, n := range names {
		u := cfg.Upstreams[n]
		server := "https://" + u.Registry
		if n == config.DefaultUpstream {
			server = "https://registry-1.docker.io"
		}
		dir := "/etc/containerd/certs.d/" + n
		ctr.WriteString("mkdir -p " + dir + "\ncat > " + dir + "/hosts.toml <<'EOF'\nserver = \"" + server + "\"\n\n[host.\"" + base + "\"]\n  capabilities = [\"pull\", \"resolve\"]\nEOF\n")
	}
	out = append(out, snippetView{ID: "snippet-containerd", Label: "containerd hosts.toml", Code: strings.TrimSuffix(ctr.String(), "\n"), Help: tr(lang, "usage.containerdHelp")})

	var k3s strings.Builder
	k3s.WriteString("mirrors:\n")
	for _, n := range names {
		k3s.WriteString("  " + n + ":\n    endpoint:\n      - \"" + base + "\"\n")
	}
	out = append(out, snippetView{ID: "snippet-k3s", Label: "/etc/rancher/k3s/registries.yaml", Code: strings.TrimSuffix(k3s.String(), "\n"), Help: tr(lang, "usage.k3sHelp")})

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
	out = append(out, snippetView{ID: "snippet-podman", Label: "/etc/containers/registries.conf", Code: strings.TrimSuffix(pod.String(), "\n"), Help: tr(lang, "usage.podmanHelp")})

	pull := "docker pull " + host + "/library/nginx:latest\ndocker pull " + host + "/ghcr.io/<owner>/<image>:<tag>\nnerdctl pull " + host + "/quay.io/<owner>/<image>:<tag>"
	out = append(out, snippetView{ID: "snippet-pull", Label: tr(lang, "usage.pullLabel"), Code: pull, Help: tr(lang, "usage.pullHelp")})
	return out
}
