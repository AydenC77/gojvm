// Package project loads gojvm.json project configuration and drives builds;
// it is shared by the command line tool and the language server.
package project

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/AydenC77/gojvm/internal/compiler"
	"github.com/AydenC77/gojvm/internal/jvm"
	"github.com/AydenC77/gojvm/internal/maven"
)

const ConfigName = "gojvm.json"

// Config is the contents of gojvm.json. Every field is optional.
type Config struct {
	Classpath    []string `json:"classpath,omitempty"`    // extra jars (relative to gojvm.json)
	Repositories []string `json:"repositories,omitempty"` // extra Maven repository URLs (Maven Central is always used)
	Dependencies []string `json:"dependencies,omitempty"` // Maven coordinates: group:artifact:version
	Resources    string   `json:"resources,omitempty"`    // directory bundled into the jar root
	Output       string   `json:"output,omitempty"`       // output jar path
	JDK          string   `json:"jdk,omitempty"`          // JDK 21 home

	Dir   string `json:"-"` // directory holding gojvm.json (or the start directory)
	Found bool   `json:"-"`
}

// Find walks up from start looking for gojvm.json. When none exists it returns
// a default Config rooted at start.
func Find(start string) (*Config, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(abs); err == nil && !st.IsDir() {
		abs = filepath.Dir(abs)
	}
	for d := abs; ; d = filepath.Dir(d) {
		p := filepath.Join(d, ConfigName)
		if b, err := os.ReadFile(p); err == nil {
			cfg := &Config{}
			if err := json.Unmarshal(b, cfg); err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			cfg.Dir, cfg.Found = d, true
			return cfg, nil
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	return &Config{Dir: abs}, nil
}

func (c *Config) resolve(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.Dir, p)
}

// Jars returns the classpath jars as absolute paths.
func (c *Config) Jars() []string {
	var out []string
	for _, j := range c.Classpath {
		out = append(out, c.resolve(j))
	}
	return out
}

func (c *Config) ResourcesDir() string { return c.resolve(c.Resources) }
func (c *Config) OutputPath() string   { return c.resolve(c.Output) }

// GoFiles lists the non-test .go files in dir.
func GoFiles(dir string) ([]string, error) {
	m, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range m {
		if !strings.HasSuffix(f, "_test.go") {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Session holds an open classpath for a project.
type Session struct {
	Cfg  *Config
	CP   *jvm.ClassPath
	Jars []string // every jar on the classpath: gojvm.json classpath + resolved dependencies
}

func Open(cfg *Config, jdkOverride string, extraJars ...string) (*Session, error) {
	jdk := jdkOverride
	if jdk == "" {
		jdk = cfg.JDK
	}
	jars := append(cfg.Jars(), extraJars...)
	if len(cfg.Dependencies) > 0 {
		resolved, err := maven.NewResolver(cfg.Repositories, os.Stderr).Resolve(cfg.Dependencies)
		if err != nil {
			return nil, err
		}
		jars = append(jars, resolved...)
	}
	cp, err := jvm.NewClassPath(jdk, jars...)
	if err != nil {
		return nil, err
	}
	return &Session{Cfg: cfg, CP: cp, Jars: jars}, nil
}

func (s *Session) Close() { s.CP.Close() }

// Parse parses files; overlay maps absolute file paths to unsaved contents.
func Parse(files []string, overlay map[string]string) (*token.FileSet, []*ast.File, []compiler.Diag) {
	fset := token.NewFileSet()
	var asts []*ast.File
	var diags []compiler.Diag
	for _, f := range files {
		var src any
		if t, ok := overlay[f]; ok {
			src = t
		}
		a, err := parser.ParseFile(fset, f, src, parser.SkipObjectResolution|parser.ParseComments)
		if err != nil {
			if el, ok := err.(scanner.ErrorList); ok {
				for _, e := range el {
					diags = append(diags, compiler.Diag{File: f, Line: e.Pos.Line, Col: e.Pos.Column, Msg: e.Msg})
				}
			} else {
				diags = append(diags, compiler.Diag{File: f, Line: 1, Col: 1, Msg: err.Error()})
			}
		}
		if a != nil {
			asts = append(asts, a)
		}
	}
	return fset, asts, diags
}

// Build parses and compiles files.
func (s *Session) Build(files []string, overlay map[string]string) (*compiler.Result, []compiler.Diag) {
	fset, asts, diags := Parse(files, overlay)
	if len(diags) > 0 {
		return nil, diags
	}
	return compiler.Compile(fset, asts, s.CP)
}

// Complete compiles files with the completion marker already substituted into
// the overlay and returns the completion items at the marker.
func (s *Session) Complete(files []string, overlay map[string]string) []compiler.CompletionItem {
	fset, asts, diags := Parse(files, overlay)
	if len(diags) > 0 && len(asts) == 0 {
		return nil
	}
	out := compiler.CompileWith(fset, asts, s.CP, compiler.Options{Complete: true})
	return out.Items
}

// LoadResources reads every file under dir into a jar-path -> contents map.
func LoadResources(dir string) (map[string][]byte, error) {
	res := map[string][]byte{}
	if dir == "" {
		return res, nil
	}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		res[filepath.ToSlash(rel)] = data
		return nil
	})
	return res, err
}
