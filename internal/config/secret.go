package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Secret 是一个密钥，从环境变量或文件读取（适合 k8s Secret 和 systemd LoadCredential）。
// 也可以用 value 直接写在配置里，但 check 会警告。打印和序列化时一律显示为 [redacted]。
type Secret struct {
	Env   string `yaml:"env"`
	File  string `yaml:"file"`
	Value string `yaml:"value"`

	resolved string
}

// redacted 是密钥打印出来的样子。
const redacted = "[redacted]"

// IsSet 表示配置里写了这个密钥。
func (s Secret) IsSet() bool { return s.Env != "" || s.File != "" || s.Value != "" }

// Reveal 返回解析后的明文。只在真正使用时调用，不要写进日志。
func (s Secret) Reveal() string { return s.resolved }

func (s Secret) String() string { return redacted }

// GoString 防止 %#v 打印出明文。
func (s Secret) GoString() string { return redacted }

// MarshalYAML 只输出来源，不输出明文。
func (s Secret) MarshalYAML() (any, error) {
	switch {
	case s.Env != "":
		return map[string]string{"env": s.Env}, nil
	case s.File != "":
		return map[string]string{"file": s.File}, nil
	case s.Value != "":
		return map[string]string{"value": redacted}, nil
	}
	return nil, nil
}

// resolve 读取环境变量或文件。文件末尾的换行会去掉。
func (s *Secret) resolve() error {
	n := 0
	for _, v := range []string{s.Env, s.File, s.Value} {
		if v != "" {
			n++
		}
	}
	if n > 1 {
		return errors.New("env、file、value 只能写一个")
	}
	switch {
	case s.Env != "":
		v, ok := os.LookupEnv(s.Env)
		if !ok || v == "" {
			return fmt.Errorf("环境变量 %s 没有设置", s.Env)
		}
		s.resolved = v
	case s.File != "":
		b, err := os.ReadFile(s.File)
		if err != nil {
			return fmt.Errorf("读取 %s：%w", s.File, err)
		}
		v := strings.TrimRight(string(b), "\r\n")
		if v == "" {
			return fmt.Errorf("%s 是空文件", s.File)
		}
		s.resolved = v
	case s.Value != "":
		s.resolved = s.Value
	}
	return nil
}
