package compiler

import (
	"go/ast"
	"go/token"
	"strings"

	"github.com/AydenC77/gojvm/internal/jvm"
)

const (
	strDesc = "Ljava/lang/String;"
	sbClass = "java/lang/StringBuilder"
)

// classRef resolves pkg.Class (or pkg.Outer.Inner) to a JVM internal class name.
func (c *Compiler) classRef(e ast.Expr) (string, bool) {
	sel, ok := unparen(e).(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	if id, ok := sel.X.(*ast.Ident); ok {
		o := c.lookup(id.Name)
		if o == nil || o.kind != oPkg || o.pkg.javaPkg == "" {
			return "", false
		}
		name := o.pkg.javaPkg + "/" + sel.Sel.Name
		if c.cp.Lookup(name) != nil {
			return name, true
		}
		return "", false
	}
	if outer, ok := c.classRef(sel.X); ok {
		name := outer + "$" + sel.Sel.Name
		if c.cp.Lookup(name) != nil {
			return name, true
		}
	}
	return "", false
}

func (c *Compiler) findField(n ast.Node, class, goName string) (*jvm.Member, string) {
	for _, name := range []string{goName, lowerFirst(goName)} {
		if m, owner := c.cp.FieldNamed(class, name); m != nil {
			return m, owner
		}
	}
	c.fail(n, "undefined: field %s in %s", goName, strings.ReplaceAll(class, "/", "."))
	return nil, ""
}

func (c *Compiler) genSelector(e *ast.SelectorExpr) Type {
	if e.Sel.Name == CompleteMarker && c.opts.Complete {
		c.completeSelector(e)
	}
	if id, ok := e.X.(*ast.Ident); ok {
		if o := c.lookup(id.Name); o != nil && o.kind == oPkg {
			c.fail(e, "undefined: %s.%s", id.Name, e.Sel.Name)
		}
	}
	if cls, ok := c.classRef(e.X); ok {
		m, _ := c.findField(e, cls, e.Sel.Name)
		if m.Access&jvm.AccStatic == 0 {
			c.fail(e, "%s is not a static field", exprStr(e))
		}
		c.code.Field(jvm.Getstatic, cls, m.Name, m.Desc)
		return TypeFromDesc(m.Desc)
	}
	if _, ok := c.classRef(e); ok {
		c.fail(e, "%s (type) is not an expression", exprStr(e))
	}
	xt := c.genExpr(e.X, nil)
	if xt.K == KStruct || xt.K == KPtr {
		f := c.structField(e, xt, e.Sel.Name)
		c.code.Field(jvm.Getfield, xt.Class, f.name, f.typ.Desc())
		return f.typ
	}
	if xt.K != KClass {
		c.fail(e, "%s undefined (type %s has no field or method %s)", exprStr(e), xt, e.Sel.Name)
	}
	m, _ := c.findField(e, xt.Class, e.Sel.Name)
	if m.Access&jvm.AccStatic != 0 {
		c.code.Pop()
		c.code.Field(jvm.Getstatic, xt.Class, m.Name, m.Desc)
	} else {
		c.code.Field(jvm.Getfield, xt.Class, m.Name, m.Desc)
	}
	return TypeFromDesc(m.Desc)
}

// ---------------------------------------------------------------------------
// calls

func (c *Compiler) genCall(e *ast.CallExpr, hint *Type) Type {
	fun := unparen(e.Fun)
	if c.isTypeExpr(fun) {
		return c.genConversion(e, c.resolveType(fun))
	}
	if e.Ellipsis.IsValid() {
		c.fail(e, "calls with ... are not supported yet")
	}
	switch f := fun.(type) {
	case *ast.Ident:
		o := c.lookup(f.Name)
		if o == nil {
			c.fail(f, "undefined: %s", f.Name)
		}
		switch o.kind {
		case oBuiltin:
			return c.builtinCall(e, f.Name)
		case oFunc:
			fi := o.fn
			if len(e.Args) != len(fi.params) {
				c.fail(e, "wrong number of arguments in call to %s (have %d, want %d)", fi.name, len(e.Args), len(fi.params))
			}
			for i, a := range e.Args {
				c.exprTo(a, fi.params[i])
			}
			c.code.Invoke(jvm.Invokestatic, c.mainName, fi.name, fi.desc, false)
			return fi.ret
		}
		c.fail(f, "cannot call non-function %s", f.Name)
	case *ast.SelectorExpr:
		return c.selectorCall(e, f)
	}
	c.fail(e, "unsupported call expression")
	return Type{}
}

func (c *Compiler) selectorCall(e *ast.CallExpr, f *ast.SelectorExpr) Type {
	if f.Sel.Name == CompleteMarker && c.opts.Complete {
		c.completeSelector(f)
	}
	name := f.Sel.Name
	if id, ok := f.X.(*ast.Ident); ok {
		if o := c.lookup(id.Name); o != nil && o.kind == oPkg {
			switch o.pkg.intrinsic {
			case "fmt":
				return c.fmtCall(e, name)
			case "os":
				if name == "Exit" && len(e.Args) == 1 {
					c.exprTo(e.Args[0], TInt)
					c.code.Op(jvm.L2i, 1, jvm.VInt)
					c.code.Invoke(jvm.Invokestatic, "java/lang/System", "exit", "(I)V", false)
					return TVoid
				}
				c.fail(f, "undefined: os.%s", name)
			case "math":
				if mname, ok := map[string]string{"Mod": "IEEEremainder"}[name]; ok {
					name = mname
				}
				return c.javaStatic(e, "java/lang/Math", name)
			}
			if strings.HasPrefix(name, "New") && len(name) > 3 {
				cls := o.pkg.javaPkg + "/" + name[3:]
				if c.cp.Lookup(cls) != nil {
					return c.javaNew(e, cls)
				}
			}
			c.fail(f, "undefined: %s.%s", id.Name, name)
		}
	}
	if cls, ok := c.classRef(f.X); ok {
		return c.javaStatic(e, cls, name)
	}
	recv := c.genExpr(f.X, nil)
	if recv.K == KStruct || recv.K == KPtr {
		return c.userMethodCall(e, recv, name)
	}
	return c.javaInstance(e, recv, name)
}

type argInfo struct {
	t  Type
	cv *constVal
}

func (c *Compiler) argInfos(args []ast.Expr) []argInfo {
	out := make([]argInfo, len(args))
	for i, a := range args {
		t := c.typeOf(a)
		if t.K == KVoid {
			c.fail(a, "%s (no value) used as value", exprStr(a))
		}
		out[i] = argInfo{t: t, cv: c.constOf(a)}
	}
	return out
}

func (c *Compiler) boxCost(a, p Type) (int, bool) {
	bx, ok := boxOf(a)
	if !ok || !p.IsRef() {
		return 0, false
	}
	if !c.refAssignable(ClassType(bx.class), p) {
		return 0, false
	}
	if p.K == KClass && p.Class == bx.class {
		return 10, true
	}
	return 11, true
}

// argCost scores passing an argument to a parameter; ok is false when not applicable.
func (c *Compiler) argCost(a argInfo, p Type) (int, bool) {
	t := a.t
	switch {
	case t.K == KNil:
		return 0, p.IsRef()
	case t.IsUntyped():
		switch t.K {
		case KUBool:
			if p.K == KBool {
				return 0, true
			}
			return c.boxCost(TBool, p)
		case KUString:
			if p.K == KString {
				return 0, true
			}
			if p.IsRef() && c.refAssignable(TString, p) {
				return 1, true
			}
			return 0, false
		case KUInt:
			switch {
			case p.IsInteger() && c.constFits(a.cv, p):
				switch p.K {
				case KInt, KInt64:
					return 0, true
				case KInt32:
					if p.JD == "C" {
						return 3, true
					}
					return 1, true
				case KInt16:
					return 2, true
				}
				return 3, true
			case p.K == KFloat64:
				return 4, true
			case p.K == KFloat32:
				return 5, true
			}
			return c.boxCost(TInt, p)
		case KURune:
			switch {
			case p.K == KInt32 && p.JD == "C":
				return 0, true
			case p.K == KInt32:
				return 1, true
			case p.IsInteger() && c.constFits(a.cv, p):
				return 2, true
			case p.K == KFloat64:
				return 5, true
			}
			return c.boxCost(TInt32, p)
		case KUFloat:
			switch p.K {
			case KFloat64:
				return 0, true
			case KFloat32:
				return 1, true
			}
			return c.boxCost(TFloat64, p)
		}
		return 0, false
	case t.IsPrim():
		if p.IsPrim() {
			if !Identical(t, p) {
				return 0, false
			}
			if t.K == KInt32 && t.JD != p.JD {
				return 1, true
			}
			return 0, true
		}
		return c.boxCost(t, p)
	default: // reference
		if !p.IsRef() {
			return 0, false
		}
		if Identical(t, p) {
			return 0, true
		}
		if c.refAssignable(t, p) {
			return 1, true
		}
		return 0, false
	}
}

type chosen struct {
	m      jvm.Member
	va     bool
	params []Type
}

func (c *Compiler) pickMethod(n ast.Node, desc string, cands []jvm.Member, args []ast.Expr) chosen {
	infos := c.argInfos(args)
	var best []chosen
	bestCost := 1 << 30
	for _, m := range cands {
		pds, _ := jvm.SplitDesc(m.Desc)
		pts := make([]Type, len(pds))
		for i, d := range pds {
			pts[i] = TypeFromDesc(d)
		}
		cost, ok := 0, true
		va := false
		if len(pts) == len(infos) {
			for i, a := range infos {
				cc, ok2 := c.argCost(a, pts[i])
				if !ok2 {
					ok = false
					break
				}
				cost += cc
			}
		} else {
			ok = false
		}
		if !ok && m.Access&jvm.AccVarargs != 0 && len(pts) > 0 && len(infos) >= len(pts)-1 {
			ok, cost, va = true, 20, true
			last := pts[len(pts)-1]
			for i, a := range infos {
				var p Type
				if i < len(pts)-1 {
					p = pts[i]
				} else if last.K == KArray {
					p = *last.Elem
				} else {
					ok = false
					break
				}
				cc, ok2 := c.argCost(a, p)
				if !ok2 {
					ok = false
					break
				}
				cost += cc
			}
		}
		if !ok {
			continue
		}
		switch {
		case cost < bestCost:
			bestCost = cost
			best = []chosen{{m, va, pts}}
		case cost == bestCost:
			best = append(best, chosen{m, va, pts})
		}
	}
	if len(best) == 0 {
		var have []string
		for _, a := range infos {
			have = append(have, a.t.String())
		}
		var sigs []string
		for _, m := range cands {
			sigs = append(sigs, "\t"+strings.Replace(prettyDesc(m), "<init>", desc, 1))
		}
		c.fail(n, "no matching overload for %s(%s)\n  candidates:\n%s", desc, strings.Join(have, ", "), strings.Join(sigs, "\n"))
	}
	if len(best) > 1 {
		var sigs []string
		for _, b := range best {
			sigs = append(sigs, "\t"+strings.Replace(prettyDesc(b.m), "<init>", desc, 1))
		}
		c.fail(n, "ambiguous call to %s; cast an argument to pick one:\n%s", desc, strings.Join(sigs, "\n"))
	}
	return best[0]
}

func prettyDesc(m jvm.Member) string {
	pds, ret := jvm.SplitDesc(m.Desc)
	var ps []string
	for _, d := range pds {
		ps = append(ps, TypeFromDesc(d).String())
	}
	r := ""
	if ret != "V" {
		r = " " + TypeFromDesc(ret).String()
	}
	return m.Name + "(" + strings.Join(ps, ", ") + ")" + r
}

func (c *Compiler) newArray(elem Type) {
	if elem.IsPrim() {
		var at byte
		switch elem.Desc()[0] {
		case 'Z':
			at = 4
		case 'C':
			at = 5
		case 'F':
			at = 6
		case 'D':
			at = 7
		case 'B':
			at = 8
		case 'S':
			at = 9
		case 'I':
			at = 10
		default:
			at = 11
		}
		c.code.NewPrimArray(at, "["+elem.Desc())
		return
	}
	c.code.Anewarray(elem.RefName())
}

func (c *Compiler) emitArgs(ch chosen, args []ast.Expr) {
	n := len(ch.params)
	if !ch.va {
		for i, a := range args {
			c.exprTo(a, ch.params[i])
		}
		return
	}
	for i := 0; i < n-1; i++ {
		c.exprTo(args[i], ch.params[i])
	}
	elem := *ch.params[n-1].Elem
	rest := args[n-1:]
	c.code.Iconst(int32(len(rest)))
	c.newArray(elem)
	for j, a := range rest {
		c.code.Dup()
		c.code.Iconst(int32(j))
		c.exprTo(a, elem)
		c.arrayStore(elem)
	}
}

func (c *Compiler) candidates(owner, goName string, static bool) []jvm.Member {
	for _, name := range []string{lowerFirst(goName), goName} {
		var out []jvm.Member
		for _, m := range c.cp.MethodsNamed(owner, name) {
			if (m.Access&jvm.AccStatic != 0) == static {
				out = append(out, m)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func (c *Compiler) javaStatic(e *ast.CallExpr, cls, goName string) Type {
	cands := c.candidates(cls, goName, true)
	short := cls[strings.LastIndex(cls, "/")+1:]
	if len(cands) == 0 {
		c.fail(e.Fun, "undefined: %s.%s (no static method)", short, goName)
	}
	ch := c.pickMethod(e, short+"."+goName, cands, e.Args)
	c.emitArgs(ch, e.Args)
	_, ret := jvm.SplitDesc(ch.m.Desc)
	ci := c.cp.Lookup(cls)
	c.code.Invoke(jvm.Invokestatic, cls, ch.m.Name, ch.m.Desc, ci != nil && ci.IsInterface())
	return TypeFromDesc(ret)
}

func (c *Compiler) javaInstance(e *ast.CallExpr, recv Type, goName string) Type {
	var owner string
	switch recv.K {
	case KString:
		owner = "java/lang/String"
	case KClass:
		owner = recv.Class
	default:
		c.fail(e.Fun, "%s undefined (type %s has no methods)", exprStr(e.Fun), recv)
	}
	cands := c.candidates(owner, goName, false)
	if len(cands) == 0 {
		c.fail(e.Fun, "%s undefined (type %s has no method %s)", exprStr(e.Fun), recv, goName)
	}
	ch := c.pickMethod(e, exprStr(e.Fun), cands, e.Args)
	c.emitArgs(ch, e.Args)
	_, ret := jvm.SplitDesc(ch.m.Desc)
	ci := c.cp.Lookup(owner)
	op := byte(jvm.Invokevirtual)
	if ci != nil && ci.IsInterface() {
		op = jvm.Invokeinterface
	}
	c.code.Invoke(op, owner, ch.m.Name, ch.m.Desc, false)
	return TypeFromDesc(ret)
}

func (c *Compiler) javaNew(e *ast.CallExpr, cls string) Type {
	ci := c.cp.Lookup(cls)
	if ci.IsInterface() || ci.Access&jvm.AccAbstract != 0 {
		c.fail(e, "cannot instantiate abstract class or interface %s", strings.ReplaceAll(cls, "/", "."))
	}
	var cands []jvm.Member
	for _, m := range ci.Methods {
		if m.Name == "<init>" && m.Access&jvm.AccPublic != 0 {
			cands = append(cands, m)
		}
	}
	short := cls[strings.LastIndex(cls, "/")+1:]
	if len(cands) == 0 {
		c.fail(e, "%s has no public constructor", short)
	}
	ch := c.pickMethod(e, "New"+short, cands, e.Args)
	c.code.New(cls)
	c.code.Dup()
	c.emitArgs(ch, e.Args)
	c.code.Invoke(jvm.Invokespecial, cls, "<init>", ch.m.Desc, false)
	return ClassType(cls)
}

// ---------------------------------------------------------------------------
// builtins and fmt

func (c *Compiler) ensureTrim() {
	if c.trimHelp {
		return
	}
	c.trimHelp = true
	cp := c.mainCls.CP
	vt := jvm.VObj("java/lang/String")
	code := jvm.NewCode(cp, []jvm.VT{vt})
	l := code.NewLabel()
	code.Load(0, vt)
	code.Sconst(".0")
	code.Invoke(jvm.Invokevirtual, "java/lang/String", "endsWith", "(Ljava/lang/String;)Z", false)
	code.Jump(jvm.Ifne, l)
	code.Load(0, vt)
	code.Return(&vt)
	code.Place(l, false)
	code.Load(0, vt)
	code.Iconst(0)
	code.Load(0, vt)
	code.Invoke(jvm.Invokevirtual, "java/lang/String", "length", "()I", false)
	code.Iconst(2)
	code.Op(jvm.Isub, 2, jvm.VInt)
	code.Invoke(jvm.Invokevirtual, "java/lang/String", "substring", "(II)Ljava/lang/String;", false)
	code.Return(&vt)
	c.mainCls.Methods = append(c.mainCls.Methods, &jvm.Method{Access: 0x1009, Name: "$trim", Desc: "(" + strDesc + ")" + strDesc, Code: code})
}

// toStr converts the value of type t on top of the stack to a String.
func (c *Compiler) toStr(t Type) {
	valueOf := func(d string) {
		c.code.Invoke(jvm.Invokestatic, "java/lang/String", "valueOf", "("+d+")"+strDesc, false)
	}
	switch {
	case t.K == KString:
	case t.K == KNil:
		c.code.Pop()
		c.code.Sconst("<nil>")
	case t.K == KBool:
		valueOf("Z")
	case t.K == KInt32 && t.JD == "C":
		valueOf("C")
	case t.K == KInt8 || t.K == KInt16 || t.K == KInt32:
		valueOf("I")
	case t.K == KInt || t.K == KInt64:
		valueOf("J")
	case t.K == KFloat32:
		c.ensureTrim()
		c.code.Invoke(jvm.Invokestatic, "java/lang/Float", "toString", "(F)"+strDesc, false)
		c.code.Invoke(jvm.Invokestatic, c.mainName, "$trim", "("+strDesc+")"+strDesc, false)
	case t.K == KFloat64:
		c.ensureTrim()
		c.code.Invoke(jvm.Invokestatic, "java/lang/Double", "toString", "(D)"+strDesc, false)
		c.code.Invoke(jvm.Invokestatic, c.mainName, "$trim", "("+strDesc+")"+strDesc, false)
	default:
		valueOf("Ljava/lang/Object;")
	}
}

// genJoin builds a String from args the way fmt.Print/Println do.
func (c *Compiler) genJoin(args []ast.Expr, ln bool) {
	c.code.New(sbClass)
	c.code.Dup()
	c.code.Invoke(jvm.Invokespecial, sbClass, "<init>", "()V", false)
	app := func() {
		c.code.Invoke(jvm.Invokevirtual, sbClass, "append", "("+strDesc+")L"+sbClass+";", false)
	}
	for i, a := range args {
		if i > 0 {
			sep := ln
			if !ln {
				sep = c.typeOf(args[i-1]).K != KString && DefaultType(c.typeOf(a)).K != KString
				if c.typeOf(args[i-1]).K == KUString {
					sep = false
				}
			}
			if sep {
				c.code.Sconst(" ")
				app()
			}
		}
		t := c.genExpr(a, nil)
		if t.K == KVoid {
			c.fail(a, "%s (no value) used as value", exprStr(a))
		}
		c.toStr(t)
		app()
	}
	if ln {
		c.code.Sconst("\n")
		app()
	}
	c.code.Invoke(jvm.Invokevirtual, sbClass, "toString", "()"+strDesc, false)
}

func (c *Compiler) printStream(field string) {
	c.code.Field(jvm.Getstatic, "java/lang/System", field, "Ljava/io/PrintStream;")
}

func (c *Compiler) printString() {
	c.code.Invoke(jvm.Invokevirtual, "java/io/PrintStream", "print", "("+strDesc+")V", false)
}

func translateFormat(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		b.WriteByte('%')
		i++
		for i < len(s) && strings.IndexByte("+-# 0123456789.", s[i]) >= 0 {
			b.WriteByte(s[i])
			i++
		}
		if i >= len(s) {
			break
		}
		switch s[i] {
		case 'v':
			b.WriteByte('s')
		case 't':
			b.WriteByte('b')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func (c *Compiler) genFormat(e *ast.CallExpr) {
	if len(e.Args) == 0 {
		c.fail(e, "missing format string")
	}
	f := e.Args[0]
	if cv := c.constOf(f); cv != nil && cv.t.K == KUString {
		c.code.Sconst(translateFormat(constString(cv)))
	} else {
		c.exprTo(f, TString)
	}
	rest := e.Args[1:]
	c.code.Iconst(int32(len(rest)))
	c.code.Anewarray("java/lang/Object")
	for i, a := range rest {
		c.code.Dup()
		c.code.Iconst(int32(i))
		c.exprTo(a, TObject)
		c.code.Op(83, 3)
	}
	c.code.Invoke(jvm.Invokestatic, "java/lang/String", "format", "("+strDesc+"[Ljava/lang/Object;)"+strDesc, false)
}

func (c *Compiler) fmtCall(e *ast.CallExpr, name string) Type {
	switch name {
	case "Println", "Print":
		c.printStream("out")
		c.genJoin(e.Args, name == "Println")
		c.printString()
		return TVoid
	case "Printf":
		c.printStream("out")
		c.genFormat(e)
		c.printString()
		return TVoid
	case "Sprintf":
		c.genFormat(e)
		return TString
	case "Sprint", "Sprintln":
		c.genJoin(e.Args, name == "Sprintln")
		return TString
	}
	c.fail(e.Fun, "undefined: fmt.%s", name)
	return Type{}
}

func (c *Compiler) builtinCall(e *ast.CallExpr, name string) Type {
	switch name {
	case "len":
		if len(e.Args) != 1 {
			c.fail(e, "len expects one argument")
		}
		t := c.genExpr(e.Args[0], nil)
		switch t.K {
		case KString:
			c.code.Invoke(jvm.Invokevirtual, "java/lang/String", "length", "()I", false)
		case KArray:
			c.code.Op(jvm.Arraylength, 1, jvm.VInt)
		default:
			c.fail(e, "invalid argument: %s (type %s) for built-in len", exprStr(e.Args[0]), t)
		}
		c.code.Op(jvm.I2l, 1, jvm.VLong)
		return TInt
	case "new":
		if len(e.Args) != 1 || !c.isTypeExpr(e.Args[0]) {
			c.fail(e, "new expects a type argument")
		}
		t := c.resolveType(e.Args[0])
		if t.K != KStruct {
			c.fail(e, "new(%s) is only supported for struct types", t)
		}
		c.zero(t)
		return Type{K: KPtr, Class: t.Class}
	case "print", "println":
		c.printStream("err")
		c.genJoin(e.Args, name == "println")
		c.printString()
		return TVoid
	case "panic":
		if len(e.Args) != 1 {
			c.fail(e, "panic expects one argument")
		}
		c.code.New("java/lang/RuntimeException")
		c.code.Dup()
		t := c.genExpr(e.Args[0], nil)
		c.toStr(t)
		c.code.Invoke(jvm.Invokespecial, "java/lang/RuntimeException", "<init>", "("+strDesc+")V", false)
		c.code.Athrow()
		return TVoid
	case "min", "max":
		if len(e.Args) < 1 {
			c.fail(e, "not enough arguments in call to %s", name)
		}
		var t Type
		first := true
		for _, a := range e.Args {
			at := c.typeOf(a)
			if !at.IsUntyped() && first {
				t, first = at, false
			}
		}
		if first {
			t = DefaultType(c.typeOf(e.Args[0]))
		}
		if !(t.K == KInt || t.K == KInt64 || t.K == KInt32 || t.IsFloat()) {
			c.fail(e, "%s is not supported for type %s", name, t)
		}
		d := t.Desc()
		c.exprTo(e.Args[0], t)
		for _, a := range e.Args[1:] {
			c.exprTo(a, t)
			c.code.Invoke(jvm.Invokestatic, "java/lang/Math", name, "("+d+d+")"+d, false)
		}
		return t
	}
	c.fail(e, "unsupported builtin %s", name)
	return Type{}
}

var _ = token.ADD
