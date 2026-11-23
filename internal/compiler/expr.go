package compiler

import (
	"go/ast"
	"go/constant"
	"go/token"

	"github.com/AydenC77/gojvm/internal/jvm"
)

var (
	trueVal  = constant.MakeBool(true)
	falseVal = constant.MakeBool(false)
)

// typeOf returns the type of e without emitting code (a memoized dry run).
// Untyped constants return their untyped type.
func (c *Compiler) typeOf(e ast.Expr) Type {
	e = unparen(e)
	if cv := c.constOf(e); cv != nil {
		return cv.t
	}
	if isNilIdent(c, e) {
		return TNil
	}
	if t, ok := c.tcache[e]; ok {
		return t
	}
	st := c.code.Save()
	t := c.genExpr(e, nil)
	c.code.Restore(st)
	c.tcache[e] = t
	return t
}

func isNilIdent(c *Compiler, e ast.Expr) bool {
	id, ok := unparen(e).(*ast.Ident)
	if !ok {
		return false
	}
	o := c.lookup(id.Name)
	return o != nil && o.kind == oNil
}

// exprTo generates e converted (assignment-compatible) to type to.
func (c *Compiler) exprTo(e ast.Expr, to Type) {
	t := c.genExpr(e, &to)
	if t.K == KStruct && c.needsCopy(e) {
		c.copyStruct(t)
	}
	c.assign(e, t, to)
}

func (c *Compiler) refAssignable(from, to Type) bool {
	if to.K == KClass && to.Class == "java/lang/Object" {
		return from.IsRef()
	}
	switch from.K {
	case KNil:
		return to.IsRef() && to.K != KStruct
	case KStruct, KPtr:
		si := c.structOf(from)
		if si == nil || to.K != KClass {
			return false
		}
		for _, r := range si.javaRoots() {
			if c.cp.IsSubclass(r, to.Class) {
				return true
			}
		}
		return false
	case KString:
		return to.K == KString || (to.K == KClass && c.cp.IsSubclass("java/lang/String", to.Class))
	case KClass:
		return to.K == KClass && c.cp.IsSubclass(from.Class, to.Class)
	case KArray:
		switch to.K {
		case KArray:
			if from.Elem.IsPrim() || to.Elem.IsPrim() {
				return Identical(*from.Elem, *to.Elem)
			}
			return c.refAssignable(*from.Elem, *to.Elem)
		case KClass:
			return to.Class == "java/lang/Cloneable" || to.Class == "java/io/Serializable"
		}
	}
	return false
}

// assign emits any conversion needed to use a value of type from as type to.
func (c *Compiler) assign(n ast.Node, from, to Type) {
	if from.K == KVoid {
		c.fail(n, "%s (no value) used as value", exprStr(n))
	}
	if Identical(from, to) {
		return
	}
	if from.IsRef() && to.IsRef() && c.refAssignable(from, to) {
		return
	}
	if from.IsPrim() && to.IsRef() {
		if bx, ok := boxOf(from); ok && c.refAssignable(ClassType(bx.class), to) {
			c.code.Invoke(jvm.Invokestatic, bx.class, "valueOf", "("+bx.valueDesc+")L"+bx.class+";", false)
			return
		}
	}
	c.fail(n, "cannot use %s (value of type %s) as %s value", exprStr(n), from, to)
}

func (c *Compiler) untypedCompat(cv *constVal, h Type) bool {
	switch cv.t.K {
	case KUBool:
		return h.K == KBool
	case KUString:
		return h.K == KString
	case KUInt, KURune, KUFloat:
		return h.IsNumeric()
	}
	return false
}

func (c *Compiler) genConstExpr(e ast.Expr, cv *constVal, hint *Type) Type {
	t := cv.t
	if t.IsUntyped() {
		target := DefaultType(t)
		if hint != nil && c.untypedCompat(cv, *hint) {
			target = *hint
		}
		t = target
	}
	c.emitConst(e, cv, t)
	return t
}

// genExpr emits code for e leaving its value on the stack and returns its type.
func (c *Compiler) genExpr(e ast.Expr, hint *Type) Type {
	e = unparen(e)
	if cv := c.constOf(e); cv != nil {
		return c.genConstExpr(e, cv, hint)
	}
	switch e := e.(type) {
	case *ast.Ident:
		if e.Name == CompleteMarker && c.opts.Complete {
			c.completeScope()
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
			c.code.Load(o.slot, o.typ.VT())
			return o.typ
		case oGlobal:
			c.code.Field(jvm.Getstatic, c.mainName, e.Name, o.typ.Desc())
			return o.typ
		case oNil:
			c.code.Null()
			return TNil
		case oFunc, oBuiltin:
			c.fail(e, "%s (function) must be called", e.Name)
		case oType:
			c.fail(e, "%s (type) is not an expression", e.Name)
		case oPkg:
			c.fail(e, "use of package %s without selector", e.Name)
		}
		c.fail(e, "cannot use %s as value", e.Name)
	case *ast.UnaryExpr:
		return c.genUnary(e, hint)
	case *ast.BinaryExpr:
		return c.genBinary(e, hint)
	case *ast.CallExpr:
		c.code.Line(c.fset.Position(e.Pos()).Line)
		return c.genCall(e, hint)
	case *ast.SelectorExpr:
		return c.genSelector(e)
	case *ast.IndexExpr:
		at := c.genExpr(e.X, nil)
		if at.K != KArray {
			c.fail(e, "invalid operation: cannot index %s (type %s)", exprStr(e.X), at)
		}
		c.genIndex(e.Index)
		c.arrayLoad(*at.Elem)
		return *at.Elem
	case *ast.TypeAssertExpr:
		if e.Type == nil {
			c.fail(e, "use of .(type) outside type switch")
		}
		xt := c.genExpr(e.X, nil)
		if !xt.IsRef() || xt.K == KNil {
			c.fail(e, "invalid operation: %s is not an interface or Java object (type %s)", exprStr(e.X), xt)
		}
		t := c.resolveType(e.Type)
		if bx, ok := boxOf(t); ok {
			c.code.Checkcast(bx.class)
			c.code.Invoke(jvm.Invokevirtual, bx.class, bx.unboxName, "()"+bx.valueDesc, false)
		} else {
			c.code.Checkcast(t.RefName())
		}
		return t
	case *ast.StarExpr:
		xt := c.genExpr(e.X, nil)
		if xt.K != KPtr {
			c.fail(e, "invalid operation: cannot indirect %s (type %s)", exprStr(e.X), xt)
		}
		return Type{K: KStruct, Class: xt.Class}
	case *ast.CompositeLit:
		return c.genCompositeLit(e)
	case *ast.FuncLit:
		c.fail(e, "function literals are not supported yet")
	case *ast.SliceExpr:
		c.fail(e, "slice expressions are not supported yet")
	}
	c.fail(e, "unsupported expression")
	return Type{}
}

func (c *Compiler) genIndex(e ast.Expr) {
	t := c.typeOf(e)
	switch {
	case t.IsUntyped():
		c.exprTo(e, TInt32)
	case t.IsInteger():
		c.genExpr(e, nil)
		if t.K == KInt || t.K == KInt64 {
			c.code.Op(jvm.L2i, 1, jvm.VInt)
		}
	default:
		c.fail(e, "invalid argument: index %s must be integer", exprStr(e))
	}
}

func (c *Compiler) genUnary(e *ast.UnaryExpr, hint *Type) Type {
	switch e.Op {
	case token.AND:
		return c.genAddr(e)
	case token.NOT:
		return c.genBoolValue(e)
	case token.ADD:
		return c.genExpr(e.X, hint)
	case token.SUB:
		t := c.genExpr(e.X, hint)
		if !t.IsNumeric() {
			c.fail(e, "operator - not defined on %s", t)
		}
		c.code.Op(byte(jvm.Ineg+t.VT().Class()), 1, t.VT())
		c.narrow(t)
		return t
	case token.XOR:
		t := c.genExpr(e.X, hint)
		if !t.IsInteger() {
			c.fail(e, "operator ^ not defined on %s", t)
		}
		c.pushMinusOne(t)
		c.code.Op(byte(jvm.Ixor+t.VT().Class()), 2, t.VT())
		c.narrow(t)
		return t
	}
	c.fail(e, "unsupported unary operator %s", e.Op)
	return Type{}
}

func (c *Compiler) pushMinusOne(t Type) {
	if t.K == KInt || t.K == KInt64 {
		c.code.Lconst(-1)
	} else {
		c.code.Iconst(-1)
	}
}

func (c *Compiler) narrow(t Type) {
	switch t.K {
	case KInt8:
		c.code.Op(jvm.I2b, 1, jvm.VInt)
	case KInt16:
		c.code.Op(jvm.I2s, 1, jvm.VInt)
	}
}

// operandType determines the common operand type of a binary operation.
func (c *Compiler) operandType(n ast.Node, x, y ast.Expr) Type {
	xn, yn := isNilIdent(c, x), isNilIdent(c, y)
	if xn && yn {
		c.fail(n, "invalid operation: nil == nil")
	}
	if xn {
		return c.typeOf(y)
	}
	if yn {
		return c.typeOf(x)
	}
	tx, ty := c.typeOf(x), c.typeOf(y)
	switch {
	case tx.IsUntyped() && ty.IsUntyped():
		return DefaultType(tx)
	case tx.IsUntyped():
		return ty
	case ty.IsUntyped():
		return tx
	}
	if Identical(tx, ty) {
		return tx
	}
	if tx.IsRef() && ty.IsRef() {
		if c.refAssignable(tx, ty) {
			return ty
		}
		if c.refAssignable(ty, tx) {
			return tx
		}
	}
	c.fail(n, "invalid operation: %s (mismatched types %s and %s)", exprStr(n), tx, ty)
	return Type{}
}

func (c *Compiler) genBinary(e *ast.BinaryExpr, hint *Type) Type {
	switch e.Op {
	case token.LAND, token.LOR, token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
		return c.genBoolValue(e)
	case token.SHL, token.SHR:
		var t Type
		xt := c.typeOf(e.X)
		switch {
		case xt.IsUntyped():
			t = TInt
			if hint != nil && hint.IsInteger() {
				t = *hint
			}
		default:
			t = xt
		}
		if !t.IsInteger() {
			c.fail(e, "invalid operation: shifted operand %s must be integer", exprStr(e.X))
		}
		c.exprTo(e.X, t)
		c.genShiftCount(e.Y)
		c.arithOp(e, e.Op, t)
		return t
	}
	t := c.operandType(e, e.X, e.Y)
	if (e.Op == token.QUO || e.Op == token.REM) && t.IsInteger() {
		if cv := c.constOf(e.Y); cv != nil && constant.Sign(constant.ToInt(cv.v)) == 0 {
			c.fail(e, "invalid operation: division by zero")
		}
	}
	c.exprTo(e.X, t)
	c.exprTo(e.Y, t)
	c.arithOp(e, e.Op, t)
	return t
}

func (c *Compiler) genShiftCount(e ast.Expr) {
	t := c.typeOf(e)
	if cv := c.constOf(e); cv != nil {
		if constant.Sign(constant.ToInt(cv.v)) < 0 {
			c.fail(e, "invalid shift count %s (negative)", exprStr(e))
		}
		c.exprTo(e, TInt32)
		return
	}
	if !t.IsInteger() {
		c.fail(e, "invalid operation: shift count %s must be integer", exprStr(e))
	}
	c.genExpr(e, nil)
	if t.K == KInt || t.K == KInt64 {
		c.code.Op(jvm.L2i, 1, jvm.VInt)
	}
}

// arithOp emits op for the two operands on the stack (shifts: right operand is an int).
func (c *Compiler) arithOp(n ast.Node, op token.Token, t Type) {
	if t.K == KString {
		if op != token.ADD {
			c.fail(n, "operator %s not defined on string", op)
		}
		c.code.Invoke(jvm.Invokevirtual, "java/lang/String", "concat", "(Ljava/lang/String;)Ljava/lang/String;", false)
		return
	}
	if !t.IsNumeric() {
		c.fail(n, "operator %s not defined on %s", op, t)
	}
	cls := t.VT().Class()
	vt := t.VT()
	var base int
	intOnly := true
	switch op {
	case token.ADD:
		base, intOnly = jvm.Iadd, false
	case token.SUB:
		base, intOnly = jvm.Isub, false
	case token.MUL:
		base, intOnly = jvm.Imul, false
	case token.QUO:
		base, intOnly = jvm.Idiv, false
	case token.REM:
		base, intOnly = jvm.Irem, false
	case token.AND:
		base = jvm.Iand
	case token.OR:
		base = jvm.Ior
	case token.XOR:
		base = jvm.Ixor
	case token.AND_NOT:
		c.pushMinusOne(t)
		c.code.Op(byte(jvm.Ixor+cls), 2, vt)
		base = jvm.Iand
	case token.SHL:
		base = jvm.Ishl
	case token.SHR:
		base = jvm.Ishr
	default:
		c.fail(n, "unsupported operator %s", op)
	}
	if intOnly && !t.IsInteger() {
		c.fail(n, "operator %s not defined on %s", op, t)
	}
	c.code.Op(byte(base+cls), 2, vt)
	if op != token.AND && op != token.OR && op != token.SHR {
		c.narrow(t)
	}
}

// ---------------------------------------------------------------------------
// conditions

func (c *Compiler) genBoolValue(e ast.Expr) Type {
	f, end := c.code.NewLabel(), c.code.NewLabel()
	c.genCond(e, f, false)
	c.code.Iconst(1)
	c.code.Jump(jvm.Goto, end)
	c.code.Place(f, false)
	c.code.Iconst(0)
	c.code.Place(end, false)
	return TBool
}

// genCond emits a branch to target taken when e evaluates to jumpIf.
func (c *Compiler) genCond(e ast.Expr, target *jvm.Label, jumpIf bool) {
	e = unparen(e)
	if cv := c.constOf(e); cv != nil {
		if cv.t.K != KUBool && cv.t.K != KBool {
			c.fail(e, "non-boolean condition %s", exprStr(e))
		}
		if constant.BoolVal(cv.v) == jumpIf {
			c.code.Jump(jvm.Goto, target)
		}
		return
	}
	switch x := e.(type) {
	case *ast.UnaryExpr:
		if x.Op == token.NOT {
			c.genCond(x.X, target, !jumpIf)
			return
		}
	case *ast.BinaryExpr:
		switch x.Op {
		case token.LAND:
			if !jumpIf {
				c.genCond(x.X, target, false)
				c.genCond(x.Y, target, false)
			} else {
				skip := c.code.NewLabel()
				c.genCond(x.X, skip, false)
				c.genCond(x.Y, target, true)
				c.code.Place(skip, false)
			}
			return
		case token.LOR:
			if jumpIf {
				c.genCond(x.X, target, true)
				c.genCond(x.Y, target, true)
			} else {
				skip := c.code.NewLabel()
				c.genCond(x.X, skip, true)
				c.genCond(x.Y, target, false)
				c.code.Place(skip, false)
			}
			return
		case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
			c.genCompare(x, target, jumpIf)
			return
		}
	}
	t := c.genExpr(e, nil)
	if t.K != KBool {
		c.fail(e, "non-boolean condition %s (type %s)", exprStr(e), t)
	}
	if jumpIf {
		c.code.Jump(jvm.Ifne, target)
	} else {
		c.code.Jump(jvm.Ifeq, target)
	}
}

func (c *Compiler) genCompare(e *ast.BinaryExpr, target *jvm.Label, jumpIf bool) {
	xn, yn := isNilIdent(c, e.X), isNilIdent(c, e.Y)
	if xn || yn {
		other := e.X
		if xn {
			other = e.Y
		}
		t := c.genExpr(other, nil)
		if !t.IsRef() || t.K == KNil || t.K == KStruct {
			c.fail(e, "invalid operation: %s (mismatched types %s and untyped nil)", exprStr(e), t)
		}
		if e.Op != token.EQL && e.Op != token.NEQ {
			c.fail(e, "operator %s not defined on nil", e.Op)
		}
		isEq := (e.Op == token.EQL) == jumpIf
		if isEq {
			c.code.Jump(jvm.Ifnull, target)
		} else {
			c.code.Jump(jvm.Ifnonnull, target)
		}
		return
	}
	t := c.operandType(e, e.X, e.Y)
	c.exprTo(e.X, t)
	c.exprTo(e.Y, t)
	c.cmpJump(e, t, e.Op, target, jumpIf)
}

func negateCmp(op token.Token) token.Token {
	switch op {
	case token.EQL:
		return token.NEQ
	case token.NEQ:
		return token.EQL
	case token.LSS:
		return token.GEQ
	case token.GEQ:
		return token.LSS
	case token.GTR:
		return token.LEQ
	}
	return token.GTR
}

// cmpJump compares the two operands on the stack and branches to target when
// (x op y) == jumpIf.
func (c *Compiler) cmpJump(n ast.Node, t Type, op token.Token, target *jvm.Label, jumpIf bool) {
	cond := op
	if !jumpIf {
		cond = negateCmp(op)
	}
	idx := map[token.Token]int{token.EQL: 0, token.NEQ: 1, token.LSS: 2, token.GEQ: 3, token.GTR: 4, token.LEQ: 5}[cond]
	eqOnly := op == token.EQL || op == token.NEQ
	switch {
	case t.K == KBool:
		if !eqOnly {
			c.fail(n, "operator %s not defined on bool", op)
		}
		c.code.Jump(byte(jvm.IfIcmpeq+idx), target)
	case t.IsInteger() && t.K != KInt && t.K != KInt64:
		c.code.Jump(byte(jvm.IfIcmpeq+idx), target)
	case t.K == KInt || t.K == KInt64:
		c.code.Op(jvm.Lcmp, 2, jvm.VInt)
		c.code.Jump(byte(jvm.Ifeq+idx), target)
	case t.IsFloat():
		g := op == token.LSS || op == token.LEQ
		var cmp byte
		switch {
		case t.K == KFloat32 && g:
			cmp = jvm.Fcmpg
		case t.K == KFloat32:
			cmp = jvm.Fcmpl
		case g:
			cmp = jvm.Dcmpg
		default:
			cmp = jvm.Dcmpl
		}
		c.code.Op(cmp, 2, jvm.VInt)
		c.code.Jump(byte(jvm.Ifeq+idx), target)
	case t.K == KString:
		if eqOnly {
			c.code.Invoke(jvm.Invokestatic, "java/util/Objects", "equals", "(Ljava/lang/Object;Ljava/lang/Object;)Z", false)
			if cond == token.EQL {
				c.code.Jump(jvm.Ifne, target)
			} else {
				c.code.Jump(jvm.Ifeq, target)
			}
		} else {
			c.code.Invoke(jvm.Invokevirtual, "java/lang/String", "compareTo", "(Ljava/lang/String;)I", false)
			c.code.Jump(byte(jvm.Ifeq+idx), target)
		}
	case t.K == KStruct:
		c.fail(n, "comparing struct values is not supported yet (compare fields, or use pointers)")
	case t.IsRef():
		if !eqOnly {
			c.fail(n, "operator %s not defined on %s", op, t)
		}
		c.code.Jump(byte(jvm.IfAcmpeq+idx), target)
	default:
		c.fail(n, "cannot compare values of type %s", t)
	}
}

// ---------------------------------------------------------------------------
// conversions

func (c *Compiler) genConversion(e *ast.CallExpr, to Type) Type {
	if len(e.Args) != 1 {
		c.fail(e, "conversion to %s needs exactly one argument", to)
	}
	from := c.genExpr(e.Args[0], &to)
	c.convert(e, from, to)
	return to
}

func (c *Compiler) convert(n ast.Node, from, to Type) {
	if from.K == KVoid {
		c.fail(n, "%s (no value) used as value", exprStr(n))
	}
	switch {
	case Identical(from, to):
		return
	case from.IsNumeric() && to.IsNumeric():
		c.numConvert(from, to)
	case from.IsInteger() && to.K == KString:
		if from.K == KInt || from.K == KInt64 {
			c.code.Op(jvm.L2i, 1, jvm.VInt)
		}
		c.code.Invoke(jvm.Invokestatic, "java/lang/Character", "toString", "(I)Ljava/lang/String;", false)
	case from.IsPrim() && to.IsRef():
		c.assign(n, from, to)
	case from.IsRef() && to.IsRef():
		if !c.refAssignable(from, to) {
			c.code.Checkcast(to.RefName())
		}
	default:
		c.fail(n, "cannot convert %s (type %s) to type %s", exprStr(n), from, to)
	}
}

func (c *Compiler) numConvert(from, to Type) {
	fc, tc := from.VT().Class(), to.VT().Class()
	ops := [4][4]byte{
		{0, jvm.I2l, jvm.I2f, jvm.I2d},
		{jvm.L2i, 0, jvm.L2f, jvm.L2d},
		{jvm.F2i, jvm.F2l, 0, jvm.F2d},
		{jvm.D2i, jvm.D2l, jvm.D2f, 0},
	}
	if op := ops[fc][tc]; op != 0 {
		c.code.Op(op, 1, to.VT())
	}
	if tc == 0 && (to.K == KInt8 || to.K == KInt16) && from.K != to.K {
		c.narrow(to)
	}
}
