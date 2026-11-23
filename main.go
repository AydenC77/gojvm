// Command gojvm compiles Go source to JVM bytecode (Java 21) packaged as a jar.
//
//	go install github.com/AydenC77/gojvm@latest
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/AydenC77/gojvm/internal/compiler"
	"github.com/AydenC77/gojvm/internal/jvm"
	"github.com/AydenC77/gojvm/internal/lsp"
	"github.com/AydenC77/gojvm/internal/project"
)

const usageText = `gojvm - compile Go to JVM bytecode (Java 21) and run it anywhere Java runs

Usage:
  gojvm new   [-t template] [dir] create a project from a template (Minecraft plugins, ...)
  gojvm init  [dir] [-lib name]   create a basic project (-lib: a library package)
  gojvm run   [path] [-- args]    compile and run a program
  gojvm build [path] [-o out.jar] compile to a jar (library jar if the package isn't main)
  gojvm check [path]              compile only, report errors
  gojvm doc   <java/pkg[/Class]>  show what a Java package or class offers, Go-style
  gojvm lsp                       language server (used by the VS Code extension)
  gojvm version                   show version and detected JDK

path is a directory (default ".") or a list of .go files. Project settings live in
gojvm.json (classpath jars, resources dir, output jar, jdk). Flags: -cp, -res, -jdk.`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usageText)
		os.Exit(2)
	}
	args := os.Args[2:]
	var code int
	switch os.Args[1] {
	case "build":
		code = cmdBuild(args, modeBuild)
	case "run":
		code = cmdBuild(args, modeRun)
	case "check":
		code = cmdBuild(args, modeCheck)
	case "init":
		code = cmdInit(args)
	case "new":
		code = cmdNew(args)
	case "doc":
		code = cmdDoc(args)
	case "lsp":
		code = lsp.Serve(os.Stdin, os.Stdout, os.Stderr)
	case "version", "-v", "--version":
		code = cmdVersion()
	case "help", "-h", "--help":
		fmt.Println(usageText)
	default:
		fmt.Fprintf(os.Stderr, "gojvm: unknown command %q\n\n%s\n", os.Args[1], usageText)
		code = 2
	}
	os.Exit(code)
}

func version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

func cmdVersion() int {
	fmt.Println("gojvm", version())
	if home, err := jvm.FindJDK(""); err == nil {
		fmt.Println("jdk:  ", home)
	} else {
		fmt.Println("jdk:   not found (", err, ")")
	}
	return 0
}

type mode int

const (
	modeBuild mode = iota
	modeRun
	modeCheck
)

// printDiags prints diagnostics with the offending source line and a caret.
func printDiags(diags []compiler.Diag) {
	cache := map[string][]string{}
	for _, d := range diags {
		fmt.Fprintln(os.Stderr, d.Error())
		if d.File == "" || d.Line <= 0 {
			continue
		}
		lines, ok := cache[d.File]
		if !ok {
			if b, err := os.ReadFile(d.File); err == nil {
				lines = strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
			}
			cache[d.File] = lines
		}
		if d.Line > len(lines) {
			continue
		}
		src := strings.ReplaceAll(lines[d.Line-1], "\t", "    ")
		col := d.Col
		if col < 1 {
			col = 1
		}
		pad := len(strings.ReplaceAll(lines[d.Line-1][:min(col-1, len(lines[d.Line-1]))], "\t", "    "))
		fmt.Fprintf(os.Stderr, "  %4d | %s\n       | %s^\n", d.Line, src, strings.Repeat(" ", pad))
	}
}

func cmdBuild(args []string, m mode) int {
	var progArgs []string
	for i, a := range args {
		if a == "--" {
			progArgs = args[i+1:]
			args = args[:i]
			break
		}
	}
	fs := flag.NewFlagSet("gojvm", flag.ContinueOnError)
	out := fs.String("o", "", "output jar path")
	jdk := fs.String("jdk", "", "JDK 21 home")
	cpFlag := fs.String("cp", "", "extra classpath jars ("+string(os.PathListSeparator)+"-separated)")
	resFlag := fs.String("res", "", "directory of resources to bundle (e.g. plugin.yml)")
	// allow flags after the path as well as before it
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
	if len(pos) == 0 {
		pos = []string{"."}
	}
	var files []string
	for _, a := range pos {
		st, err := os.Stat(a)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gojvm:", err)
			return 1
		}
		if st.IsDir() {
			fl, _ := project.GoFiles(a)
			files = append(files, fl...)
		} else {
			files = append(files, a)
		}
	}
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "gojvm: no Go files found (try `gojvm init` to create a project)")
		return 1
	}
	cfg, err := project.Find(files[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "gojvm:", err)
		return 1
	}
	var extra []string
	if *cpFlag != "" {
		extra = filepath.SplitList(*cpFlag)
	}
	sess, err := project.Open(cfg, *jdk, extra...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gojvm:", err)
		return 1
	}
	defer sess.Close()

	res, diags := sess.Build(files, nil)
	if len(diags) > 0 {
		printDiags(diags)
		n := len(diags)
		fmt.Fprintf(os.Stderr, "\n%d error%s\n", n, map[bool]string{true: "s", false: ""}[n != 1])
		return 1
	}
	if m == modeCheck {
		fmt.Println("ok")
		return 0
	}
	if m == modeRun && res.MainClass == "" {
		fmt.Fprintln(os.Stderr, "gojvm: cannot run a library package (package is not main); use `gojvm build`")
		return 1
	}
	resDir := cfg.ResourcesDir()
	if *resFlag != "" {
		resDir = *resFlag
	}
	resources, err := project.LoadResources(resDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gojvm:", err)
		return 1
	}
	jar := *out
	if jar == "" && m == modeBuild {
		jar = cfg.OutputPath()
		if jar == "" {
			base := strings.TrimSuffix(filepath.Base(files[0]), ".go")
			if len(files) > 1 || filepath.Ext(pos[0]) != ".go" {
				abs, _ := filepath.Abs(filepath.Dir(files[0]))
				base = filepath.Base(abs)
			}
			jar = base + ".jar"
		}
	}
	if m == modeRun {
		d, _ := os.MkdirTemp("", "gojvm")
		defer os.RemoveAll(d)
		jar = filepath.Join(d, "out.jar")
	}
	var buf bytes.Buffer
	if err := jvm.WriteJar(&buf, res.MainClass, res.Classes, resources); err != nil {
		fmt.Fprintln(os.Stderr, "gojvm:", err)
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(jar), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "gojvm:", err)
		return 1
	}
	if err := os.WriteFile(jar, buf.Bytes(), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "gojvm:", err)
		return 1
	}
	if m == modeBuild {
		kind := "jar"
		if res.MainClass == "" {
			kind = "library jar"
		}
		fmt.Printf("built %s (%s)\n", jar, kind)
		return 0
	}
	cpath := strings.Join(append([]string{jar}, sess.Jars...), string(os.PathListSeparator))
	cmd := exec.Command(filepath.Join(sess.CP.Home, "bin", "java"),
		append([]string{"-cp", cpath, strings.ReplaceAll(res.MainClass, "/", ".")}, progArgs...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "gojvm:", err)
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------

const mainTemplate = `package main

import (
	"fmt"
	"java/util"
)

func main() {
	names := util.NewArrayList()
	names.Add("Go")
	names.Add("on the")
	names.Add("JVM")
	for i := 0; i < int(names.Size()); i++ {
		fmt.Println(i, names.Get(int32(i)).(string))
	}
}
`

const libTemplate = `package %s

import "fmt"

// A library package builds a jar with no entry point. Extend Java classes with
// directives, e.g. for a plugin API on the classpath (see gojvm.json):
//
//	//gojvm:extends some/api/BasePlugin
//	//gojvm:implements java/lang/Runnable
type Greeter struct {
	Name string
}

func (g *Greeter) Greet() string {
	return fmt.Sprintf("hello, %%s", g.Name)
}
`

func cmdInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	lib := fs.String("lib", "", "create a library/plugin package with this name instead of a program")
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
	dir := "."
	if len(pos) > 0 {
		dir = pos[0]
	}
	if err := os.MkdirAll(filepath.Join(dir, ".vscode"), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "gojvm:", err)
		return 1
	}
	abs, _ := filepath.Abs(dir)
	name := filepath.Base(abs)
	src, srcName := mainTemplate, "main.go"
	if *lib != "" {
		src, srcName = fmt.Sprintf(libTemplate, *lib), *lib+".go"
	}
	files := map[string]string{
		srcName:                   src,
		"gojvm.json":              fmt.Sprintf("{\n  \"classpath\": [],\n  \"output\": \"build/%s.jar\"\n}\n", name),
		".gitignore":              "build/\n*.jar\n",
		".vscode/settings.json":   "{\n  \"go.useLanguageServer\": false,\n  \"[go]\": {\n    \"editor.formatOnSave\": true\n  }\n}\n",
		".vscode/extensions.json": "{\n  \"recommendations\": [\"aydenc77.gojvm\"]\n}\n",
	}
	var names []string
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := filepath.Join(dir, n)
		if _, err := os.Stat(p); err == nil {
			fmt.Println("exists  ", p)
			continue
		}
		if err := os.WriteFile(p, []byte(files[n]), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "gojvm:", err)
			return 1
		}
		fmt.Println("created ", p)
	}
	fmt.Println("\nnext: cd", dir, "&& gojvm run")
	return 0
}

// ---------------------------------------------------------------------------

func cmdDoc(args []string) int {
	fs := flag.NewFlagSet("doc", flag.ContinueOnError)
	jdk := fs.String("jdk", "", "JDK 21 home")
	cpFlag := fs.String("cp", "", "extra classpath jars")
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
	if len(pos) == 0 {
		fmt.Fprintln(os.Stderr, "usage: gojvm doc <java/pkg | java/pkg/Class> [filter]")
		return 2
	}
	cfg, _ := project.Find(".")
	var extra []string
	if *cpFlag != "" {
		extra = filepath.SplitList(*cpFlag)
	}
	sess, err := project.Open(cfg, *jdk, extra...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gojvm:", err)
		return 1
	}
	defer sess.Close()
	target := strings.ReplaceAll(strings.Trim(pos[0], `"`), ".", "/")
	filter := ""
	if len(pos) > 1 {
		filter = strings.ToLower(pos[1])
	}
	cp := sess.CP
	if ci := cp.Lookup(target); ci != nil {
		short := target[strings.LastIndex(target, "/")+1:]
		pkg := target[:strings.LastIndex(target, "/")+1]
		alias := pkg[strings.LastIndex(strings.TrimSuffix(pkg, "/"), "/")+1:]
		alias = strings.TrimSuffix(alias, "/")
		fmt.Printf("class %s", strings.ReplaceAll(target, "/", "."))
		if ci.Super != "" && ci.Super != "java/lang/Object" {
			fmt.Printf("  extends %s", strings.ReplaceAll(ci.Super, "/", "."))
		}
		if len(ci.Interfaces) > 0 {
			var is []string
			for _, i := range ci.Interfaces {
				is = append(is, strings.ReplaceAll(i, "/", "."))
			}
			fmt.Printf("  implements %s", strings.Join(is, ", "))
		}
		fmt.Printf("\nimport %q\n", strings.TrimSuffix(pkg, "/"))
		show := func(title string, lines []string) {
			var keep []string
			for _, l := range lines {
				if filter == "" || strings.Contains(strings.ToLower(l), filter) {
					keep = append(keep, l)
				}
			}
			if len(keep) == 0 {
				return
			}
			sort.Strings(keep)
			fmt.Printf("\n%s\n", title)
			for _, l := range keep {
				fmt.Println("  " + l)
			}
		}
		var ctors, smeth, imeth, sfield, ifield []string
		for _, m := range ci.Methods {
			if m.Name == "<init>" && m.Access&jvm.AccPublic != 0 {
				ctors = append(ctors, fmt.Sprintf("%s.New%s%s", alias, short, compiler.GoSignature(m.Desc)))
			}
		}
		for _, m := range cp.AllMethods(target, false) {
			line := goName(m.Name) + compiler.GoSignature(m.Desc)
			if m.Access&jvm.AccStatic != 0 {
				smeth = append(smeth, fmt.Sprintf("%s.%s.%s", alias, short, line))
			} else {
				imeth = append(imeth, line)
			}
		}
		for _, f := range cp.AllFields(target) {
			line := goName(f.Name)
			if f.Name == strings.ToUpper(f.Name) {
				line = f.Name
			}
			line += " " + compiler.TypeFromDesc(f.Desc).String()
			if f.Access&jvm.AccStatic != 0 {
				sfield = append(sfield, fmt.Sprintf("%s.%s.%s", alias, short, line))
			} else {
				ifield = append(ifield, line)
			}
		}
		show("constructors:", ctors)
		show("static methods:", smeth)
		show("static fields:", sfield)
		show("methods:", imeth)
		show("fields:", ifield)
		return 0
	}
	if classes := cp.ClassesIn(target); len(classes) > 0 {
		fmt.Printf("package %s  (import %q)\n\n", strings.ReplaceAll(target, "/", "."), target)
		for _, c := range classes {
			short := c[strings.LastIndex(c, "/")+1:]
			if filter == "" || strings.Contains(strings.ToLower(short), filter) {
				fmt.Println("  " + short)
			}
		}
		return 0
	}
	fmt.Fprintf(os.Stderr, "gojvm: no Java class or package %q found\n", pos[0])
	return 1
}

func goName(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
