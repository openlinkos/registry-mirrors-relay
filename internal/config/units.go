package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Duration is a duration written as "30s" or "6h".
type Duration time.Duration

// D returns the time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML accepts Go duration syntax, plus a number of days ending in d (such as "7d").
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := parseDuration(s)
	if err != nil {
		return fmt.Errorf("第 %d 行：%w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML outputs a string, so that check -print can print it.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("时长 %q 写法不对", s)
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	v, err := time.ParseDuration(s)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("时长 %q 写法不对，应写成 30s、5m、6h、7d 这样", s)
	}
	return v, nil
}

// Size is a byte count written as "64MiB" or "32KiB", or as a plain integer.
type Size int64

var sizeUnits = []struct {
	suffix string
	mult   int64
}{
	{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
	{"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3}, {"B", 1},
}

// UnmarshalYAML accepts an integer or a string with a unit.
func (s *Size) UnmarshalYAML(n *yaml.Node) error {
	var raw string
	if err := n.Decode(&raw); err != nil {
		return err
	}
	v, err := parseSize(raw)
	if err != nil {
		return fmt.Errorf("第 %d 行：%w", n.Line, err)
	}
	*s = Size(v)
	return nil
}

// MarshalYAML outputs the value in the largest unit that divides it exactly.
func (s Size) MarshalYAML() (any, error) { return s.String(), nil }

func (s Size) String() string {
	for _, u := range sizeUnits[:3] {
		if s != 0 && int64(s)%u.mult == 0 {
			return strconv.FormatInt(int64(s)/u.mult, 10) + u.suffix
		}
	}
	return strconv.FormatInt(int64(s), 10)
}

func parseSize(raw string) (int64, error) {
	t := strings.TrimSpace(raw)
	for _, u := range sizeUnits {
		if num, ok := strings.CutSuffix(t, u.suffix); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(num), 10, 64)
			if err != nil || n < 0 {
				return 0, fmt.Errorf("大小 %q 写法不对", raw)
			}
			return n * u.mult, nil
		}
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("大小 %q 写法不对，应写成 64MiB、32KiB 这样", raw)
	}
	return n, nil
}
