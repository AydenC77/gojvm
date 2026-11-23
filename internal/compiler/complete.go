package compiler

import (
	"go/ast"
	"sort"
	"strings"

	"github.com/AydenC77/gojvm/internal/jvm"
)

// CompleteMarker is the identifier the language server substitutes at the cursor.
const CompleteMarker = "__gojvm__"

// LSP completion item kinds used by the compiler.
const (
	KindMethod      = 2
	KindFunction    = 3
	KindConstructor = 4
	KindField       = 5
	KindVariable    = 6
	KindClass       = 7
	KindModule      = 9
	KindKeyword     = 14
	KindConstant    = 21
	KindStruct      = 22
)

// CompletionItem is one completion candidate.
type CompletionItem struct {
	Label  string
	Detail string
	Kind   int
	Doc    string
}

func isMarkerExpr(x ast.Expr) bool {
	switch x := unparen(x).(type) {
	case *ast.Ident:
		return x.Name == CompleteMarker
	case *ast.SelectorExpr:
		return x.Sel.Name == CompleteMarker
	}
	return false
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// goMemberName maps a Java member name to the name used from Go.
func goMemberName(javaName string, isField bool) string {
	if isField && javaName == strings.ToUpper(javaName) {
		return javaName // constants keep their SHOUTING_CASE
	}
	return upperFirst(javaName)
}

// GoSignature renders a method descriptor as a Go-style "(params) result".
func GoSignature(desc string) string {
	pds, ret := jvm.SplitDesc(desc)
	var ps []string
	for _, d := range pds {
		ps = append(ps, TypeFromDesc(d).String())
	}
	r := ""
	if ret != "V" {
		r = " " + TypeFromDesc(ret).String()
	}
	return "(" + strings.Join(ps, ", ") + ")" + r
}

func (c *Compiler) done(items []CompletionItem) {
	sort.SliceStable(items, func(i, j int) bool { return items[i].Label < items[j].Label })
	c.items = items
	panic(completionDone{})
}

// MethodItems builds completion items for Java methods, folding overloads.
func methodItems(ms []jvm.Member, static bool, kind int) []CompletionItem {
	type group struct {
		first string
		n     int
	}
	order := []string{}
	groups := map[string]*group{}
	for _, m := range ms {
		if (m.Access&jvm.AccStatic != 0) != static {
			continue
		}
		name := goMemberName(m.Name, false)
		g := groups[name]
		if g == nil {
			g = &group{first: GoSignature(m.Desc)}
			groups[name] = g
			order = append(order, name)
		}
		g.n++
	}
	var out []CompletionItem
	for _, name := range order {
		g := groups[name]
		d := g.first
		if g.n > 1 {
			d += "  (+" + itoa(g.n-1) + " overloads)"
		}
		out = append(out, CompletionItem{Label: name, Detail: d, Kind: kind})
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func fieldItems(fs []jvm.Member, static bool) []CompletionItem {
	var out []CompletionItem
	for _, f := range fs {
		if (f.Access&jvm.AccStatic != 0) != static {
			continue
		}
		k := KindField
		if f.Access&jvm.AccStatic != 0 && f.Access&jvm.AccFinal != 0 {
			k = KindConstant
		}
		out = append(out, CompletionItem{Label: goMemberName(f.Name, true), Detail: TypeFromDesc(f.Desc).String(), Kind: k})
	}
	return out
}

// ClassMembers lists the Go-visible members of a Java class: static=true gives
// static methods and fields, otherwise instance members.
func ClassMembers(cp *jvm.ClassPath, class string, static, protected bool) []CompletionItem {
	items := methodItems(cp.AllMethods(class, protected), static, KindMethod)
	items = append(items, fieldItems(cp.AllFields(class), static)...)
	return items
}

func (c *Compiler) fnSig(fi *funcInfo) string {
	var ps []string
	for i, p := range fi.params {
		ps = append(ps, fi.names[i]+" "+p.String())
	}
	r := ""
	if fi.ret.K != KVoid {
		r = " " + fi.ret.String()
	}
	return "(" + strings.Join(ps, ", ") + ")" + r
}

func (c *Compiler) packageItems(o *object) []CompletionItem {
	var out []CompletionItem
	switch o.pkg.intrinsic {
	case "fmt":
		for _, n := range []string{"Println", "Printf", "Print", "Sprintf", "Sprint", "Sprintln"} {
			d := "(a ...any)"
			if strings.HasSuffix(n, "f") {
				d = "(format string, a ...any)"
			}
			out = append(out, CompletionItem{Label: n, Detail: d, Kind: KindFunction})
		}
		return out
	case "os":
		return []CompletionItem{{Label: "Exit", Detail: "(code int)", Kind: KindFunction}}
	case "math":
		out = methodItems(c.cp.AllMethods("java/lang/Math", false), true, KindFunction)
		for n, cv := range mathConsts {
			out = append(out, CompletionItem{Label: n, Detail: cv.t.String(), Kind: KindConstant})
		}
		return out
	}
	for _, cls := range c.cp.ClassesIn(o.pkg.javaPkg) {
		short := cls[strings.LastIndex(cls, "/")+1:]
		out = append(out, CompletionItem{Label: short, Detail: "class " + strings.ReplaceAll(cls, "/", "."), Kind: KindClass})
		ci := c.cp.Lookup(cls)
		if ci != nil && !ci.IsInterface() && ci.Access&jvm.AccAbstract == 0 {
			out = append(out, CompletionItem{Label: "New" + short, Detail: "constructor → " + short, Kind: KindConstructor})
		}
	}
	return out
}

func (c *Compiler) completeSelector(e *ast.SelectorExpr) {
	x := unparen(e.X)
	if id, ok := x.(*ast.Ident); ok {
		if o := c.lookup(id.Name); o != nil && o.kind == oPkg {
			c.done(c.packageItems(o))
		}
	}
	if cls, ok := c.classRef(x); ok {
		c.done(ClassMembers(c.cp, cls, true, false))
	}
	t := c.genExpr(x, nil)
	switch t.K {
	case KString:
		c.done(ClassMembers(c.cp, "java/lang/String", false, false))
	case KClass:
		c.done(ClassMembers(c.cp, t.Class, false, false))
	case KStruct, KPtr:
		si := c.structOf(t)
		var items []CompletionItem
		for _, f := range si.fields {
			items = append(items, CompletionItem{Label: f.name, Detail: f.typ.String(), Kind: KindField})
		}
		for name, m := range si.methods {
			items = append(items, CompletionItem{Label: name, Detail: c.fnSig(m), Kind: KindMethod})
		}
		for _, r := range si.javaRoots() {
			for _, it := range ClassMembers(c.cp, r, false, true) {
				dup := false
				for _, have := range items {
					if have.Label == it.Label {
						dup = true
						break
					}
				}
				if !dup {
					items = append(items, it)
				}
			}
		}
		c.done(items)
	}
	c.done(nil)
}

var goKeywords = []string{"break", "case", "const", "continue", "default", "else", "fallthrough",
	"for", "func", "if", "import", "package", "range", "return", "struct", "switch", "type", "var"}

func (c *Compiler) completeScope() {
	var items []CompletionItem
	seen := map[string]bool{}
	for s := c.cur; s != nil; s = s.parent {
		for name, o := range s.names {
			if seen[name] || name == CompleteMarker {
				continue
			}
			seen[name] = true
			it := CompletionItem{Label: name}
			switch o.kind {
			case oLocal:
				it.Kind, it.Detail = KindVariable, o.typ.String()
			case oGlobal:
				it.Kind, it.Detail = KindVariable, o.typ.String()
			case oConst:
				it.Kind, it.Detail = KindConstant, o.cv.t.String()
			case oFunc:
				it.Kind, it.Detail = KindFunction, c.fnSig(o.fn)
			case oPkg:
				it.Kind, it.Detail = KindModule, o.pkg.path
			case oType:
				it.Kind, it.Detail = KindStruct, o.typ.String()
			case oBuiltin:
				it.Kind = KindFunction
			case oNil:
				it.Kind = KindConstant
			}
			items = append(items, it)
		}
	}
	for _, k := range goKeywords {
		items = append(items, CompletionItem{Label: k, Kind: KindKeyword})
	}
	c.done(items)
}
