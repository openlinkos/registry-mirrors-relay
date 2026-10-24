// registry-mirrors-relay：只做转发、不落盘的容器镜像加速服务。
//
//	registry-mirrors-relay [serve] [-config 文件]   启动服务（默认子命令）
//	registry-mirrors-relay check [-config 文件] [-print]
//	registry-mirrors-relay hash-password [-user 名字] [-cost 12]
//	registry-mirrors-relay admin create|password|list [-config 文件] [-name 名字] [-role admin|viewer]
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

// defaultConfigPath 是没有 -config 和 RELAY_CONFIG 时读取的配置文件。不存在时只用默认值启动。
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
	fmt.Fprintf(stderr, "不认识的子命令 %q\n\n", cmd)
	usage(stderr)
	return 2
}

func usage(w io.Writer) {
	fmt.Fprint(w, `用法：
  registry-mirrors-relay [serve] [-config 文件]        启动服务（默认）
  registry-mirrors-relay check [-config 文件] [-print] 校验配置，-print 打印生效的配置（密钥隐去）
  registry-mirrors-relay hash-password [-user 名字]    从标准输入读口令，输出 bcrypt 哈希（htpasswd 格式）
  registry-mirrors-relay admin create -name 名字 [-role admin|viewer]   新建后台管理员（口令从标准输入读）
  registry-mirrors-relay admin password -name 名字      重设后台管理员的口令（并启用）
  registry-mirrors-relay admin list                     列出后台管理员
                                                        admin 子命令要先停止服务：运营数据库同时只能被一个进程打开
  registry-mirrors-relay probe [-config 文件] [-url 地址] [-wait 时长]
                                                        健康检查，给容器的 HEALTHCHECK 用；默认按配置的端口
  registry-mirrors-relay version

配置文件依次取 -config、环境变量 RELAY_CONFIG、`+defaultConfigPath+`；都没有时只用默认值。
`)
}

// loadConfig 按 -config、RELAY_CONFIG、默认路径的顺序找配置。明确指定的文件不存在时报错；
// 默认路径不存在时只用默认值（docker run 不挂配置也能起来）。
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
	cfgPath := fl.String("config", "", "配置文件")
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
		log.Warn("没有找到配置文件，只用默认值启动", "looked_for", defaultConfigPath)
	} else {
		log.Info("已读取配置", "path", path)
	}
	for _, w := range res.Warnings {
		log.Warn(w)
	}
	log.Info("启动", "version", version.Version)
	server.SetMemoryLimit(log)

	var raw []byte
	if path != "" {
		raw, _ = os.ReadFile(path) //nolint:gosec // 配置文件路径由运维指定
	}
	a, err := app.New(res, raw, app.Options{ConfigPath: path, Log: log})
	if err != nil {
		log.Error("启动失败", "err", err)
		return 1
	}
	defer a.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go a.Watch(ctx)          // 配置文件变化时自动重载
	go a.Stats.Run(ctx)      // 每小时把统计存进运营数据库
	go a.WatchUpstreams(ctx) // 每分钟探测一次各上游的 /v2/
	watchSignals(ctx, a, log)

	m, err := mgmt.New(a, log)
	if err != nil {
		log.Error("启动失败", "err", err)
		return 1
	}
	cfg := res.Config
	var pull http.Handler = relay.New(relay.Deps{App: a, Log: log})
	var extras []server.Listener
	if cfg.Listen.Admin == "" {
		pull = m.Shared(pull) // 三合一：后台、API、指标和拉取共用端口
	} else {
		extras = append(extras, server.Listener{Name: "管理入口", Addr: cfg.Listen.Admin, Handler: m.Standalone(log)})
	}
	log.Info("管理入口", "listen", cmp.Or(cfg.Listen.Admin, "和拉取入口共用"), "public", cfg.Admin.Public, "allow", cfg.Admin.Allow,
		"proxied_allow", cfg.Admin.ProxiedAllow, "metrics", cfg.Metrics.Enabled)
	srv := server.New(cfg, log, pull, extras...)
	srv.ACMEHosts(func() []string { return a.Runtime().Config.TLS.ACME.Hosts })
	if err := srv.Run(ctx); err != nil {
		log.Error("退出", "err", err)
		return 1
	}
	log.Info("已退出")
	return 0
}

func cmdCheck(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("check", flag.ContinueOnError)
	fl.SetOutput(stderr)
	cfgPath := fl.String("config", "", "配置文件")
	print := fl.Bool("print", false, "打印生效的配置（密钥隐去）")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	res, path, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if path == "" {
		fmt.Fprintf(stderr, "没有找到配置文件（%s），检查的是默认值\n", defaultConfigPath)
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(stderr, "提醒：", w)
	}
	if *print {
		b, err := res.Config.Print()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		_, _ = stdout.Write(b)
	} else {
		fmt.Fprintln(stdout, "配置没有问题")
	}
	return 0
}

func cmdHashPassword(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("hash-password", flag.ContinueOnError)
	fl.SetOutput(stderr)
	user := fl.String("user", "", "用户名；给了就输出 用户名:哈希，可以直接追加到 htpasswd 文件")
	cost := fl.Int("cost", 12, "bcrypt cost（10–14）")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if *cost < 10 || *cost > 14 {
		fmt.Fprintln(stderr, "-cost 应在 10 到 14 之间")
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
			fmt.Fprintln(stderr, "用户名不能含冒号或换行")
			return 2
		}
		fmt.Fprintf(stdout, "%s:%s\n", *user, h)
	} else {
		fmt.Fprintln(stdout, string(h))
	}
	return 0
}

// readPassword 在终端上不回显地读口令（输两次）；从管道读时只读第一行。
func readPassword(stdin io.Reader, stderr io.Writer) (string, error) {
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(stderr, "口令：")
		a, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(stderr)
		if err != nil {
			return "", err
		}
		fmt.Fprint(stderr, "再输一次：")
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(stderr)
		if err != nil {
			return "", err
		}
		if string(a) != string(b) {
			return "", errors.New("两次输入的口令不一致")
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
		return "", errors.New("口令至少 12 个字符")
	case len(pw) > 72:
		return "", errors.New("口令最多 72 个字节（bcrypt 的上限）")
	}
	return pw, nil
}

// probeURL 由监听地址得到本机的 /healthz 地址：只写了端口（:8080）或监听所有地址时用 127.0.0.1。
func probeURL(l config.Listen) (url string, insecure bool, err error) {
	scheme, addr := "http", l.HTTP
	if addr == "" {
		scheme, addr, insecure = "https", l.HTTPS, true
	}
	if addr == "" {
		return "", false, errors.New("listen.http 和 listen.https 都没开，请用 -url 指定探测地址")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", false, fmt.Errorf("监听地址 %q 写法不对", addr)
	}
	if ip, perr := netip.ParseAddr(host); host == "" || (perr == nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return scheme + "://" + net.JoinHostPort(host, port) + "/healthz", insecure, nil
}

// cmdAdmin 在服务停止时管理后台管理员：create、password、list。直接打开运营数据库（bbolt 同时只能被一个进程打开，
// 服务在跑时会在 2 秒后报错）。操作记进审计，操作人是 cli:<系统用户>。
func cmdAdmin(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, "用法：registry-mirrors-relay admin create|password|list [-config 文件] [-name 名字] [-role admin|viewer]")
		return 2
	}
	sub, args := args[0], args[1:]
	fl := flag.NewFlagSet("admin "+sub, flag.ContinueOnError)
	fl.SetOutput(stderr)
	cfgPath := fl.String("config", "", "配置文件")
	name := fl.String("name", "", "管理员的用户名")
	role := fl.String("role", "admin", "角色：admin（可修改）或 viewer（只读）")
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
		fmt.Fprintln(stderr, "运营数据是只读的（admin.store.driver: none）：管理员来自配置里的 admin.initial_admin")
		return 1
	}
	if sub != "list" && !ops.ValidName(*name) {
		fmt.Fprintln(stderr, "要用 -name 给出用户名：字母、数字和 . _ -，以字母或数字开头，最长 64 个字符")
		return 2
	}
	st, err := store.OpenBolt(cfg.Admin.Store.Path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		fmt.Fprintln(stderr, "服务在运行时请先停止它，或者在网页后台里操作。")
		return 1
	}
	defer func() { _ = st.Close() }()
	actor := "cli:" + cmp.Or(os.Getenv("USER"), os.Getenv("USERNAME"), "unknown")

	switch sub {
	case "list":
		for _, a := range st.Data().Admins {
			status := "正常"
			if a.Disabled {
				status = "已停用"
			}
			fmt.Fprintln(stdout, strings.Join([]string{a.Name, a.Role, status, a.Created.Format(time.RFC3339)}, "  "))
		}
		return 0
	case "create", "password":
	default:
		fmt.Fprintf(stderr, "不认识的 admin 子命令 %q（create、password、list）\n", sub)
		return 2
	}
	if *role != "admin" && *role != "viewer" {
		fmt.Fprintln(stderr, "-role 只能是 admin 或 viewer")
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
				return fmt.Errorf("管理员 %s 已存在，要重设口令请用 admin password", *name)
			}
			d.Admins[i].Hash, d.Admins[i].Disabled, d.Admins[i].MustChange = hash, false, false
			return nil
		}
		if sub == "password" {
			return fmt.Errorf("没有管理员 %s，要新建请用 admin create", *name)
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
		fmt.Fprintf(stdout, "已创建管理员 %s（%s）\n", *name, *role)
	} else {
		fmt.Fprintf(stdout, "已重设 %s 的口令\n", *name)
	}
	return 0
}

// cmdProbe 是容器的 HEALTHCHECK：请求本机拉取入口的 /healthz。没给 -url 时按配置里的 listen.http（没开时用
// listen.https，不校验证书）拼出地址，这样改了端口也不用改健康检查。
// -wait 时失败了每秒重试一次，直到成功或超时：helm test 用它，刚建好的 Service 要过一会儿才连得通。
func cmdProbe(args []string, stderr io.Writer) int {
	fl := flag.NewFlagSet("probe", flag.ContinueOnError)
	fl.SetOutput(stderr)
	cfgPath := fl.String("config", "", "配置文件（没给 -url 时用来找端口）")
	url := fl.String("url", "", "探测地址，默认按配置的 listen.http 或 listen.https")
	timeout := fl.Duration("timeout", 3*time.Second, "每次请求的超时")
	wait := fl.Duration("wait", 0, "失败时每秒重试一次，最多等这么久")
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
			// 探测的是本机的 127.0.0.1，证书上没有这个地址；只看服务在不在
			client = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // 见上
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
		return fmt.Errorf("状态码 %d", resp.StatusCode)
	}
	return nil
}
