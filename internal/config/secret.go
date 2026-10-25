package config

import (
	"os"
	"strings"

	"github.com/openlinkos/registry-mirrors-relay/internal/i18n"
)

// Secret is a secret read from an environment variable or a file (suited to k8s Secrets and systemd
// LoadCredential). It can also be written inline in the configuration with value, but check warns about it.
// It always prints and serialises as [redacted].
type Secret struct {
	Env   string `yaml:"env"`
	File  string `yaml:"file"`
	Value string `yaml:"value"`

	resolved string
}

// redacted is how a secret looks when printed.
const redacted = "[redacted]"

// IsSet reports whether the secret is set in the configuration.
func (s Secret) IsSet() bool { return s.Env != "" || s.File != "" || s.Value != "" }

// Reveal returns the resolved plain text. Call it only where the secret is actually used, and never log it.
func (s Secret) Reveal() string { return s.resolved }

func (s Secret) String() string { return redacted }

// GoString keeps %#v from printing the plain text.
func (s Secret) GoString() string { return redacted }

// MarshalYAML outputs only the source, never the plain text.
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

// resolve reads the environment variable or the file. Trailing newlines in the file are removed.
func (s *Secret) resolve() error {
	n := 0
	for _, v := range []string{s.Env, s.File, s.Value} {
		if v != "" {
			n++
		}
	}
	if n > 1 {
		return i18n.Errorf("set only one of env, file and value")
	}
	switch {
	case s.Env != "":
		v, ok := os.LookupEnv(s.Env)
		if !ok || v == "" {
			return i18n.Errorf("environment variable %s is not set", s.Env)
		}
		s.resolved = v
	case s.File != "":
		b, err := os.ReadFile(s.File)
		if err != nil {
			return i18n.Errorf("read %s: %w", s.File, err)
		}
		v := strings.TrimRight(string(b), "\r\n")
		if v == "" {
			return i18n.Errorf("%s is empty", s.File)
		}
		s.resolved = v
	case s.Value != "":
		s.resolved = s.Value
	}
	return nil
}
