package i18n

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// repoRoot is the module root, two levels above this package.
const repoRoot = "../.."

// i18nCalls are the package's functions that take a format string, by argument position.
var i18nCalls = map[string]int{"Errorf": 0, "M": 0, "Coded": 1, "T": 1}

// wrappers are helpers elsewhere that pass their format string on to this package, by package name,
// function name and argument position. Their calls must use literal formats too.
var wrappers = map[string]int{
	"config.bad":  0,
	"config.warn": 0,
	"ops.bad":     2,
}

// formats collects the literal format strings passed to this package and to the wrappers, with the
// place each one is used.
func formats(t *testing.T) map[string][]string {
	t.Helper()
	fset := token.NewFileSet()
	found := map[string][]string{}
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "node_modules" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		pkg := f.Name.Name
		if pkg == "i18n" {
			return nil
		}
		params := map[string]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			if ft, ok := n.(*ast.FuncType); ok && ft.Params != nil {
				for _, p := range ft.Params.List {
					for _, name := range p.Names {
						params[name.Name] = true
					}
				}
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			pos := -1
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				if x, ok := fn.X.(*ast.Ident); ok && x.Name == "i18n" {
					if i, ok := i18nCalls[fn.Sel.Name]; ok {
						pos = i
					}
				}
			case *ast.Ident:
				if i, ok := wrappers[pkg+"."+fn.Name]; ok {
					pos = i
				}
			}
			if pos < 0 || pos >= len(call.Args) {
				return true
			}
			where := fset.Position(call.Pos()).String()
			s, ok := literal(call.Args[pos])
			if !ok {
				// A wrapper passing its own format parameter on is fine; anything else is not.
				if id, isIdent := call.Args[pos].(*ast.Ident); !isIdent || !params[id.Name] {
					t.Errorf("%s: the format string is not a literal", where)
				}
				return true
			}
			found[s] = append(found[s], where)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// literal evaluates a string literal or a concatenation of string literals.
func literal(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		a, ok1 := literal(v.X)
		b, ok2 := literal(v.Y)
		return a + b, ok1 && ok2
	case *ast.ParenExpr:
		return literal(v.X)
	}
	return "", false
}

// verbs maps each argument index used by a format string to its verb (%w counts as %v).
func verbs(f string) (map[int]byte, error) {
	out := map[int]byte{}
	arg := 0
	for i := 0; i < len(f); i++ {
		if f[i] != '%' {
			continue
		}
		i++
		if i < len(f) && f[i] == '%' {
			continue
		}
		for i < len(f) && strings.IndexByte("+-# 0", f[i]) >= 0 {
			i++
		}
		if i < len(f) && f[i] == '[' {
			j := strings.IndexByte(f[i:], ']')
			if j < 0 {
				return nil, fmt.Errorf("unclosed argument index in %q", f)
			}
			n, err := strconv.Atoi(f[i+1 : i+j])
			if err != nil || n < 1 {
				return nil, fmt.Errorf("bad argument index in %q", f)
			}
			arg = n - 1
			i += j + 1
		}
		for i < len(f) && (f[i] >= '0' && f[i] <= '9' || f[i] == '.') {
			i++
		}
		if i >= len(f) {
			return nil, fmt.Errorf("%q ends inside a verb", f)
		}
		v := f[i]
		if v == 'w' {
			v = 'v'
		}
		if old, ok := out[arg]; ok && old != v {
			return nil, fmt.Errorf("argument %d has two verbs in %q", arg+1, f)
		}
		out[arg] = v
		arg++
	}
	return out, nil
}

func TestCatalogue(t *testing.T) {
	used := formats(t)
	var missing []string
	for f, places := range used {
		tr, ok := zh[f]
		if !ok {
			missing = append(missing, fmt.Sprintf("%q (%s)", f, places[0]))
			continue
		}
		ve, err1 := verbs(f)
		vz, err2 := verbs(tr)
		switch {
		case err1 != nil:
			t.Error(err1)
		case err2 != nil:
			t.Error(err2)
		case fmt.Sprint(ve) != fmt.Sprint(vz):
			t.Errorf("%q: the translation uses the arguments differently: %v, want %v", f, vz, ve)
		}
	}
	slices.Sort(missing)
	for _, m := range missing {
		t.Errorf("no Chinese translation for %s", m)
	}
	for f := range zh {
		if _, ok := used[f]; !ok {
			t.Errorf("unused catalogue entry %q", f)
		}
	}
}

// Code, comments, scripts and deployment files are written in English. Chinese text lives only in the
// catalogues and the Chinese documentation.
var hanExempt = []string{
	".git/",
	"node_modules/",
	"docs/",
	"internal/i18n/zh.go",
	"internal/i18n/i18n_test.go", // tests the translations
	"internal/admin/messages_zh.go",
	"internal/web/messages_zh.go",
	"internal/ui/vendor/",
}

// hanPending lists files still being translated. It must shrink to nothing.
var hanPending = []string{}

var han = regexp.MustCompile(`[\p{Han}\x{3000}-\x{303F}\x{FF00}-\x{FFEF}]`)

func TestNoChinese(t *testing.T) {
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	skip := func(rel string) bool {
		if strings.HasSuffix(rel, ".md") {
			return true
		}
		for _, p := range slices.Concat(hanExempt, hanPending) {
			if rel == p || strings.HasSuffix(p, "/") && strings.HasPrefix(rel+"/", p) {
				return true
			}
		}
		return false
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if skip(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if slices.Contains(b, 0) {
			return nil // binary
		}
		for i, line := range strings.Split(string(b), "\n") {
			if han.MatchString(line) {
				t.Errorf("%s:%d: Chinese text outside the catalogues: %s", rel, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
