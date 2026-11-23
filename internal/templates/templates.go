// Package templates implements `gojvm new`: project templates (Minecraft
// plugins and more) embedded in the binary. A template is a folder with a
// template.json descriptor and a files/ tree rendered with text/template using
// [[ ]] delimiters.
package templates

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
)

//go:embed all:builtin
var builtinFS embed.FS

// Property is one question a template asks.
type Property struct {
	Name    string   `json:"name"`
	Label   string   `json:"label"`
	Type    string   `json:"type"` // string | bool | select
	Default string   `json:"default,omitempty"`
	Options []string `json:"options,omitempty"`
	Dynamic string   `json:"dynamic,omitempty"` // option provider, e.g. paper_versions
	Help    string   `json:"help,omitempty"`
}

// Derived is a computed value, evaluated in order after the answers are known.
type Derived struct {
	Name string `json:"name"`
	Expr string `json:"expr"`
}

// Manifest is the parsed template.json.
type Manifest struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Group       string     `json:"group"`
	Description string     `json:"description"`
	Properties  []Property `json:"properties"`
	Derived     []Derived  `json:"derived,omitempty"`
	Notes       string     `json:"notes,omitempty"` // shown after creation (template syntax)
}

// Template is a loaded template.
type Template struct {
	Manifest
	root fs.FS // the template directory
}

// All returns every built-in template, sorted by group then name.
func All() ([]*Template, error) {
	entries, err := fs.ReadDir(builtinFS, "builtin")
	if err != nil {
		return nil, err
	}
	var out []*Template
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t, err := load(e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Find returns the template with the given id.
func Find(id string) (*Template, error) {
	all, err := All()
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, t := range all {
		if t.ID == id {
			return t, nil
		}
		ids = append(ids, t.ID)
	}
	return nil, fmt.Errorf("unknown template %q (available: %s)", id, strings.Join(ids, ", "))
}

func load(dir string) (*Template, error) {
	sub, err := fs.Sub(builtinFS, "builtin/"+dir)
	if err != nil {
		return nil, err
	}
	b, err := fs.ReadFile(sub, "template.json")
	if err != nil {
		return nil, fmt.Errorf("template %s: %w", dir, err)
	}
	t := &Template{root: sub}
	if err := json.Unmarshal(b, &t.Manifest); err != nil {
		return nil, fmt.Errorf("template %s: %w", dir, err)
	}
	if t.ID == "" {
		t.ID = dir
	}
	return t, nil
}

// ResolveOptions fills in dynamic option lists (this may use the network; a
// provider that fails falls back to the static options in the descriptor).
func (t *Template) ResolveOptions() {
	for i := range t.Properties {
		p := &t.Properties[i]
		if p.Dynamic == "" {
			continue
		}
		if opts, err := providers[p.Dynamic](); err == nil && len(opts) > 0 {
			p.Options = opts
			p.Default = opts[0]
		}
	}
}

// Defaults returns the default answer for every property.
func (t *Template) Defaults() map[string]string {
	m := map[string]string{}
	for _, p := range t.Properties {
		d := p.Default
		if d == "" && len(p.Options) > 0 {
			d = p.Options[0]
		}
		m[p.Name] = d
	}
	return m
}

func (t *Template) data(vals map[string]string) (map[string]any, error) {
	data := map[string]any{}
	for _, p := range t.Properties {
		v, ok := vals[p.Name]
		if !ok {
			v = t.Defaults()[p.Name]
		}
		switch p.Type {
		case "bool":
			data[p.Name] = v == "true" || v == "yes" || v == "y" || v == "1"
		default:
			data[p.Name] = v
		}
		if p.Type == "select" && len(p.Options) > 0 && v != "" {
			found := false
			for _, o := range p.Options {
				found = found || o == v
			}
			if !found {
				return nil, fmt.Errorf("%s: %q is not one of %s", p.Name, v, strings.Join(p.Options, ", "))
			}
		}
	}
	for _, d := range t.Derived {
		s, err := render(d.Expr, data)
		if err != nil {
			return nil, fmt.Errorf("derived %s: %w", d.Name, err)
		}
		data[d.Name] = s
	}
	return data, nil
}

func render(text string, data map[string]any) (string, error) {
	tpl, err := template.New("t").Delims("[[", "]]").Funcs(funcs).Option("missingkey=error").Parse(text)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	if err := tpl.Execute(&sb, data); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// Generate renders the template into dir. It refuses to overwrite existing files.
func (t *Template) Generate(vals map[string]string, dir string) (created []string, notes string, err error) {
	data, err := t.data(vals)
	if err != nil {
		return nil, "", err
	}
	type out struct {
		path string
		body string
	}
	var files []out
	err = fs.WalkDir(t.root, "files", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(p, "files/")
		relOut, err := render(rel, data)
		if err != nil {
			return fmt.Errorf("file name %s: %w", rel, err)
		}
		relOut = strings.TrimSuffix(relOut, ".tmpl")
		raw, err := fs.ReadFile(t.root, p)
		if err != nil {
			return err
		}
		body := string(raw)
		if strings.HasSuffix(rel, ".tmpl") {
			if body, err = render(body, data); err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
		}
		files = append(files, out{path.Clean(relOut), body})
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f.path))); err == nil {
			return nil, "", fmt.Errorf("%s already exists; choose an empty directory", f.path)
		}
	}
	for _, f := range files {
		full := filepath.Join(dir, filepath.FromSlash(f.path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return nil, "", err
		}
		if err := os.WriteFile(full, []byte(f.body), 0o644); err != nil {
			return nil, "", err
		}
		created = append(created, f.path)
	}
	if t.Notes != "" {
		notes, _ = render(t.Notes, data)
	}
	return created, notes, nil
}
