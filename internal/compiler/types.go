package compiler

import (
	"fmt"
	"strings"

	"github.com/AydenC77/gojvm/internal/jvm"
)

type Kind int

const (
	KInvalid Kind = iota
	KBool
	KInt8
	KInt16
	KInt32
	KInt
	KInt64
	KFloat32
	KFloat64
	KString
	KClass
	KArray
	KStruct
	KPtr
	KVoid
	KNil
	KUBool
	KUInt
	KURune
	KUFloat
	KUString
)

// Type is a Go-level type. Java classes appear as KClass, Java arrays as KArray.
type Type struct {
	K     Kind
	Class string // internal name for KClass
	Elem  *Type  // for KArray
	JD    string // optional JVM descriptor override (e.g. "C" for Java char)
}

var (
	TBool    = Type{K: KBool}
	TInt8    = Type{K: KInt8}
	TInt16   = Type{K: KInt16}
	TInt32   = Type{K: KInt32}
	TInt     = Type{K: KInt}
	TInt64   = Type{K: KInt64}
	TFloat32 = Type{K: KFloat32}
	TFloat64 = Type{K: KFloat64}
	TString  = Type{K: KString}
	TVoid    = Type{K: KVoid}
	TNil     = Type{K: KNil}
	TUBool   = Type{K: KUBool}
	TUInt    = Type{K: KUInt}
	TURune   = Type{K: KURune}
	TUFloat  = Type{K: KUFloat}
	TUString = Type{K: KUString}
	TObject  = Type{K: KClass, Class: "java/lang/Object"}
)

func ClassType(name string) Type {
	if name == "java/lang/String" {
		return TString
	}
	return Type{K: KClass, Class: name}
}

func ArrayOf(e Type) Type { return Type{K: KArray, Elem: &e} }

func (t Type) IsUntyped() bool { return t.K >= KUBool }
func (t Type) IsInteger() bool { return t.K >= KInt8 && t.K <= KInt64 }
func (t Type) IsFloat() bool   { return t.K == KFloat32 || t.K == KFloat64 }
func (t Type) IsNumeric() bool { return t.IsInteger() || t.IsFloat() }
func (t Type) IsPrim() bool    { return t.K >= KBool && t.K <= KFloat64 }
func (t Type) IsRef() bool {
	return t.K == KString || t.K == KClass || t.K == KArray || t.K == KNil || t.K == KStruct || t.K == KPtr
}

// RefName is the JVM internal name used for checkcast/instanceof of a reference type.
func (t Type) RefName() string {
	switch t.K {
	case KString:
		return "java/lang/String"
	case KClass, KStruct, KPtr:
		return t.Class
	case KArray:
		return t.Desc()
	}
	return "java/lang/Object"
}

func (t Type) Desc() string {
	if t.JD != "" {
		return t.JD
	}
	switch t.K {
	case KBool:
		return "Z"
	case KInt8:
		return "B"
	case KInt16:
		return "S"
	case KInt32:
		return "I"
	case KInt, KInt64:
		return "J"
	case KFloat32:
		return "F"
	case KFloat64:
		return "D"
	case KString:
		return "Ljava/lang/String;"
	case KClass, KStruct, KPtr:
		return "L" + t.Class + ";"
	case KArray:
		return "[" + t.Elem.Desc()
	case KVoid:
		return "V"
	case KNil:
		return "Ljava/lang/Object;"
	}
	panic(fmt.Sprintf("Desc of %v", t.K))
}

func (t Type) VT() jvm.VT {
	if t.K == KNil {
		return jvm.VNull
	}
	return jvm.VTFromDesc(t.Desc())
}

func (t Type) String() string {
	switch t.K {
	case KBool:
		return "bool"
	case KInt8:
		return "int8"
	case KInt16:
		return "int16"
	case KInt32:
		if t.JD == "C" {
			return "char"
		}
		return "int32"
	case KInt:
		return "int"
	case KInt64:
		return "int64"
	case KFloat32:
		return "float32"
	case KFloat64:
		return "float64"
	case KString:
		return "string"
	case KClass:
		if t.Class == "java/lang/Object" {
			return "any"
		}
		return strings.ReplaceAll(t.Class, "/", ".")
	case KArray:
		return "[]" + t.Elem.String()
	case KStruct:
		return t.Class[strings.LastIndex(t.Class, "/")+1:]
	case KPtr:
		return "*" + t.Class[strings.LastIndex(t.Class, "/")+1:]
	case KVoid:
		return "no value"
	case KNil:
		return "untyped nil"
	case KUBool:
		return "untyped bool"
	case KUInt:
		return "untyped int"
	case KURune:
		return "untyped rune"
	case KUFloat:
		return "untyped float"
	case KUString:
		return "untyped string"
	}
	return "invalid type"
}

func Identical(a, b Type) bool {
	if a.K != b.K {
		return false
	}
	switch a.K {
	case KClass, KStruct, KPtr:
		return a.Class == b.Class
	case KArray:
		return Identical(*a.Elem, *b.Elem)
	}
	return true
}

// DefaultType gives the type an untyped constant takes when no context applies.
func DefaultType(t Type) Type {
	switch t.K {
	case KUBool:
		return TBool
	case KUInt:
		return TInt
	case KURune:
		return TInt32
	case KUFloat:
		return TFloat64
	case KUString:
		return TString
	}
	return t
}

// TypeFromDesc converts a JVM field descriptor to a Go-level type.
func TypeFromDesc(d string) Type {
	switch d[0] {
	case 'Z':
		return TBool
	case 'B':
		return TInt8
	case 'S':
		return TInt16
	case 'C':
		return Type{K: KInt32, JD: "C"}
	case 'I':
		return TInt32
	case 'J':
		return TInt64
	case 'F':
		return TFloat32
	case 'D':
		return TFloat64
	case 'V':
		return TVoid
	case 'L':
		return ClassType(d[1 : len(d)-1])
	case '[':
		return ArrayOf(TypeFromDesc(d[1:]))
	}
	panic("bad descriptor " + d)
}

// Boxing information.
type boxInfo struct {
	class, valueDesc, unboxName string
}

func boxOf(t Type) (boxInfo, bool) {
	switch t.K {
	case KBool:
		return boxInfo{"java/lang/Boolean", "Z", "booleanValue"}, true
	case KInt8:
		return boxInfo{"java/lang/Byte", "B", "byteValue"}, true
	case KInt16:
		return boxInfo{"java/lang/Short", "S", "shortValue"}, true
	case KInt32:
		if t.JD == "C" {
			return boxInfo{"java/lang/Character", "C", "charValue"}, true
		}
		return boxInfo{"java/lang/Integer", "I", "intValue"}, true
	case KInt, KInt64:
		return boxInfo{"java/lang/Long", "J", "longValue"}, true
	case KFloat32:
		return boxInfo{"java/lang/Float", "F", "floatValue"}, true
	case KFloat64:
		return boxInfo{"java/lang/Double", "D", "doubleValue"}, true
	}
	return boxInfo{}, false
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}
