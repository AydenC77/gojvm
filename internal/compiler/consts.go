package compiler

import (
	"go/ast"
	"go/constant"
	"go/token"
	"math"
	"strings"
)

type constVal struct {
	v constant.Value
	t Type
}

// math package constants exposed as untyped constants.
var mathConsts = map[string]constVal{
	"Pi":       {constant.MakeFromLiteral("3.14159265358979323846264338327950288419716939937510582097494459", token.FLOAT, 0), TUFloat},
	"E":        {constant.MakeFromLiteral("2.71828182845904523536028747135266249775724709369995957496696763", token.FLOAT, 0), TUFloat},
	"MaxInt32": {constant.MakeInt64(math.MaxInt32), TUInt},
	"MinInt32": {constant.MakeInt64(math.MinInt32), TUInt},
	"MaxInt64": {constant.MakeInt64(math.MaxInt64), TUInt},
	"MinInt64": {constant.MakeInt64(math.MinInt64), TUInt},
	"MaxInt":   {constant.MakeInt64(math.MaxInt64), TUInt},
	"MinInt":   {constant.MakeInt64(math.MinInt64), TUInt},
}

func unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

func untypedRank(k Kind) int {
	switch k {
	case KUInt:
		return 1
	case KURune:
		return 2
	case KUFloat:
		return 3
	}
	return 0
}

// constOf returns the constant value of e, or nil if e is not a constant expression.
func (c *Compiler) constOf(e ast.Expr) *constVal {
	switch e := e.(type) {
	case *ast.ParenExpr:
		return c.constOf(e.X)
	case *ast.BasicLit:
		switch e.Kind {
		case token.INT:
			return &constVal{constant.MakeFromLiteral(e.Value, token.INT, 0), TUInt}
		case token.FLOAT:
			return &constVal{constant.MakeFromLiteral(e.Value, token.FLOAT, 0), TUFloat}
		case token.CHAR:
			return &constVal{constant.MakeFromLiteral(e.Value, token.CHAR, 0), TURune}
		case token.STRING:
			return &constVal{constant.MakeFromLiteral(e.Value, token.STRING, 0), TUString}
		}
		c.fail(e, "unsupported literal %s", e.Value)
	case *ast.Ident:
		if e.Name == "iota" && c.iota >= 0 {
			if o := c.lookup("iota"); o == nil || o.kind == oBuiltin {
				return &constVal{constant.MakeInt64(int64(c.iota)), TUInt}
			}
		}
		if o := c.lookup(e.Name); o != nil && o.kind == oConst {
			return o.cv
		}
	case *ast.SelectorExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			if o := c.lookup(id.Name); o != nil && o.kind == oPkg && o.pkg.intrinsic == "math" {
				if cv, ok := mathConsts[e.Sel.Name]; ok {
					return &cv
				}
			}
		}
	case *ast.UnaryExpr:
		x := c.constOf(e.X)
		if x == nil {
			return nil
		}
		switch e.Op {
		case token.ADD:
			return x
		case token.SUB:
			return &constVal{constant.UnaryOp(token.SUB, x.v, 0), x.t}
		case token.NOT:
			return &constVal{constant.UnaryOp(token.NOT, x.v, 0), x.t}
		case token.XOR:
			return &constVal{constant.UnaryOp(token.XOR, constant.ToInt(x.v), 0), x.t}
		}
	case *ast.BinaryExpr:
		x := c.constOf(e.X)
		if x == nil {
			return nil
		}
		y := c.constOf(e.Y)
		if y == nil {
			return nil
		}
		return c.foldBinary(e, x, y)
	case *ast.CallExpr:
		if len(e.Args) != 1 {
			return nil
		}
		if !c.isTypeExpr(e.Fun) {
			return nil
		}
		t := c.resolveType(e.Fun)
		if !(t.IsPrim() || t.K == KString) {
			return nil
		}
		x := c.constOf(e.Args[0])
		if x == nil {
			return nil
		}
		if t.K == KString && x.t.K != KString && x.t.K != KUString {
			return nil // string(rune) is a runtime conversion here
		}
		v := c.materialize(e, x, t)
		return &constVal{v, t}
	}
	return nil
}

func (c *Compiler) foldBinary(e *ast.BinaryExpr, x, y *constVal) *constVal {
	op := e.Op
	switch op {
	case token.SHL, token.SHR:
		if !(x.t.IsInteger() || x.t.K == KUInt || x.t.K == KURune || (x.t.K == KUFloat && constant.ToInt(x.v).Kind() == constant.Int)) {
			c.fail(e, "invalid shift of %s", x.t)
		}
		s, ok := constant.Uint64Val(constant.ToInt(y.v))
		if !ok || s > 1000 {
			c.fail(e, "invalid shift count")
		}
		t := x.t
		if t.K == KUFloat {
			t = TUInt
		}
		return &constVal{constant.Shift(constant.ToInt(x.v), op, uint(s)), t}
	case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
		xv, yv := x.v, y.v
		if x.t.K == KUFloat || y.t.K == KUFloat || x.t.IsFloat() || y.t.IsFloat() {
			xv, yv = constant.ToFloat(xv), constant.ToFloat(yv)
		}
		return &constVal{constant.MakeBool(constant.Compare(xv, op, yv)), TUBool}
	}
	// result type
	var rt Type
	switch {
	case x.t.IsUntyped() && y.t.IsUntyped():
		rt = x.t
		if untypedRank(y.t.K) > untypedRank(x.t.K) {
			rt = y.t
		}
		if (x.t.K == KUString) != (y.t.K == KUString) || (x.t.K == KUBool) != (y.t.K == KUBool) {
			c.fail(e, "invalid operation: mismatched types %s and %s", x.t, y.t)
		}
	case x.t.IsUntyped():
		rt = y.t
	case y.t.IsUntyped():
		rt = x.t
	default:
		if !Identical(x.t, y.t) {
			c.fail(e, "invalid operation: mismatched types %s and %s", x.t, y.t)
		}
		rt = x.t
	}
	xv, yv := x.v, y.v
	intDiv := false
	switch {
	case rt.IsInteger() || rt.K == KUInt || rt.K == KURune:
		xv, yv = c.materialize(e, x, DefaultOrSelf(rt)), c.materialize(e, y, DefaultOrSelf(rt))
		intDiv = true
	case rt.IsFloat() || rt.K == KUFloat:
		xv, yv = constant.ToFloat(xv), constant.ToFloat(yv)
	}
	var tok token.Token
	switch op {
	case token.ADD, token.SUB, token.MUL, token.REM, token.AND, token.OR, token.XOR, token.AND_NOT, token.LAND, token.LOR:
		tok = op
	case token.QUO:
		tok = op
		if intDiv {
			tok = token.QUO_ASSIGN
		}
	default:
		c.fail(e, "operator %s not defined on constants", op)
	}
	if (op == token.QUO || op == token.REM) && constant.Sign(yv) == 0 {
		c.fail(e, "invalid operation: division by zero")
	}
	if op == token.REM && !intDiv {
		c.fail(e, "invalid operation: operator %% not defined on %s", rt)
	}
	res := constant.BinaryOp(xv, tok, yv)
	if rt.K == KString || rt.K == KUString {
		if op != token.ADD {
			c.fail(e, "operator %s not defined on strings", op)
		}
	}
	if !rt.IsUntyped() {
		res = c.materialize(e, &constVal{res, rt}, rt)
	}
	return &constVal{res, rt}
}

func DefaultOrSelf(t Type) Type {
	if t.IsUntyped() {
		return DefaultType(t)
	}
	return t
}

// materialize checks that cv is representable as t and returns the converted value.
func (c *Compiler) materialize(n ast.Node, cv *constVal, t Type) constant.Value {
	v := cv.v
	switch {
	case t.K == KBool:
		if v.Kind() != constant.Bool {
			c.fail(n, "cannot use %s constant as bool", cv.t)
		}
		return v
	case t.K == KString:
		if v.Kind() != constant.String {
			c.fail(n, "cannot use %s constant as string", cv.t)
		}
		return v
	case t.IsInteger():
		iv := constant.ToInt(v)
		if iv.Kind() != constant.Int {
			if v.Kind() == constant.Float {
				c.fail(n, "cannot use %s (untyped float constant) as %s value (truncated)", exprStr(n), t)
			}
			c.fail(n, "cannot use %s constant as %s value", cv.t, t)
		}
		var lo, hi int64
		switch t.K {
		case KInt8:
			lo, hi = math.MinInt8, math.MaxInt8
		case KInt16:
			lo, hi = math.MinInt16, math.MaxInt16
		case KInt32:
			lo, hi = math.MinInt32, math.MaxInt32
		default:
			lo, hi = math.MinInt64, math.MaxInt64
		}
		i64, exact := constant.Int64Val(iv)
		if !exact || i64 < lo || i64 > hi {
			c.fail(n, "cannot use %s (untyped constant %s) as %s value (overflows)", exprStr(n), iv.ExactString(), t)
		}
		return iv
	case t.IsFloat():
		fv := constant.ToFloat(v)
		if fv.Kind() != constant.Float && fv.Kind() != constant.Int {
			c.fail(n, "cannot use %s constant as %s value", cv.t, t)
		}
		f, _ := constant.Float64Val(fv)
		if math.IsInf(f, 0) {
			c.fail(n, "constant overflows %s", t)
		}
		if t.K == KFloat32 {
			return constant.MakeFloat64(float64(float32(f)))
		}
		return constant.MakeFloat64(f)
	}
	c.fail(n, "cannot use %s constant as %s value", cv.t, t)
	return nil
}

// constFits reports whether an untyped constant can be represented as t without error.
func (c *Compiler) constFits(cv *constVal, t Type) bool {
	v := cv.v
	switch {
	case t.K == KBool:
		return v.Kind() == constant.Bool
	case t.K == KString:
		return v.Kind() == constant.String
	case t.IsInteger():
		iv := constant.ToInt(v)
		if iv.Kind() != constant.Int {
			return false
		}
		i64, exact := constant.Int64Val(iv)
		if !exact {
			return false
		}
		switch t.K {
		case KInt8:
			return i64 >= math.MinInt8 && i64 <= math.MaxInt8
		case KInt16:
			return i64 >= math.MinInt16 && i64 <= math.MaxInt16
		case KInt32:
			return i64 >= math.MinInt32 && i64 <= math.MaxInt32
		}
		return true
	case t.IsFloat():
		return v.Kind() == constant.Int || v.Kind() == constant.Float
	}
	return false
}

func exprStr(n ast.Node) string {
	switch e := n.(type) {
	case *ast.BasicLit:
		return e.Value
	case *ast.Ident:
		return e.Name
	case *ast.UnaryExpr:
		return e.Op.String() + exprStr(e.X)
	case *ast.BinaryExpr:
		return exprStr(e.X) + " " + e.Op.String() + " " + exprStr(e.Y)
	case *ast.ParenExpr:
		return "(" + exprStr(e.X) + ")"
	case *ast.SelectorExpr:
		return exprStr(e.X) + "." + e.Sel.Name
	case *ast.CallExpr:
		var a []string
		for _, x := range e.Args {
			a = append(a, exprStr(x))
		}
		return exprStr(e.Fun) + "(" + strings.Join(a, ", ") + ")"
	}
	return "expression"
}

// emitConst pushes constant cv converted to type t.
func (c *Compiler) emitConst(n ast.Node, cv *constVal, t Type) {
	v := c.materialize(n, cv, t)
	code := c.code
	switch {
	case t.K == KBool:
		if constant.BoolVal(v) {
			code.Iconst(1)
		} else {
			code.Iconst(0)
		}
	case t.K == KString:
		code.Sconst(constant.StringVal(v))
	case t.K == KInt, t.K == KInt64:
		i, _ := constant.Int64Val(v)
		code.Lconst(i)
	case t.IsInteger():
		i, _ := constant.Int64Val(v)
		code.Iconst(int32(i))
	case t.K == KFloat32:
		f, _ := constant.Float64Val(v)
		code.Fconst(float32(f))
	case t.K == KFloat64:
		f, _ := constant.Float64Val(v)
		code.Dconst(f)
	default:
		c.fail(n, "cannot use constant as %s", t)
	}
}

func constString(cv *constVal) string { return constant.StringVal(cv.v) }
