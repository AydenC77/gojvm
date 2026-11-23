package compiler_test

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AydenC77/gojvm/internal/compiler"
	"github.com/AydenC77/gojvm/internal/jvm"
)

func compileRun(t *testing.T, src string) (string, []string) {
	t.Helper()
	cp, err := jvm.NewClassPath("")
	if err != nil {
		t.Skip(err)
	}
	defer cp.Close()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "t.go", src, parser.SkipObjectResolution|parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	res, diags := compiler.Compile(fset, []*ast.File{f}, cp)
	if len(diags) > 0 {
		var errs []string
		for _, d := range diags {
			errs = append(errs, d.Error())
		}
		return "", errs
	}
	jar := filepath.Join(t.TempDir(), "t.jar")
	var buf bytes.Buffer
	if err := jvm.WriteJar(&buf, res.MainClass, res.Classes, nil); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(jar, buf.Bytes(), 0o644)
	out, err := exec.Command(filepath.Join(cp.Home, "bin", "java"), "-Xverify:all", "-jar", jar).CombinedOutput()
	if err != nil {
		t.Fatalf("java failed: %v\n%s", err, out)
	}
	return strings.ReplaceAll(string(out), "\r\n", "\n"), nil
}

func TestPrograms(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"arith", `package main
import "fmt"
func main() { x := 7; fmt.Println(x*6, x/2, x%4, -x, 1<<10, 7.0/2) }`, "42 3 3 -7 1024 3.5\n"},
		{"loops", `package main
import "fmt"
func main() {
	s := 0
	for i := 0; i < 10; i++ { if i%2 == 0 { continue }; if i > 7 { break }; s += i }
	fmt.Println(s)
}`, "16\n"},
		{"bool-values", `package main
import "fmt"
func f(a, b bool) bool { return a && !b || b && !a }
func main() { fmt.Println(f(true, false), f(true, true), "x"+fmt.Sprint(f(false, true))) }`, "true false xtrue\n"},
		{"java-interop", `package main
import ("fmt"; "java/util")
func main() {
	l := util.NewArrayList()
	l.Add("b"); l.Add("a")
	util.Collections.Sort(l)
	fmt.Println(l, l.Size())
}`, "[a, b] 2\n"},
		{"structs", `package main
import "fmt"
type P struct{ X, Y int }
func (p P) Sum() int { return p.X + p.Y }
func (p *P) Inc() { p.X++ }
func main() {
	a := P{1, 2}
	b := a
	b.Inc()
	a.Inc()
	a.Inc()
	pp := &a
	pp.Inc()
	fmt.Println(a, b, b.Sum(), a.X)
}`, "{4 2} {2 2} 4 4\n"},
		{"strict-types", `package main
func main() { var a int32 = 1; var b int = 2; _ = a + b }`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, errs := compileRun(t, c.src)
			if c.want == "" {
				if len(errs) == 0 {
					t.Fatal("expected compile error")
				}
				return
			}
			if len(errs) > 0 {
				t.Fatal(errs)
			}
			if got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestJavaSupertypes(t *testing.T) {
	ok := `package main
import ("fmt"; "java/lang")
//gojvm:extends java/lang/Thread
//gojvm:implements java/lang/Runnable
type Worker struct{ N int }
func (w *Worker) Run() { w.N = 42 }
func main() {
	w := &Worker{}
	w.Start()
	w.Join()
	var r lang.Runnable = w
	_ = r
	fmt.Println(w.N)
}`
	got, errs := compileRun(t, ok)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if got != "42\n" {
		t.Fatalf("got %q", got)
	}

	bad := map[string]string{
		"does not implement abstract method": `package main
//gojvm:implements java/lang/Runnable
type W struct{}
func main() {}`,
		"signature differs": `package main
//gojvm:implements java/lang/Comparable
type W struct{}
func (w *W) CompareTo(o int32) int32 { return 0 }
func main() {}`,
		"not found": `package main
//gojvm:extends no/such/Class
type W struct{}
func main() {}`,
	}
	for want, src := range bad {
		_, errs := compileRun(t, src)
		if len(errs) == 0 || !strings.Contains(strings.Join(errs, "\n"), want) {
			t.Errorf("want error containing %q, got %v", want, errs)
		}
	}
}
