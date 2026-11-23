package compiler

import (
	"strings"

	"go/ast"
	"go/token"
	"path/filepath"

	"github.com/AydenC77/gojvm/internal/jvm"
)

// Go structs compile to JVM classes. A struct value and a pointer to it are both
// references to the same class; value semantics are kept by copying (via the
// generated $copy method) whenever a value is stored, passed or returned.

type fieldInfo struct {
	name string
	typ  Type
}

type structInfo struct {
	name    string // Go name
	jname   string // JVM internal class name
	super   string // Java superclass (default java/lang/Object)
	ifaces  []string
	fields  []fieldInfo
	methods map[string]*funcInfo
	cls     *jvm.Class
	spec    *ast.StructType
}

func (si *structInfo) field(name string) (fieldInfo, bool) {
	for _, f := range si.fields {
		if f.name == name {
			return f, true
		}
	}
	return fieldInfo{}, false
}

func (c *Compiler) structOf(t Type) *structInfo {
	if t.K == KStruct || t.K == KPtr {
		return c.structs[t.Class]
	}
	return nil
}

// directives collects //gojvm:<key> <args...> lines from comment groups.
func directives(groups ...*ast.CommentGroup) map[string][]string {
	out := map[string][]string{}
	for _, g := range groups {
		if g == nil {
			continue
		}
		for _, cm := range g.List {
			line := strings.TrimSpace(strings.TrimPrefix(cm.Text, "//"))
			if !strings.HasPrefix(line, "gojvm:") {
				continue
			}
			f := strings.Fields(strings.TrimPrefix(line, "gojvm:"))
			if len(f) > 0 {
				out[f[0]] = append(out[f[0]], f[1:]...)
			}
		}
	}
	return out
}

func (c *Compiler) declareTypes(gd *ast.GenDecl) {
	for _, s := range gd.Specs {
		ts := s.(*ast.TypeSpec)
		if ts.Assign.IsValid() {
			c.fail(ts, "type aliases are not supported yet")
		}
		if ts.TypeParams != nil {
			c.fail(ts, "generic types are not supported yet")
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			c.fail(ts, "only struct types can be declared so far (type %s)", ts.Name.Name)
		}
		name := ts.Name.Name
		if _, dup := c.pkgScope.names[name]; dup || name == mainClass {
			c.fail(ts.Name, "%s redeclared in this block", name)
		}
		jn := c.prefix + name
		si := &structInfo{name: name, jname: jn, super: "java/lang/Object", methods: map[string]*funcInfo{}, spec: st}
		dirs := directives(gd.Doc, ts.Doc)
		for k, v := range dirs {
			switch k {
			case "extends":
				if len(v) != 1 {
					c.fail(ts, "//gojvm:extends takes exactly one class")
				}
				si.super = v[0]
			case "implements":
				si.ifaces = v
			default:
				c.fail(ts, "unknown directive //gojvm:%s", k)
			}
		}
		si.cls = jvm.NewClass(jn, si.super)
		si.cls.Interfaces = si.ifaces
		si.cls.SourceFile = c.cls.SourceFile
		c.structs[jn] = si
		c.structOrder = append(c.structOrder, si)
		c.pkgScope.names[name] = &object{kind: oType, name: name, typ: Type{K: KStruct, Class: jn}}
	}
}

func (c *Compiler) checkJavaSupertypes(si *structInfo) {
	at := si.spec
	sup := c.cp.Lookup(si.super)
	if sup == nil {
		c.fail(at, "//gojvm:extends: class %s not found (add its jar with -cp)", si.super)
	}
	if sup.IsInterface() {
		c.fail(at, "%s is an interface; use //gojvm:implements", si.super)
	}
	if sup.Access&jvm.AccFinal != 0 {
		c.fail(at, "cannot extend final class %s", si.super)
	}
	ok := false
	for _, m := range sup.Methods {
		if m.Name == "<init>" && m.Desc == "()V" && m.Access&(jvm.AccPublic|0x0004) != 0 {
			ok = true
		}
	}
	if !ok {
		c.fail(at, "%s has no accessible no-argument constructor", si.super)
	}
	for _, i := range si.ifaces {
		ci := c.cp.Lookup(i)
		if ci == nil {
			c.fail(at, "//gojvm:implements: interface %s not found (add its jar with -cp)", i)
		}
		if !ci.IsInterface() {
			c.fail(at, "%s is not an interface", i)
		}
	}
}

func (c *Compiler) resolveFields(si *structInfo) {
	c.checkJavaSupertypes(si)
	for _, f := range si.spec.Fields.List {
		if len(f.Names) == 0 {
			c.fail(f, "embedded fields are not supported yet")
		}
		t := c.resolveType(f.Type)
		if t.K == KStruct && t.Class == si.jname {
			c.fail(f, "invalid recursive type %s", si.jname)
		}
		for _, n := range f.Names {
			if _, dup := si.field(n.Name); dup {
				c.fail(n, "%s redeclared", n.Name)
			}
			si.fields = append(si.fields, fieldInfo{n.Name, t})
			si.cls.Fields = append(si.cls.Fields, jvm.Field{Access: jvm.AccPublic, Name: n.Name, Desc: t.Desc()})
		}
	}
}

// genStructClasses emits the constructor, $copy and toString of every struct class.
func (c *Compiler) genStructClasses() {
	for _, si := range c.structOrder {
		c.checkAbstract(si)
		c.genInit(si)
		c.genCopy(si)
		c.genToString(si)
	}
}

func (c *Compiler) genInit(si *structInfo) {
	vt := jvm.VObj(si.jname)
	code := jvm.NewCode(si.cls.CP, []jvm.VT{vt})
	code.Load(0, vt)
	code.Invoke(jvm.Invokespecial, si.super, "<init>", "()V", false)
	for _, f := range si.fields {
		switch f.typ.K {
		case KString:
			code.Load(0, vt)
			code.Sconst("")
			code.Field(jvm.Putfield, si.jname, f.name, f.typ.Desc())
		case KStruct:
			code.Load(0, vt)
			code.New(f.typ.Class)
			code.Dup()
			code.Invoke(jvm.Invokespecial, f.typ.Class, "<init>", "()V", false)
			code.Field(jvm.Putfield, si.jname, f.name, f.typ.Desc())
		}
	}
	code.Return(nil)
	si.cls.Methods = append(si.cls.Methods, &jvm.Method{Access: jvm.AccPublic, Name: "<init>", Desc: "()V", Code: code})
}

func (c *Compiler) genCopy(si *structInfo) {
	vt := jvm.VObj(si.jname)
	code := jvm.NewCode(si.cls.CP, []jvm.VT{vt})
	code.New(si.jname)
	code.Dup()
	code.Invoke(jvm.Invokespecial, si.jname, "<init>", "()V", false)
	for _, f := range si.fields {
		code.Dup()
		code.Load(0, vt)
		code.Field(jvm.Getfield, si.jname, f.name, f.typ.Desc())
		if f.typ.K == KStruct {
			code.Invoke(jvm.Invokevirtual, f.typ.Class, "$copy", "()L"+f.typ.Class+";", false)
		}
		code.Field(jvm.Putfield, si.jname, f.name, f.typ.Desc())
	}
	code.Return(&vt)
	si.cls.Methods = append(si.cls.Methods, &jvm.Method{Access: jvm.AccPublic, Name: "$copy", Desc: "()L" + si.jname + ";", Code: code})
}

// genToString makes fmt printing work: it uses the Go String() method when
// present, otherwise prints {f1 f2 ...} like Go's %v.
func (c *Compiler) genToString(si *structInfo) {
	vt := jvm.VObj(si.jname)
	code := jvm.NewCode(si.cls.CP, []jvm.VT{vt})
	retVT := jvm.VObj("java/lang/String")
	if m, ok := si.methods["String"]; ok && len(m.params) == 0 && m.ret.K == KString {
		code.Load(0, vt)
		code.Invoke(jvm.Invokevirtual, si.jname, m.jname, m.desc, false)
		code.Return(&retVT)
	} else {
		saveCode, saveCls := c.code, c.cls
		c.code, c.cls = code, si.cls
		code.New(sbClass)
		code.Dup()
		code.Invoke(jvm.Invokespecial, sbClass, "<init>", "()V", false)
		app := func() {
			code.Invoke(jvm.Invokevirtual, sbClass, "append", "("+strDesc+")L"+sbClass+";", false)
		}
		code.Sconst("{")
		app()
		for i, f := range si.fields {
			if i > 0 {
				code.Sconst(" ")
				app()
			}
			code.Load(0, vt)
			code.Field(jvm.Getfield, si.jname, f.name, f.typ.Desc())
			c.toStr(f.typ)
			app()
		}
		code.Sconst("}")
		app()
		code.Invoke(jvm.Invokevirtual, sbClass, "toString", "()"+strDesc, false)
		code.Return(&retVT)
		c.code, c.cls = saveCode, saveCls
	}
	si.cls.Methods = append(si.cls.Methods, &jvm.Method{Access: jvm.AccPublic, Name: "toString", Desc: "()" + strDesc, Code: code})
}

func (c *Compiler) structField(n ast.Node, xt Type, name string) fieldInfo {
	si := c.structOf(xt)
	if f, ok := si.field(name); ok {
		return f
	}
	if _, ok := si.methods[name]; ok {
		c.fail(n, "method value %s.%s must be called", xt, name)
	}
	c.fail(n, "%s undefined (type %s has no field or method %s)", exprStr(n), xt, name)
	return fieldInfo{}
}

// needsCopy reports whether a struct-valued expression denotes existing storage
// (and so must be copied to preserve Go's value semantics).
func (c *Compiler) needsCopy(e ast.Expr) bool {
	switch unparen(e).(type) {
	case *ast.CompositeLit, *ast.CallExpr:
		return false
	}
	return true
}

func (c *Compiler) copyStruct(t Type) {
	c.code.Invoke(jvm.Invokevirtual, t.Class, "$copy", "()L"+t.Class+";", false)
}

func (c *Compiler) genCompositeLit(e *ast.CompositeLit) Type {
	if e.Type == nil {
		c.fail(e, "composite literal without a type is not supported yet")
	}
	t := c.resolveType(e.Type)
	if t.K != KStruct {
		c.fail(e, "composite literals of type %s are not supported yet", t)
	}
	si := c.structOf(t)
	c.code.New(si.jname)
	c.code.Dup()
	c.code.Invoke(jvm.Invokespecial, si.jname, "<init>", "()V", false)
	if len(e.Elts) == 0 {
		return t
	}
	_, keyed := e.Elts[0].(*ast.KeyValueExpr)
	if !keyed && len(e.Elts) != len(si.fields) {
		c.fail(e, "too few values in struct literal of type %s", si.jname)
	}
	seen := map[string]bool{}
	for i, el := range e.Elts {
		var f fieldInfo
		var val ast.Expr
		if keyed {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				c.fail(el, "mixture of field:value and value elements in struct literal")
			}
			id, ok := kv.Key.(*ast.Ident)
			if !ok {
				c.fail(kv.Key, "invalid field name %s in struct literal", exprStr(kv.Key))
			}
			var found bool
			if f, found = si.field(id.Name); !found {
				c.fail(id, "unknown field %s in struct literal of type %s", id.Name, si.jname)
			}
			if seen[id.Name] {
				c.fail(id, "duplicate field name %s in struct literal", id.Name)
			}
			seen[id.Name] = true
			val = kv.Value
		} else {
			if _, bad := el.(*ast.KeyValueExpr); bad {
				c.fail(el, "mixture of field:value and value elements in struct literal")
			}
			f, val = si.fields[i], el
		}
		c.code.Dup()
		c.exprTo(val, f.typ)
		c.code.Field(jvm.Putfield, si.jname, f.name, f.typ.Desc())
	}
	return t
}

// genAddr handles &x for struct values: the pointer aliases the same object.
func (c *Compiler) genAddr(e *ast.UnaryExpr) Type {
	x := unparen(e.X)
	var t Type
	if cl, ok := x.(*ast.CompositeLit); ok {
		t = c.genCompositeLit(cl)
	} else {
		t = c.genExpr(x, nil)
	}
	if t.K != KStruct {
		c.fail(e, "cannot take the address of %s (only struct values are supported)", exprStr(e.X))
	}
	return Type{K: KPtr, Class: t.Class}
}

func (c *Compiler) userMethodCall(e *ast.CallExpr, recv Type, name string) Type {
	si := c.structOf(recv)
	m, ok := si.methods[name]
	if !ok {
		if _, isField := si.field(name); isField {
			c.fail(e.Fun, "cannot call non-function field %s", exprStr(e.Fun))
		}
		cands := c.javaMethods(si, name)
		if len(cands) == 0 {
			c.fail(e.Fun, "%s undefined (type %s has no field or method %s)", exprStr(e.Fun), recv, name)
		}
		ch := c.pickMethod(e, exprStr(e.Fun), cands, e.Args)
		c.emitArgs(ch, e.Args)
		_, ret := jvm.SplitDesc(ch.m.Desc)
		c.code.Invoke(jvm.Invokevirtual, si.jname, ch.m.Name, ch.m.Desc, false)
		return TypeFromDesc(ret)
	}
	if len(e.Args) != len(m.params) {
		c.fail(e, "wrong number of arguments in call to %s (have %d, want %d)", exprStr(e.Fun), len(e.Args), len(m.params))
	}
	for i, a := range e.Args {
		c.exprTo(a, m.params[i])
	}
	c.code.Invoke(jvm.Invokevirtual, si.jname, m.jname, m.desc, false)
	return m.ret
}

// javaRoots lists the Java supertypes of a struct.
func (si *structInfo) javaRoots() []string {
	return append([]string{si.super}, si.ifaces...)
}

// javaMethods finds inherited Java methods (public or protected) callable on si.
func (c *Compiler) javaMethods(si *structInfo, goName string) []jvm.Member {
	for _, name := range []string{lowerFirst(goName), goName} {
		var out []jvm.Member
		seen := map[string]bool{}
		for _, r := range si.javaRoots() {
			for _, m := range c.cp.HierarchyMethods(r, name, true) {
				if m.Access&jvm.AccStatic == 0 && !seen[m.Desc] {
					seen[m.Desc] = true
					out = append(out, m)
				}
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// checkAbstract reports Java abstract methods a struct forgot to implement.
func (c *Compiler) checkAbstract(si *structInfo) {
	have := map[string]bool{}
	for _, m := range si.methods {
		have[m.jname+m.desc] = true
	}
	for _, m := range c.cp.AbstractMethods(si.javaRoots()...) {
		if !have[m.Name+m.Desc] {
			c.fail(si.spec, "type %s does not implement abstract method %s", si.name, prettyDesc(m))
		}
	}
}

// resolveJavaName decides the JVM name of a Go method: it overrides an inherited
// Java method when the Go name matches (Run -> run) and the signature agrees.
func (c *Compiler) resolveJavaName(n ast.Node, si *structInfo, fi *funcInfo) {
	fi.jname = fi.name
	for _, name := range []string{lowerFirst(fi.name), fi.name} {
		var sameArity []jvm.Member
		for _, r := range si.javaRoots() {
			for _, m := range c.cp.HierarchyMethods(r, name, true) {
				if m.Access&(jvm.AccStatic|jvm.AccFinal) != 0 {
					continue
				}
				pds, _ := jvm.SplitDesc(m.Desc)
				if len(pds) == len(fi.params) {
					sameArity = append(sameArity, m)
				}
				if m.Desc == fi.desc {
					fi.jname = m.Name
					return
				}
			}
		}
		if len(sameArity) > 0 && name != fi.name {
			var sigs []string
			for _, m := range sameArity {
				sigs = append(sigs, prettyDesc(m))
			}
			c.fail(n, "method %s looks like an override of %s but the signature differs; "+
				"use exactly those Go types, or rename the method", fi.name, strings.Join(sigs, " or "))
		}
	}
}

func (c *Compiler) declareMethod(fd *ast.FuncDecl, fi *funcInfo) {
	if len(fd.Recv.List) != 1 {
		c.fail(fd, "method must have exactly one receiver")
	}
	rf := fd.Recv.List[0]
	rt := rf.Type
	if st, ok := rt.(*ast.StarExpr); ok {
		fi.recvPtr = true
		rt = st.X
	}
	id, ok := rt.(*ast.Ident)
	if !ok {
		c.fail(rf, "invalid receiver type")
	}
	var si *structInfo
	if o := c.pkgScope.names[id.Name]; o != nil && o.kind == oType {
		si = c.structs[o.typ.Class]
	}
	if si == nil {
		c.fail(id, "undefined receiver type: %s (methods can only be declared on struct types)", id.Name)
	}
	fi.recv = si
	fi.recvName = "_"
	if len(rf.Names) > 0 {
		fi.recvName = rf.Names[0].Name
	}
	if _, dup := si.methods[fi.name]; dup {
		c.fail(fd.Name, "method %s.%s already declared", si.jname, fi.name)
	}
	if _, dup := si.field(fi.name); dup {
		c.fail(fd.Name, "field and method with the same name %s", fi.name)
	}
	c.resolveJavaName(fd.Name, si, fi)
	si.methods[fi.name] = fi
}

func (c *Compiler) setClassSource(files []*ast.File) {
	if len(files) > 0 {
		c.cls.SourceFile = filepath.Base(c.fset.Position(files[0].Pos()).Filename)
	}
}

var _ = token.ADD
