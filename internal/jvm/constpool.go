// Package jvm contains a self-contained JVM classfile writer (constant pool,
// bytecode emitter with StackMapTable generation) and a classfile/jimage reader.
package jvm

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"unicode/utf16"
)

// ConstPool is a JVM constant pool builder with de-duplication.
type ConstPool struct {
	buf bytes.Buffer
	n   int
	idx map[string]int
}

func NewConstPool() *ConstPool {
	return &ConstPool{n: 1, idx: map[string]int{}}
}

func (c *ConstPool) add(key string, slots int, write func(b *bytes.Buffer)) int {
	if i, ok := c.idx[key]; ok {
		return i
	}
	i := c.n
	write(&c.buf)
	c.n += slots
	c.idx[key] = i
	return i
}

func u16(b *bytes.Buffer, v int)    { binary.Write(b, binary.BigEndian, uint16(v)) }
func u32(b *bytes.Buffer, v uint32) { binary.Write(b, binary.BigEndian, v) }

// modifiedUTF8 encodes s the way the JVM expects in CONSTANT_Utf8.
func modifiedUTF8(s string) []byte {
	var out []byte
	for _, u := range utf16.Encode([]rune(s)) {
		switch {
		case u != 0 && u < 0x80:
			out = append(out, byte(u))
		case u < 0x800:
			out = append(out, 0xC0|byte(u>>6), 0x80|byte(u&0x3F))
		default:
			out = append(out, 0xE0|byte(u>>12), 0x80|byte((u>>6)&0x3F), 0x80|byte(u&0x3F))
		}
	}
	return out
}

func (c *ConstPool) Utf8(s string) int {
	return c.add("u:"+s, 1, func(b *bytes.Buffer) {
		b.WriteByte(1)
		enc := modifiedUTF8(s)
		u16(b, len(enc))
		b.Write(enc)
	})
}

func (c *ConstPool) Class(name string) int {
	n := c.Utf8(name)
	return c.add("c:"+name, 1, func(b *bytes.Buffer) { b.WriteByte(7); u16(b, n) })
}

func (c *ConstPool) String(s string) int {
	n := c.Utf8(s)
	return c.add("s:"+s, 1, func(b *bytes.Buffer) { b.WriteByte(8); u16(b, n) })
}

func (c *ConstPool) Int(v int32) int {
	return c.add(fmt.Sprintf("i:%d", v), 1, func(b *bytes.Buffer) { b.WriteByte(3); u32(b, uint32(v)) })
}

func (c *ConstPool) Float(v float32) int {
	return c.add(fmt.Sprintf("f:%x", math.Float32bits(v)), 1, func(b *bytes.Buffer) { b.WriteByte(4); u32(b, math.Float32bits(v)) })
}

func (c *ConstPool) Long(v int64) int {
	return c.add(fmt.Sprintf("l:%d", v), 2, func(b *bytes.Buffer) {
		b.WriteByte(5)
		binary.Write(b, binary.BigEndian, uint64(v))
	})
}

func (c *ConstPool) Double(v float64) int {
	return c.add(fmt.Sprintf("d:%x", math.Float64bits(v)), 2, func(b *bytes.Buffer) {
		b.WriteByte(6)
		binary.Write(b, binary.BigEndian, math.Float64bits(v))
	})
}

func (c *ConstPool) NameAndType(name, desc string) int {
	n, d := c.Utf8(name), c.Utf8(desc)
	return c.add("nt:"+name+":"+desc, 1, func(b *bytes.Buffer) { b.WriteByte(12); u16(b, n); u16(b, d) })
}

func (c *ConstPool) ref(tag byte, owner, name, desc string) int {
	cl, nt := c.Class(owner), c.NameAndType(name, desc)
	return c.add(fmt.Sprintf("r%d:%s:%s:%s", tag, owner, name, desc), 1, func(b *bytes.Buffer) {
		b.WriteByte(tag)
		u16(b, cl)
		u16(b, nt)
	})
}

func (c *ConstPool) Field(owner, name, desc string) int  { return c.ref(9, owner, name, desc) }
func (c *ConstPool) Method(owner, name, desc string) int { return c.ref(10, owner, name, desc) }
func (c *ConstPool) IMethod(owner, name, desc string) int {
	return c.ref(11, owner, name, desc)
}

func (c *ConstPool) Count() int    { return c.n }
func (c *ConstPool) Bytes() []byte { return c.buf.Bytes() }
