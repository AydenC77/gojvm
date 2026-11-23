package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/AydenC77/gojvm/internal/project"
	"github.com/AydenC77/gojvm/internal/templates"
)

type setFlags map[string]string

func (s setFlags) String() string { return "" }
func (s setFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("--set wants KEY=VALUE, got %q", v)
	}
	s[strings.TrimSpace(k)] = val
	return nil
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func cmdNew(args []string) int {
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	id := fs.String("t", "", "template id (see --list)")
	list := fs.Bool("list", false, "list available templates")
	describe := fs.Bool("describe", false, "print a template's questions (with --json for editors)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	yes := fs.Bool("yes", false, "accept defaults instead of asking")
	noCheck := fs.Bool("no-check", false, "skip the test build after creating the project")
	sets := setFlags{}
	fs.Var(sets, "set", "answer a question: --set PROJECT_NAME=MyPlugin (repeatable)")
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return 2
		}
		args = fs.Args()
		if len(args) > 0 {
			pos = append(pos, args[0])
			args = args[1:]
		}
	}

	all, err := templates.All()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gojvm:", err)
		return 1
	}
	if *list {
		if *asJSON {
			type item struct {
				ID          string `json:"id"`
				Name        string `json:"name"`
				Group       string `json:"group"`
				Description string `json:"description"`
			}
			var out []item
			for _, t := range all {
				out = append(out, item{t.ID, t.Name, t.Group, t.Description})
			}
			b, _ := json.MarshalIndent(out, "", "  ")
			fmt.Println(string(b))
			return 0
		}
		group := ""
		for _, t := range all {
			if t.Group != group {
				group = t.Group
				fmt.Printf("\n%s\n", group)
			}
			fmt.Printf("  %-20s %s\n", t.ID, t.Description)
		}
		fmt.Println("\ncreate one with:  gojvm new -t <id> [dir]")
		return 0
	}

	interactive := !*yes && !*asJSON && isTerminal(os.Stdin)
	in := bufio.NewReader(os.Stdin)

	var tpl *templates.Template
	if *id != "" {
		if tpl, err = templates.Find(*id); err != nil {
			fmt.Fprintln(os.Stderr, "gojvm:", err)
			return 1
		}
	} else {
		if !interactive {
			fmt.Fprintln(os.Stderr, "gojvm: choose a template with -t (see `gojvm new --list`)")
			return 2
		}
		fmt.Println("What do you want to create?")
		for i, t := range all {
			fmt.Printf("  %d) %-22s %s\n", i+1, t.Name, t.Description)
		}
		n, _ := strconv.Atoi(ask(in, "Template number", "1"))
		if n < 1 || n > len(all) {
			fmt.Fprintln(os.Stderr, "gojvm: invalid choice")
			return 2
		}
		tpl = all[n-1]
	}

	if !*asJSON {
		fmt.Fprintln(os.Stderr, "gojvm: looking up versions...")
	}
	tpl.ResolveOptions()

	if *describe {
		b, _ := json.MarshalIndent(tpl.Manifest, "", "  ")
		fmt.Println(string(b))
		return 0
	}

	vals := tpl.Defaults()
	for k, v := range sets {
		vals[k] = v
	}
	if interactive {
		fmt.Printf("\n%s\n", tpl.Name)
		for _, p := range tpl.Properties {
			if _, preset := sets[p.Name]; preset {
				continue
			}
			vals[p.Name] = prompt(in, p, vals[p.Name])
		}
	}

	dir := ""
	if len(pos) > 0 {
		dir = pos[0]
	} else {
		name := strings.Join(strings.Fields(vals["PROJECT_NAME"]), "")
		if name == "" {
			name = tpl.ID
		}
		dir = name
		if interactive {
			dir = ask(in, "Directory", name)
		}
	}
	files, notes, err := tpl.Generate(vals, dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gojvm:", err)
		return 1
	}
	fmt.Printf("\ncreated %s\n", dir)
	for _, f := range files {
		fmt.Println("  ", f)
	}

	if !*noCheck {
		fmt.Print("\nfetching dependencies and test-building... ")
		if msg := testBuild(dir); msg != "" {
			fmt.Println("problem:")
			fmt.Fprintln(os.Stderr, msg)
		} else {
			fmt.Println("ok")
		}
	}
	if notes != "" {
		fmt.Println("\n" + notes)
	}
	fmt.Printf("\nnext: cd %s\n", dir)
	return 0
}

// testBuild compiles the new project once (also warming the dependency cache).
func testBuild(dir string) string {
	files, _ := project.GoFiles(dir)
	cfg, err := project.Find(dir)
	if err != nil {
		return err.Error()
	}
	sess, err := project.Open(cfg, "")
	if err != nil {
		return err.Error()
	}
	defer sess.Close()
	_, diags := sess.Build(files, nil)
	var sb strings.Builder
	for _, d := range diags {
		sb.WriteString(d.Error() + "\n")
	}
	return strings.TrimSpace(sb.String())
}

func ask(in *bufio.Reader, label, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", label, def)
	} else {
		fmt.Printf("%s: ", label)
	}
	line, _ := in.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

func prompt(in *bufio.Reader, p templates.Property, def string) string {
	label := p.Label
	if label == "" {
		label = p.Name
	}
	switch p.Type {
	case "bool":
		d := "n"
		if def == "true" {
			d = "y"
		}
		a := strings.ToLower(ask(in, label+" (y/n)", d))
		if strings.HasPrefix(a, "y") {
			return "true"
		}
		return "false"
	case "select":
		shown := p.Options
		if len(shown) > 12 {
			shown = shown[:12]
		}
		fmt.Printf("%s:\n", label)
		for i, o := range shown {
			mark := " "
			if o == def {
				mark = "*"
			}
			fmt.Printf("  %s %d) %s\n", mark, i+1, o)
		}
		if len(p.Options) > len(shown) {
			fmt.Printf("    ... %d more; type a value to use one\n", len(p.Options)-len(shown))
		}
		a := ask(in, "Choice", def)
		if n, err := strconv.Atoi(a); err == nil && n >= 1 && n <= len(shown) {
			return shown[n-1]
		}
		return a
	}
	return ask(in, label, def)
}

var _ = filepath.Join
