// Package compiler translates a Go package (parsed with go/parser) directly
// into JVM bytecode (Java 21 class files).
package compiler

import (
	"fmt"
	"go/ast"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/AydenC77/gojvm/internal/jvm"
)

const mainClass = "Main"

type objKind int

const (
	oLocal objKind = iota + 1
	oGlobal
	oConst
	oFunc
	oPkg
	oType
	oBuiltin
	oNil
)

type pkgInfo struct {
	path      string
	javaPkg   string // internal package name, e.g. java/util
	intrinsic string // "fmt", "math", "os"
}

type funcInfo struct {
	name   string
	names  []string
	params []Type
	ret    Type
	decl   *ast.FuncDecl
	desc   string

	recv     *structInfo // non-nil for methods
	recvPtr  bool
	recvName string
	jname    string // JVM method name (differs from name when overriding a Java method)
}

type object struct {
	kind objKind
	name string
	typ  Type
	slot int
	cv   *constVal
	fn   *funcInfo
	pkg  *pkgInfo
}

type scope struct {
	parent *scope
	names  map[string]*object
}

func newScope(parent *scope) *scope { return &scope{parent: parent, names: map[string]*object{}} }

type loopCtx struct {
	label string
	brk   *jvm.Label
	cont  *jvm.Label // nil for switch
}

// Diag is a compiler diagnostic with a source position.
type Diag struct {
	File      string
	Line, Col int
	Msg       string
}

func (d Diag) Error() string {
	if d.File == "" {
		return d.Msg
	}
	return fmt.Sprintf("%s:%d:%d: %s", filepath.Base(d.File), d.Line, d.Col, d.Msg)
}

type compileError struct{ d Diag }

// completionDone is panicked once the completion marker has been resolved.
type completionDone struct{}

// Output is the full result of CompileWith.
type Output struct {
	Result    *Result
	Diags     []Diag
	Items     []CompletionItem // set in completion mode
	Completed bool
}

// Options configure CompileWith.
type Options struct {
	// Complete makes the compiler stop at the completion marker identifier and
	// return the items valid at that position.
	Complete bool
}

// Result is the output of a successful compilation.
type Result struct {
	MainClass string
	Classes   map[string][]byte
}

type Compiler struct {
	fset        *token.FileSet
	cp          *jvm.ClassPath
	cls         *jvm.Class // class currently being generated
	mainCls     *jvm.Class
	structs     map[string]*structInfo
	structOrder []*structInfo
	lib         bool   // non-main package: build a library jar
	prefix      string // JVM package prefix ("" or "pkg/")
	mainName    string // internal name of the class holding package-level funcs
	errs        []Diag
	opts        Options
	items       []CompletionItem
	completed   bool
	universe    *scope
	pkgScope    *scope
	cur         *scope
	code        *jvm.Code
	clinit      *jvm.Code
	hasClinit   bool
	fn          *funcInfo
	iota        int
	tcache      map[ast.Expr]Type
	loops       []*loopCtx
	label       string
	trimHelp    bool
	funcs       []*funcInfo
}

func (c *Compiler) fail(n ast.Node, format string, args ...any) {
	d := Diag{Msg: fmt.Sprintf(format, args...)}
	if n != nil && n.Pos().IsValid() {
		p := c.fset.Position(n.Pos())
		d.File, d.Line, d.Col = p.Filename, p.Line, p.Column
	}
	panic(compileError{d})
}

func (c *Compiler) try(f func()) {
	defer func() {
		if r := recover(); r != nil {
			if ce, ok := r.(compileError); ok {
				c.errs = append(c.errs, ce.d)
				return
			}
			if _, ok := r.(completionDone); ok {
				c.completed = true
				return
			}
			panic(r)
		}
	}()
	f()
}

func (c *Compiler) lookup(name string) *object {
	for s := c.cur; s != nil; s = s.parent {
		if o, ok := s.names[name]; ok {
			return o
		}
	}
	return nil
}

func (c *Compiler) define(name string, o *object) {
	if name == "_" {
		return
	}
	o.name = name
	c.cur.names[name] = o
}

func (c *Compiler) openScope() int {
	c.cur = newScope(c.cur)
	return c.code.NumLocals()
}

func (c *Compiler) closeScope(mark int) {
	c.cur = c.cur.parent
	c.code.Truncate(mark)
}

func (c *Compiler) setupUniverse() {
	u := newScope(nil)
	for name, t := range map[string]Type{
		"bool": TBool, "int8": TInt8, "int16": TInt16, "int32": TInt32, "rune": TInt32, "int": TInt,
		"int64": TInt64, "float32": TFloat32, "float64": TFloat64, "string": TString, "any": TObject,
	} {
		u.names[name] = &object{kind: oType, name: name, typ: t}
	}
	u.names["true"] = &object{kind: oConst, cv: &constVal{trueVal, TUBool}}
	u.names["false"] = &object{kind: oConst, cv: &constVal{falseVal, TUBool}}
	u.names["nil"] = &object{kind: oNil}
	u.names["iota"] = &object{kind: oBuiltin, name: "iota"}
	for _, b := range []string{"len", "print", "println", "panic", "min", "max", "new"} {
		u.names[b] = &object{kind: oBuiltin, name: b}
	}
	c.universe = u
}

// Compile compiles the given files (one package) to JVM classes.
func Compile(fset *token.FileSet, files []*ast.File, cp *jvm.ClassPath) (*Result, []Diag) {
	out := CompileWith(fset, files, cp, Options{})
	return out.Result, out.Diags
}

// CompileWith compiles with options (see Options).
func CompileWith(fset *token.FileSet, files []*ast.File, cp *jvm.ClassPath, opts Options) *Output {
	res, errs, c := compile(fset, files, cp, opts)
	return &Output{Result: res, Diags: errs, Items: c.items, Completed: c.completed}
}

func compile(fset *token.FileSet, files []*ast.File, cp *jvm.ClassPath, opts Options) (*Result, []Diag, *Compiler) {
	c := &Compiler{opts: opts, fset: fset, cp: cp, iota: -1, tcache: map[ast.Expr]Type{}, structs: map[string]*structInfo{}}
	c.setupUniverse()
	c.pkgScope = newScope(c.universe)
	c.cur = c.pkgScope
	pkg := "main"
	if len(files) > 0 {
		pkg = files[0].Name.Name
	}
	c.lib = pkg != "main"
	if c.lib {
		c.prefix = pkg + "/"
	}
	c.mainName = c.prefix + mainClass
	c.cls = jvm.NewClass(c.mainName, "java/lang/Object")
	c.mainCls = c.cls
	c.clinit = jvm.NewCode(c.cls.CP, nil)
	c.setClassSource(files)
	for _, f := range files {
		if f.Name.Name != pkg {
			p := fset.Position(f.Name.Pos())
			c.errs = append(c.errs, Diag{File: p.Filename, Line: p.Line, Col: p.Column,
				Msg: fmt.Sprintf("found packages %s and %s in one build", pkg, f.Name.Name)})
			return nil, c.errs, c
		}
	}
	// pass 0: imports
	for _, f := range files {
		for _, imp := range f.Imports {
			c.try(func() { c.importSpec(imp) })
		}
	}
	// pass T: struct types (names first so they can refer to each other)
	for _, f := range files {
		for _, d := range f.Decls {
			if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.TYPE {
				c.try(func() { c.declareTypes(gd) })
			}
		}
	}
	for _, si := range c.structOrder {
		si := si
		c.try(func() { c.resolveFields(si) })
	}
	// pass 1: function signatures
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				c.try(func() { c.declareFunc(fd) })
			}
		}
	}
	// pass 2: constants, globals
	for _, f := range files {
		for _, d := range f.Decls {
			if gd, ok := d.(*ast.GenDecl); ok && gd.Tok != token.IMPORT && gd.Tok != token.TYPE {
				c.try(func() { c.globalDecl(gd) })
			}
		}
	}
	// pass 3: function bodies
	for _, fi := range c.funcs {
		fi := fi
		if c.completed {
			break
		}
		c.try(func() { c.compileFunc(fi) })
	}
	if len(c.errs) == 0 && !c.lib && !c.completed {
		if o, ok := c.pkgScope.names["main"]; !ok || o.kind != oFunc {
			c.errs = append(c.errs, Diag{Msg: "function main is undeclared in the main package"})
		}
	}
	if len(c.errs) > 0 || c.completed {
		return nil, c.errs, c
	}
	// struct classes, entry point wrapper, clinit
	c.try(c.genStructClasses)
	if len(c.errs) > 0 {
		return nil, c.errs, c
	}
	main := c.mainCls
	if !c.lib {
		entry := jvm.NewCode(main.CP, []jvm.VT{jvm.VObj("[Ljava/lang/String;")})
		entry.Invoke(jvm.Invokestatic, c.mainName, "main", "()V", false)
		entry.Return(nil)
		main.Methods = append(main.Methods, &jvm.Method{Access: 0x0009, Name: "main", Desc: "([Ljava/lang/String;)V", Code: entry})
	}
	if c.hasClinit {
		c.clinit.Return(nil)
		main.Methods = append(main.Methods, &jvm.Method{Access: 0x0008, Name: "<clinit>", Desc: "()V", Code: c.clinit})
	}
	res := &Result{Classes: map[string][]byte{}}
	if !c.lib {
		res.MainClass = c.mainName
	}
	b, err := main.Bytes()
	if err != nil {
		return nil, []Diag{{Msg: err.Error()}}, c
	}
	res.Classes[c.mainName] = b
	for _, si := range c.structOrder {
		b, err := si.cls.Bytes()
		if err != nil {
			return nil, []Diag{{Msg: err.Error()}}, c
		}
		res.Classes[si.jname] = b
	}
	return res, nil, c
}

func (c *Compiler) importSpec(imp *ast.ImportSpec) {
	path, _ := strconv.Unquote(imp.Path.Value)
	name := path[strings.LastIndex(path, "/")+1:]
	if imp.Name != nil {
		name = imp.Name.Name
	}
	pi := &pkgInfo{path: path}
	switch path {
	case "fmt", "math", "os":
		pi.intrinsic = path
	default:
		if !c.cp.HasPackage(path) {
			c.fail(imp, "package %s is not in the JDK (Java packages are imported like \"java/util\")", path)
		}
		pi.javaPkg = path
	}
	if name == "_" {
		return
	}
	c.pkgScope.names[name] = &object{kind: oPkg, name: name, pkg: pi}
}

func (c *Compiler) resolveType(e ast.Expr) Type {
	switch e := e.(type) {
	case *ast.ParenExpr:
		return c.resolveType(e.X)
	case *ast.StarExpr:
		t := c.resolveType(e.X)
		if t.K == KStruct {
			return Type{K: KPtr, Class: t.Class}
		}
		return t
	case *ast.Ident:
		o := c.lookup(e.Name)
		if o == nil {
			switch e.Name {
			case "byte", "uint8", "uint", "uint16", "uint32", "uint64", "uintptr":
				c.fail(e, "unsigned type %s is not supported yet", e.Name)
			case "error":
				c.fail(e, "type error is not supported yet")
			}
			c.fail(e, "undefined: %s", e.Name)
		}
		if o.kind != oType {
			c.fail(e, "%s is not a type", e.Name)
		}
		return o.typ
	case *ast.SelectorExpr:
		if name, ok := c.classRef(e); ok {
			return ClassType(name)
		}
		c.fail(e, "%s is not a type", exprStr(e))
	case *ast.ArrayType:
		if e.Len != nil {
			c.fail(e, "fixed-size arrays are not supported yet; use []T for Java arrays")
		}
		return ArrayOf(c.resolveType(e.Elt))
	case *ast.InterfaceType:
		if e.Methods == nil || len(e.Methods.List) == 0 {
			return TObject
		}
		c.fail(e, "interface types are not supported yet")
	}
	c.fail(e, "unsupported type expression")
	return Type{}
}

// isTypeExpr reports whether e denotes a type.
func (c *Compiler) isTypeExpr(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.ParenExpr:
		return c.isTypeExpr(e.X)
	case *ast.StarExpr:
		return c.isTypeExpr(e.X)
	case *ast.Ident:
		o := c.lookup(e.Name)
		return o != nil && o.kind == oType
	case *ast.SelectorExpr:
		_, ok := c.classRef(e)
		return ok
	case *ast.ArrayType, *ast.InterfaceType:
		return true
	}
	return false
}

func (c *Compiler) declareFunc(fd *ast.FuncDecl) {
	if fd.Type.TypeParams != nil {
		c.fail(fd, "generics are not supported yet")
	}
	if fd.Body == nil {
		c.fail(fd, "function %s has no body", fd.Name.Name)
	}
	fi := &funcInfo{name: fd.Name.Name, decl: fd, ret: TVoid}
	for _, f := range fd.Type.Params.List {
		if _, ok := f.Type.(*ast.Ellipsis); ok {
			c.fail(f, "variadic functions are not supported yet")
		}
		t := c.resolveType(f.Type)
		if len(f.Names) == 0 {
			fi.names = append(fi.names, "_")
			fi.params = append(fi.params, t)
		}
		for _, n := range f.Names {
			fi.names = append(fi.names, n.Name)
			fi.params = append(fi.params, t)
		}
	}
	if fd.Type.Results != nil {
		n := 0
		for _, f := range fd.Type.Results.List {
			if len(f.Names) > 0 {
				c.fail(f, "named results are not supported yet")
			}
			n++
			fi.ret = c.resolveType(f.Type)
		}
		if n > 1 {
			c.fail(fd, "multiple return values are not supported yet")
		}
	}
	if fd.Recv == nil && (fi.name == "main" || fi.name == "init") {
		if len(fi.params) > 0 || fi.ret.K != KVoid {
			c.fail(fd, "func %s must have no arguments and no return values", fi.name)
		}
	}
	if fd.Recv == nil && fi.name == "init" {
		c.fail(fd, "init functions are not supported yet")
	}
	var sb strings.Builder
	sb.WriteByte('(')
	for _, p := range fi.params {
		sb.WriteString(p.Desc())
	}
	sb.WriteByte(')')
	sb.WriteString(fi.ret.Desc())
	fi.desc = sb.String()
	if fd.Recv != nil {
		c.declareMethod(fd, fi)
		c.funcs = append(c.funcs, fi)
		return
	}
	if prev, ok := c.pkgScope.names[fi.name]; ok && prev.kind != oPkg {
		c.fail(fd.Name, "%s redeclared in this block", fi.name)
	}
	c.pkgScope.names[fi.name] = &object{kind: oFunc, name: fi.name, fn: fi}
	c.funcs = append(c.funcs, fi)
}

func (c *Compiler) compileFunc(fi *funcInfo) {
	var vts []jvm.VT
	cls := c.mainCls
	slot := 0
	if fi.recv != nil {
		cls = fi.recv.cls
		vts = append(vts, jvm.VObj(fi.recv.jname))
		slot = 1
	}
	for _, p := range fi.params {
		vts = append(vts, p.VT())
	}
	code := jvm.NewCode(cls.CP, vts)
	c.cls = cls
	c.code, c.fn, c.loops = code, fi, nil
	c.cur = newScope(c.pkgScope)
	if fi.recv != nil && fi.recvName != "_" {
		if fi.recvPtr {
			c.define(fi.recvName, &object{kind: oLocal, typ: Type{K: KPtr, Class: fi.recv.jname}, slot: 0})
		} else {
			// value receiver: work on a copy so the caller's value is untouched
			t := Type{K: KStruct, Class: fi.recv.jname}
			code.Load(0, t.VT())
			c.copyStruct(t)
			s := code.NewLocal(t.VT())
			code.Store(s, t.VT())
			c.define(fi.recvName, &object{kind: oLocal, typ: t, slot: s})
		}
	}
	for i, n := range fi.names {
		c.define(n, &object{kind: oLocal, typ: fi.params[i], slot: slot})
		slot += fi.params[i].VT().Size()
	}
	c.code.Line(c.fset.Position(fi.decl.Pos()).Line)
	c.stmts(fi.decl.Body.List)
	if !code.Dead() {
		if fi.ret.K != KVoid {
			c.fail(&ast.Ident{NamePos: fi.decl.Body.Rbrace}, "missing return")
		}
		code.Line(c.fset.Position(fi.decl.Body.Rbrace).Line)
		code.Return(nil)
	}
	c.cur = c.pkgScope
	access := uint16(0x0009)
	if fi.recv != nil {
		access = jvm.AccPublic
	}
	cls.Methods = append(cls.Methods, &jvm.Method{Access: access, Name: methodName(fi), Desc: fi.desc, Code: code})
}

// ---------------------------------------------------------------------------
// declarations

func (c *Compiler) globalDecl(gd *ast.GenDecl) {
	switch gd.Tok {
	case token.CONST:
		c.constDecl(gd, func(n *ast.Ident, cv *constVal) {
			if _, dup := c.pkgScope.names[n.Name]; dup && n.Name != "_" {
				c.fail(n, "%s redeclared in this block", n.Name)
			}
			c.pkgScope.names[n.Name] = &object{kind: oConst, name: n.Name, cv: cv}
		})
	case token.VAR:
		c.code = c.clinit
		c.cur = c.pkgScope
		c.fn = nil
		for _, s := range gd.Specs {
			vs := s.(*ast.ValueSpec)
			c.hasClinit = true
			c.varSpec(vs, true)
		}
	case token.TYPE:
		c.fail(gd, "type declarations are not supported yet")
	}
}

func (c *Compiler) constDecl(gd *ast.GenDecl, def func(*ast.Ident, *constVal)) {
	var lastType ast.Expr
	var lastVals []ast.Expr
	for idx, s := range gd.Specs {
		vs := s.(*ast.ValueSpec)
		c.iota = idx
		typ, vals := vs.Type, vs.Values
		if len(vals) == 0 {
			typ, vals = lastType, lastVals
		} else {
			lastType, lastVals = typ, vals
		}
		if len(vals) < len(vs.Names) {
			c.iota = -1
			c.fail(vs, "missing init expr for const declaration")
		}
		for i, n := range vs.Names {
			cv := c.constOf(vals[i])
			if cv == nil {
				c.iota = -1
				c.fail(vals[i], "%s is not constant", exprStr(vals[i]))
			}
			if typ != nil {
				t := c.resolveType(typ)
				cv = &constVal{c.materialize(vals[i], cv, t), t}
			}
			def(n, cv)
		}
	}
	c.iota = -1
}

func (c *Compiler) varSpec(vs *ast.ValueSpec, global bool) {
	var declT *Type
	if vs.Type != nil {
		t := c.resolveType(vs.Type)
		declT = &t
	}
	if len(vs.Values) != 0 && len(vs.Values) != len(vs.Names) {
		c.fail(vs, "assignment mismatch: %d variables but %d values", len(vs.Names), len(vs.Values))
	}
	if len(vs.Values) == 0 && declT == nil {
		c.fail(vs, "missing type or init expr")
	}
	type pending struct {
		name *ast.Ident
		t    Type
	}
	var pend []pending
	for i, n := range vs.Names {
		var t Type
		if declT != nil {
			t = *declT
			if len(vs.Values) > 0 {
				c.exprTo(vs.Values[i], t)
			} else {
				c.zero(t)
			}
		} else {
			t = c.genExpr(vs.Values[i], nil)
			c.checkValue(vs.Values[i], t)
			if t.K == KStruct && c.needsCopy(vs.Values[i]) {
				c.copyStruct(t)
			}
		}
		if global {
			if _, dup := c.pkgScope.names[n.Name]; dup && n.Name != "_" {
				c.fail(n, "%s redeclared in this block", n.Name)
			}
			if n.Name == "_" {
				c.code.Pop()
				continue
			}
			c.mainCls.Fields = append(c.mainCls.Fields, jvm.Field{Access: 0x0009, Name: n.Name, Desc: t.Desc()})
			c.code.Field(jvm.Putstatic, c.mainName, n.Name, t.Desc())
			c.pkgScope.names[n.Name] = &object{kind: oGlobal, name: n.Name, typ: t}
			continue
		}
		pend = append(pend, pending{n, t})
		if i < len(vs.Names)-1 {
			// keep values on the stack; store afterwards in reverse order
			continue
		}
	}
	for i := len(pend) - 1; i >= 0; i-- {
		p := pend[i]
		if p.name.Name == "_" {
			c.code.Pop()
			continue
		}
		slot := c.code.NewLocal(p.t.VT())
		c.code.Store(slot, p.t.VT())
		c.define(p.name.Name, &object{kind: oLocal, typ: p.t, slot: slot})
	}
}

func (c *Compiler) checkValue(e ast.Expr, t Type) {
	switch t.K {
	case KVoid:
		c.fail(e, "%s (no value) used as value", exprStr(e))
	case KNil:
		c.fail(e, "use of untyped nil in variable declaration")
	}
}

func (c *Compiler) zero(t Type) {
	switch {
	case t.K == KString:
		c.code.Sconst("")
	case t.K == KStruct:
		c.code.New(t.Class)
		c.code.Dup()
		c.code.Invoke(jvm.Invokespecial, t.Class, "<init>", "()V", false)
	case t.K == KInt || t.K == KInt64:
		c.code.Lconst(0)
	case t.K == KFloat32:
		c.code.Fconst(0)
	case t.K == KFloat64:
		c.code.Dconst(0)
	case t.IsPrim():
		c.code.Iconst(0)
	default:
		c.code.Null()
	}
}

// ---------------------------------------------------------------------------
// statements

func (c *Compiler) stmts(list []ast.Stmt) {
	for _, s := range list {
		c.safeStmt(s)
	}
}

// silentError aborts a statement that uses a variable whose declaration already
// failed, so one mistake does not cascade into many.
type silentError struct{}

// safeStmt compiles one statement; on an error it records it, restores the
// scope state and carries on with the next statement.
func (c *Compiler) safeStmt(s ast.Stmt) {
	savedCur, savedLoops, mark := c.cur, len(c.loops), c.code.NumLocals()
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		switch e := r.(type) {
		case compileError:
			c.errs = append(c.errs, e.d)
		case silentError:
		default:
			panic(r)
		}
		c.cur, c.loops = savedCur, c.loops[:savedLoops]
		c.code.Truncate(mark)
		c.code.ResetStack()
		c.label = ""
		c.poisonDecls(s)
	}()
	c.stmt(s)
}

// poisonDecls defines the variables a failed statement would have declared with
// an invalid type, so later uses stay quiet.
func (c *Compiler) poisonDecls(s ast.Stmt) {
	declare := func(name string) {
		if name == "_" {
			return
		}
		if _, ok := c.cur.names[name]; !ok {
			c.cur.names[name] = &object{kind: oLocal, name: name}
		}
	}
	switch s := s.(type) {
	case *ast.AssignStmt:
		if s.Tok == token.DEFINE {
			for _, l := range s.Lhs {
				if id, ok := l.(*ast.Ident); ok {
					declare(id.Name)
				}
			}
		}
	case *ast.DeclStmt:
		if gd, ok := s.Decl.(*ast.GenDecl); ok && gd.Tok == token.VAR {
			for _, sp := range gd.Specs {
				for _, n := range sp.(*ast.ValueSpec).Names {
					declare(n.Name)
				}
			}
		}
	}
}

func (c *Compiler) block(list []ast.Stmt) {
	mark := c.openScope()
	c.stmts(list)
	c.closeScope(mark)
}

func (c *Compiler) stmt(s ast.Stmt) {
	c.code.Line(c.fset.Position(s.Pos()).Line)
	switch s := s.(type) {
	case *ast.EmptyStmt:
	case *ast.ExprStmt:
		x := unparen(s.X)
		if _, ok := x.(*ast.CallExpr); !ok {
			if c.opts.Complete && isMarkerExpr(x) {
				c.genExpr(x, nil)
			}
			c.fail(s, "%s (value) is not used", exprStr(s.X))
		}
		t := c.genExpr(x, nil)
		if t.K != KVoid {
			c.code.Pop()
		}
	case *ast.AssignStmt:
		c.assignStmt(s)
	case *ast.IncDecStmt:
		lv := c.lvalueOf(s.X)
		if !lv.typ.IsNumeric() {
			c.fail(s, "invalid operation: %s%s (non-numeric type %s)", exprStr(s.X), s.Tok, lv.typ)
		}
		c.lvLoad(lv)
		c.pushOne(lv.typ)
		op := token.ADD
		if s.Tok == token.DEC {
			op = token.SUB
		}
		c.arithOp(s, op, lv.typ)
		c.lvStore(lv)
	case *ast.DeclStmt:
		gd := s.Decl.(*ast.GenDecl)
		switch gd.Tok {
		case token.VAR:
			for _, sp := range gd.Specs {
				c.varSpec(sp.(*ast.ValueSpec), false)
			}
		case token.CONST:
			c.constDecl(gd, func(n *ast.Ident, cv *constVal) {
				c.define(n.Name, &object{kind: oConst, cv: cv})
			})
		default:
			c.fail(s, "local type declarations are not supported yet")
		}
	case *ast.BlockStmt:
		c.block(s.List)
	case *ast.IfStmt:
		c.ifStmt(s)
	case *ast.ForStmt:
		c.forStmt(s)
	case *ast.RangeStmt:
		c.rangeStmt(s)
	case *ast.SwitchStmt:
		c.switchStmt(s)
	case *ast.ReturnStmt:
		c.returnStmt(s)
	case *ast.BranchStmt:
		c.branchStmt(s)
	case *ast.LabeledStmt:
		c.label = s.Label.Name
		c.stmt(s.Stmt)
		c.label = ""
	case *ast.GoStmt:
		c.fail(s, "goroutines are not supported yet")
	case *ast.DeferStmt:
		c.fail(s, "defer is not supported yet")
	default:
		c.fail(s, "unsupported statement")
	}
}

func (c *Compiler) takeLabel() string {
	l := c.label
	c.label = ""
	return l
}

func (c *Compiler) ifStmt(s *ast.IfStmt) {
	mark := c.openScope()
	if s.Init != nil {
		c.stmt(s.Init)
	}
	elseL, end := c.code.NewLabel(), c.code.NewLabel()
	c.genCond(s.Cond, elseL, false)
	c.block(s.Body.List)
	if s.Else != nil {
		c.code.Jump(jvm.Goto, end)
		c.code.Place(elseL, false)
		c.stmt(s.Else)
		c.closeScope(mark)
		c.code.Place(end, false)
	} else {
		c.closeScope(mark)
		c.code.Place(elseL, false)
	}
}

func (c *Compiler) forStmt(s *ast.ForStmt) {
	label := c.takeLabel()
	mark := c.openScope()
	if s.Init != nil {
		c.stmt(s.Init)
	}
	head, cont, end := c.code.NewLabel(), c.code.NewLabel(), c.code.NewLabel()
	c.code.Place(head, true)
	if s.Cond != nil {
		c.genCond(s.Cond, end, false)
	}
	c.loops = append(c.loops, &loopCtx{label, end, cont})
	c.block(s.Body.List)
	c.loops = c.loops[:len(c.loops)-1]
	c.code.Place(cont, false)
	if s.Post != nil {
		c.stmt(s.Post)
	}
	c.code.Jump(jvm.Goto, head)
	c.closeScope(mark)
	c.code.Place(end, false)
}

func (c *Compiler) rangeStmt(s *ast.RangeStmt) {
	label := c.takeLabel()
	mark := c.openScope()
	if s.Tok == token.ASSIGN {
		c.fail(s, "range with assignment (=) is not supported yet; use :=")
	}
	isBlank := func(e ast.Expr) bool {
		if e == nil {
			return true
		}
		id, ok := e.(*ast.Ident)
		return ok && id.Name == "_"
	}
	xt := c.typeOf(s.X)
	head, cont, end := c.code.NewLabel(), c.code.NewLabel(), c.code.NewLabel()
	switch {
	case xt.IsInteger() || xt.K == KUInt || xt.K == KURune:
		t := DefaultOrSelf(xt)
		if !isBlank(s.Value) {
			c.fail(s.Value, "range over %s permits only one iteration variable", t)
		}
		c.exprTo(s.X, t)
		vt := t.VT()
		lim := c.code.NewLocal(vt)
		c.code.Store(lim, vt)
		c.zero(t)
		idx := c.code.NewLocal(vt)
		c.code.Store(idx, vt)
		c.code.Place(head, true)
		c.code.Load(idx, vt)
		c.code.Load(lim, vt)
		c.cmpJump(s, t, token.GEQ, end, true)
		bm := c.openScope()
		if !isBlank(s.Key) {
			c.code.Load(idx, vt)
			slot := c.code.NewLocal(vt)
			c.code.Store(slot, vt)
			c.define(s.Key.(*ast.Ident).Name, &object{kind: oLocal, typ: t, slot: slot})
		}
		c.loops = append(c.loops, &loopCtx{label, end, cont})
		c.stmts(s.Body.List)
		c.loops = c.loops[:len(c.loops)-1]
		c.closeScope(bm)
		c.code.Place(cont, false)
		c.code.Load(idx, vt)
		c.pushOne(t)
		c.arithOp(s, token.ADD, t)
		c.code.Store(idx, vt)
	case xt.K == KArray:
		at := xt
		c.genExpr(s.X, nil)
		avt := at.VT()
		arr := c.code.NewLocal(avt)
		c.code.Store(arr, avt)
		c.code.Iconst(0)
		idx := c.code.NewLocal(jvm.VInt)
		c.code.Store(idx, jvm.VInt)
		c.code.Place(head, true)
		c.code.Load(idx, jvm.VInt)
		c.code.Load(arr, avt)
		c.code.Op(jvm.Arraylength, 1, jvm.VInt)
		c.code.Jump(jvm.IfIcmpge, end)
		bm := c.openScope()
		if !isBlank(s.Key) {
			c.code.Load(idx, jvm.VInt)
			c.code.Op(jvm.I2l, 1, jvm.VLong)
			slot := c.code.NewLocal(jvm.VLong)
			c.code.Store(slot, jvm.VLong)
			c.define(s.Key.(*ast.Ident).Name, &object{kind: oLocal, typ: TInt, slot: slot})
		}
		if !isBlank(s.Value) {
			c.code.Load(arr, avt)
			c.code.Load(idx, jvm.VInt)
			c.arrayLoad(*at.Elem)
			slot := c.code.NewLocal(at.Elem.VT())
			c.code.Store(slot, at.Elem.VT())
			c.define(s.Value.(*ast.Ident).Name, &object{kind: oLocal, typ: *at.Elem, slot: slot})
		}
		c.loops = append(c.loops, &loopCtx{label, end, cont})
		c.stmts(s.Body.List)
		c.loops = c.loops[:len(c.loops)-1]
		c.closeScope(bm)
		c.code.Place(cont, false)
		c.code.Load(idx, jvm.VInt)
		c.code.Iconst(1)
		c.code.Op(jvm.Iadd, 2, jvm.VInt)
		c.code.Store(idx, jvm.VInt)
	default:
		c.fail(s.X, "cannot range over %s (only integers and Java arrays are supported yet)", xt)
	}
	c.code.Jump(jvm.Goto, head)
	c.closeScope(mark)
	c.code.Place(end, false)
}

func (c *Compiler) switchStmt(s *ast.SwitchStmt) {
	label := c.takeLabel()
	mark := c.openScope()
	if s.Init != nil {
		c.stmt(s.Init)
	}
	var tagT Type
	tagSlot := -1
	if s.Tag != nil {
		tagT = DefaultOrSelf(c.typeOf(s.Tag))
		c.exprTo(s.Tag, tagT)
		tagSlot = c.code.NewLocal(tagT.VT())
		c.code.Store(tagSlot, tagT.VT())
	}
	end := c.code.NewLabel()
	bodies := make([]*jvm.Label, len(s.Body.List))
	def := -1
	for i, st := range s.Body.List {
		bodies[i] = c.code.NewLabel()
		cc := st.(*ast.CaseClause)
		if cc.List == nil {
			def = i
			continue
		}
		for _, e := range cc.List {
			if s.Tag != nil {
				c.code.Load(tagSlot, tagT.VT())
				c.exprTo(e, tagT)
				c.cmpJump(e, tagT, token.EQL, bodies[i], true)
			} else {
				c.genCond(e, bodies[i], true)
			}
		}
	}
	if def >= 0 {
		c.code.Jump(jvm.Goto, bodies[def])
	} else {
		c.code.Jump(jvm.Goto, end)
	}
	c.loops = append(c.loops, &loopCtx{label, end, nil})
	for i, st := range s.Body.List {
		cc := st.(*ast.CaseClause)
		c.code.Place(bodies[i], false)
		list := cc.Body
		fall := false
		if n := len(list); n > 0 {
			if br, ok := list[n-1].(*ast.BranchStmt); ok && br.Tok == token.FALLTHROUGH {
				fall = true
				list = list[:n-1]
			}
		}
		c.block(list)
		if fall {
			if i+1 >= len(bodies) {
				c.fail(cc, "cannot fallthrough final case in switch")
			}
			c.code.Jump(jvm.Goto, bodies[i+1])
		} else {
			c.code.Jump(jvm.Goto, end)
		}
	}
	c.loops = c.loops[:len(c.loops)-1]
	c.closeScope(mark)
	c.code.Place(end, false)
}

func (c *Compiler) returnStmt(s *ast.ReturnStmt) {
	fi := c.fn
	if fi == nil {
		c.fail(s, "return outside function")
	}
	if fi.ret.K == KVoid {
		if len(s.Results) > 0 {
			c.fail(s, "too many return values")
		}
		c.code.Return(nil)
		return
	}
	if len(s.Results) != 1 {
		c.fail(s, "not enough return values (have %d, want 1)", len(s.Results))
	}
	c.exprTo(s.Results[0], fi.ret)
	vt := fi.ret.VT()
	c.code.Return(&vt)
}

func (c *Compiler) branchStmt(s *ast.BranchStmt) {
	switch s.Tok {
	case token.BREAK, token.CONTINUE:
		for i := len(c.loops) - 1; i >= 0; i-- {
			l := c.loops[i]
			if s.Label != nil && l.label != s.Label.Name {
				continue
			}
			if s.Tok == token.BREAK {
				c.code.Jump(jvm.Goto, l.brk)
				return
			}
			if l.cont != nil {
				c.code.Jump(jvm.Goto, l.cont)
				return
			}
			if s.Label != nil {
				c.fail(s, "invalid continue label %s", s.Label.Name)
			}
		}
		if s.Label != nil {
			c.fail(s, "invalid %s label %s", s.Tok, s.Label.Name)
		}
		c.fail(s, "%s is not in a loop", s.Tok)
	case token.GOTO:
		c.fail(s, "goto is not supported yet")
	case token.FALLTHROUGH:
		c.fail(s, "fallthrough statement out of place")
	}
}

// ---------------------------------------------------------------------------
// assignment

func (c *Compiler) assignStmt(s *ast.AssignStmt) {
	switch s.Tok {
	case token.DEFINE:
		c.defineStmt(s)
	case token.ASSIGN:
		if len(s.Lhs) != len(s.Rhs) {
			c.fail(s, "assignment mismatch: %d variables but %d values", len(s.Lhs), len(s.Rhs))
		}
		if len(s.Lhs) == 1 {
			lv := c.lvalueOf(s.Lhs[0])
			if lv.kind == lvBlank {
				t := c.genExpr(s.Rhs[0], nil)
				c.checkValue(s.Rhs[0], t)
				c.code.Pop()
				return
			}
			c.exprTo(s.Rhs[0], lv.typ)
			c.lvStore(lv)
			return
		}
		lvs := make([]lval, len(s.Lhs))
		for i, l := range s.Lhs {
			lvs[i] = c.lvalueOf(l)
			if lvs[i].kind == lvField || lvs[i].kind == lvArray {
				c.fail(l, "tuple assignment to fields or elements is not supported yet")
			}
		}
		for i, r := range s.Rhs {
			if lvs[i].kind == lvBlank {
				t := c.genExpr(r, nil)
				c.checkValue(r, t)
			} else {
				c.exprTo(r, lvs[i].typ)
			}
		}
		for i := len(lvs) - 1; i >= 0; i-- {
			c.lvStore(lvs[i])
		}
	default:
		if len(s.Lhs) != 1 || len(s.Rhs) != 1 {
			c.fail(s, "assignment operation %s requires single-valued expressions", s.Tok)
		}
		op := map[token.Token]token.Token{
			token.ADD_ASSIGN: token.ADD, token.SUB_ASSIGN: token.SUB, token.MUL_ASSIGN: token.MUL,
			token.QUO_ASSIGN: token.QUO, token.REM_ASSIGN: token.REM, token.AND_ASSIGN: token.AND,
			token.OR_ASSIGN: token.OR, token.XOR_ASSIGN: token.XOR, token.SHL_ASSIGN: token.SHL,
			token.SHR_ASSIGN: token.SHR, token.AND_NOT_ASSIGN: token.AND_NOT,
		}[s.Tok]
		lv := c.lvalueOf(s.Lhs[0])
		if lv.kind == lvBlank {
			c.fail(s, "cannot use _ as value")
		}
		c.lvLoad(lv)
		if op == token.SHL || op == token.SHR {
			c.genShiftCount(s.Rhs[0])
		} else {
			c.exprTo(s.Rhs[0], lv.typ)
		}
		c.arithOp(s, op, lv.typ)
		c.lvStore(lv)
	}
}

func (c *Compiler) defineStmt(s *ast.AssignStmt) {
	if len(s.Lhs) != len(s.Rhs) {
		c.fail(s, "assignment mismatch: %d variables but %d values (multiple return values are not supported yet)", len(s.Lhs), len(s.Rhs))
	}
	type target struct {
		name     string
		t        Type
		existing *object
	}
	targets := make([]target, len(s.Lhs))
	anyNew := false
	for i, l := range s.Lhs {
		id, ok := l.(*ast.Ident)
		if !ok {
			c.fail(l, "non-name %s on left side of :=", exprStr(l))
		}
		tg := target{name: id.Name}
		if id.Name != "_" {
			if o, ok := c.cur.names[id.Name]; ok && (o.kind == oLocal) {
				tg.existing = o
				tg.t = o.typ
			} else {
				anyNew = true
			}
		}
		targets[i] = tg
	}
	if !anyNew {
		c.fail(s, "no new variables on left side of :=")
	}
	for i, r := range s.Rhs {
		tg := &targets[i]
		if tg.existing != nil {
			c.exprTo(r, tg.t)
			continue
		}
		t := c.genExpr(r, nil)
		c.checkValue(r, t)
		if t.K == KStruct && c.needsCopy(r) {
			c.copyStruct(t)
		}
		tg.t = t
	}
	for i := len(targets) - 1; i >= 0; i-- {
		tg := targets[i]
		switch {
		case tg.name == "_":
			c.code.Pop()
		case tg.existing != nil:
			c.code.Store(tg.existing.slot, tg.t.VT())
		default:
			slot := c.code.NewLocal(tg.t.VT())
			c.code.Store(slot, tg.t.VT())
			c.define(tg.name, &object{kind: oLocal, typ: tg.t, slot: slot})
		}
	}
}

const (
	lvBlank = iota
	lvLocal
	lvGlobal
	lvStatic
	lvField
	lvArray
)

type lval struct {
	kind              int
	typ               Type
	slot              int
	owner, name, desc string
}

// lvalueOf resolves an assignable expression and emits any operand preparation
// (receiver object, or array and index) onto the stack.
func (c *Compiler) lvalueOf(e ast.Expr) lval {
	e = unparen(e)
	switch e := e.(type) {
	case *ast.Ident:
		if e.Name == "_" {
			return lval{kind: lvBlank}
		}
		o := c.lookup(e.Name)
		if o == nil {
			c.fail(e, "undefined: %s", e.Name)
		}
		switch o.kind {
		case oLocal:
			if o.typ.K == KInvalid {
				panic(silentError{})
			}
			return lval{kind: lvLocal, typ: o.typ, slot: o.slot}
		case oGlobal:
			return lval{kind: lvGlobal, typ: o.typ, owner: c.mainName, name: e.Name, desc: o.typ.Desc()}
		}
		c.fail(e, "cannot assign to %s", e.Name)
	case *ast.SelectorExpr:
		if cls, ok := c.classRef(e.X); ok {
			m, owner := c.findField(e, cls, e.Sel.Name)
			if m.Access&jvm.AccStatic == 0 {
				c.fail(e, "%s is not a static field", exprStr(e))
			}
			return lval{kind: lvStatic, typ: TypeFromDesc(m.Desc), owner: owner, name: m.Name, desc: m.Desc}
		}
		xt := c.genExpr(e.X, nil)
		if xt.K == KStruct || xt.K == KPtr {
			f := c.structField(e, xt, e.Sel.Name)
			return lval{kind: lvField, typ: f.typ, owner: xt.Class, name: f.name, desc: f.typ.Desc()}
		}
		if xt.K != KClass {
			c.fail(e, "%s undefined (type %s has no field)", exprStr(e), xt)
		}
		m, _ := c.findField(e, xt.Class, e.Sel.Name)
		if m.Access&jvm.AccStatic != 0 {
			c.code.Pop()
			return lval{kind: lvStatic, typ: TypeFromDesc(m.Desc), owner: xt.Class, name: m.Name, desc: m.Desc}
		}
		return lval{kind: lvField, typ: TypeFromDesc(m.Desc), owner: xt.Class, name: m.Name, desc: m.Desc}
	case *ast.IndexExpr:
		at := c.genExpr(e.X, nil)
		if at.K != KArray {
			c.fail(e, "invalid operation: cannot index %s", at)
		}
		c.genIndex(e.Index)
		return lval{kind: lvArray, typ: *at.Elem}
	}
	c.fail(e, "cannot assign to %s", exprStr(e))
	return lval{}
}

func (c *Compiler) lvLoad(lv lval) {
	switch lv.kind {
	case lvLocal:
		c.code.Load(lv.slot, lv.typ.VT())
	case lvGlobal, lvStatic:
		c.code.Field(jvm.Getstatic, lv.owner, lv.name, lv.desc)
	case lvField:
		c.code.Dup()
		c.code.Field(jvm.Getfield, lv.owner, lv.name, lv.desc)
	case lvArray:
		c.code.Dup2()
		c.arrayLoad(lv.typ)
	}
}

func (c *Compiler) lvStore(lv lval) {
	switch lv.kind {
	case lvBlank:
		c.code.Pop()
	case lvLocal:
		c.code.Store(lv.slot, lv.typ.VT())
	case lvGlobal, lvStatic:
		c.code.Field(jvm.Putstatic, lv.owner, lv.name, lv.desc)
	case lvField:
		c.code.Field(jvm.Putfield, lv.owner, lv.name, lv.desc)
	case lvArray:
		c.arrayStore(lv.typ)
	}
}

// pushOne pushes the constant 1 of type t.
func (c *Compiler) pushOne(t Type) {
	switch {
	case t.K == KInt || t.K == KInt64:
		c.code.Lconst(1)
	case t.K == KFloat32:
		c.code.Fconst(1)
	case t.K == KFloat64:
		c.code.Dconst(1)
	default:
		c.code.Iconst(1)
	}
}

func (c *Compiler) arrayOpDesc(t Type) byte {
	return t.Desc()[0]
}

func (c *Compiler) arrayLoad(elem Type) {
	var op byte
	switch c.arrayOpDesc(elem) {
	case 'I':
		op = 46
	case 'J':
		op = 47
	case 'F':
		op = 48
	case 'D':
		op = 49
	case 'B', 'Z':
		op = 51
	case 'C':
		op = 52
	case 'S':
		op = 53
	default:
		op = 50
	}
	c.code.Op(op, 2, elem.VT())
}

func (c *Compiler) arrayStore(elem Type) {
	var op byte
	switch c.arrayOpDesc(elem) {
	case 'I':
		op = 79
	case 'J':
		op = 80
	case 'F':
		op = 81
	case 'D':
		op = 82
	case 'B', 'Z':
		op = 84
	case 'C':
		op = 85
	case 'S':
		op = 86
	default:
		op = 83
	}
	c.code.Op(op, 3)
}

func methodName(fi *funcInfo) string {
	if fi.recv != nil && fi.jname != "" {
		return fi.jname
	}
	return fi.name
}
