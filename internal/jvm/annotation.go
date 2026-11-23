package jvm

import "bytes"

// AnnValue is one annotation element value.
type AnnValue struct {
	Tag      byte // Z B C S I J F D (constants), 's' string, 'e' enum, '[' array
	Int      int64
	Float    float64
	Str      string // 's' value; enum constant name for 'e'
	EnumType string // field descriptor of the enum type for 'e'
	Array    []AnnValue
}

// Annotation is a runtime-visible annotation on a class, field or method.
type Annotation struct {
	Type   string // field descriptor, e.g. Lorg/bukkit/event/EventHandler;
	Names  []string
	Values []AnnValue
}

func (cp *ConstPool) writeValue(b *bytes.Buffer, v AnnValue) {
	b.WriteByte(v.Tag)
	switch v.Tag {
	case 'Z', 'B', 'C', 'S', 'I':
		u16(b, cp.Int(int32(v.Int)))
	case 'J':
		u16(b, cp.Long(v.Int))
	case 'F':
		u16(b, cp.Float(float32(v.Float)))
	case 'D':
		u16(b, cp.Double(v.Float))
	case 's':
		u16(b, cp.Utf8(v.Str))
	case 'e':
		u16(b, cp.Utf8(v.EnumType))
		u16(b, cp.Utf8(v.Str))
	case '[':
		u16(b, len(v.Array))
		for _, e := range v.Array {
			cp.writeValue(b, e)
		}
	}
}

// annotationsAttr returns the body of a RuntimeVisibleAnnotations attribute.
func (cp *ConstPool) annotationsAttr(anns []Annotation) []byte {
	var b bytes.Buffer
	u16(&b, len(anns))
	for _, a := range anns {
		u16(&b, cp.Utf8(a.Type))
		u16(&b, len(a.Names))
		for i, n := range a.Names {
			u16(&b, cp.Utf8(n))
			cp.writeValue(&b, a.Values[i])
		}
	}
	return b.Bytes()
}

// attrs writes the attribute table entries for anns (zero or one attribute).
func (cp *ConstPool) annotationAttribute(b *bytes.Buffer, anns []Annotation) int {
	if len(anns) == 0 {
		return 0
	}
	body := cp.annotationsAttr(anns)
	u16(b, cp.Utf8("RuntimeVisibleAnnotations"))
	u32(b, uint32(len(body)))
	b.Write(body)
	return 1
}
