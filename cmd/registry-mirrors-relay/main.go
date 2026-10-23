// registry-mirrors-relay：只做转发、不落盘的容器镜像加速服务。
//
//	registry-mirrors-relay [serve] [-config 文件]   启动服务（默认子命令）
//	registry-mirrors-relay check [-config 文件] [-print]
//	registry-mirrors-relay hash-password [-user 名字] [-cost 12]
//	registry-mirrors-relay probe [-url http://127.0.0.1:8080/healthz]
//	registry-mirrors-relay version
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"

	"github.com/openlinkos/registry-mirrors-relay/internal/accesslog"
	"github.com/openlinkos/registry-mirrors-relay/internal/clientip"
	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/relay"
	"github.com/openlinkos/registry-mirrors-relay/internal/route"
	"github.com/openlinkos/registry-mirrors-relay/internal/server"
	"github.com/openlinkos/registry-mirrors-relay/internal/upstream"
	"github.com/openlinkos/registry-mirrors-relay/internal/version"
	"github.com/openlinkos/registry-mirrors-relay/internal/web"
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
  registry-mirrors-relay probe [-url 地址]             健康检查，给容器的 HEALTHCHECK 用
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

	h, closeFn, err := buildHandler(res.Config, log)
	if err != nil {
		log.Error("启动失败", "err", err)
		return 1
	}
	defer closeFn()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := server.New(res.Config, log, h)
	if err := srv.Run(ctx); err != nil {
		log.Error("退出", "err", err)
		return 1
	}
	log.Info("已退出")
	return 0
}

// buildHandler 由配置组装拉取入口的处理器。返回的 close 函数在退出时写完访问日志、关闭上游连接。
func buildHandler(cfg *config.Config, log *slog.Logger) (http.Handler, func(), error) {
	trusted, err := config.ParsePrefixes(cfg.ClientIP.TrustedProxies)
	if err != nil {
		return nil, nil, err
	}
	pool, err := upstream.NewPool(cfg, upstream.Options{Log: log})
	if err != nil {
		return nil, nil, err
	}
	wh, err := web.New(cfg)
	if err != nil {
		return nil, nil, err
	}
	al, err := accesslog.New(cfg.Log.Access)
	if err != nil {
		return nil, nil, fmt.Errorf("访问日志：%w", err)
	}
	h := relay.New(relay.Deps{
		Config: cfg,
		Router: route.New(cfg),
		Pool:   pool,
		IPs:    clientip.New(trusted, cfg.ClientIP.UseXRealIP),
		Web:    wh,
		Access: al,
		Log:    log,
	})
	return h, func() {
		if n := al.Dropped(); n > 0 {
			log.Warn("访问日志队列满时丢弃过记录", "count", n)
		}
		_ = al.Close()
		pool.Close()
	}, nil
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

func cmdProbe(args []string, stderr io.Writer) int {
	fl := flag.NewFlagSet("probe", flag.ContinueOnError)
	fl.SetOutput(stderr)
	url := fl.String("url", "http://127.0.0.1:8080/healthz", "探测地址")
	timeout := fl.Duration("timeout", 3*time.Second, "超时")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, *url, nil)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(stderr, "状态码", resp.StatusCode)
		return 1
	}
	return 0
}
