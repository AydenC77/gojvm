package jvm

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"strings"
)

type Field struct {
	Access uint16
	Name   string
	Desc   string
}

type Method struct {
	Access uint16
	Name   string
	Desc   string
	Code   *Code // nil for abstract/native
}

type Class struct {
	Name       string
	Super      string
	Access     uint16
	SourceFile string
	CP         *ConstPool
	Interfaces []string
	Fields     []Field
	Methods    []*Method
}

func NewClass(name, super string) *Class {
	return &Class{Name: name, Super: super, Access: 0x0021, CP: NewConstPool()}
}

func (cl *Class) Bytes() ([]byte, error) {
	cp := cl.CP
	thisIdx := cp.Class(cl.Name)
	superIdx := cp.Class(cl.Super)
	var ifaceIdx []int
	for _, i := range cl.Interfaces {
		ifaceIdx = append(ifaceIdx, cp.Class(i))
	}

	var fields bytes.Buffer
	for _, f := range cl.Fields {
		u16(&fields, int(f.Access))
		u16(&fields, cp.Utf8(f.Name))
		u16(&fields, cp.Utf8(f.Desc))
		u16(&fields, 0)
	}

	var methods bytes.Buffer
	for _, m := range cl.Methods {
		u16(&methods, int(m.Access))
		u16(&methods, cp.Utf8(m.Name))
		u16(&methods, cp.Utf8(m.Desc))
		if m.Code == nil {
			u16(&methods, 0)
			continue
		}
		code, err := m.Code.Finish()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", m.Name, err)
		}
		if len(code) >= 65535 {
			return nil, fmt.Errorf("%s: method too large", m.Name)
		}
		var attrs bytes.Buffer
		nattr := 0
		if len(m.Code.lines) > 0 {
			var lt bytes.Buffer
			u16(&lt, len(m.Code.lines))
			for _, l := range m.Code.lines {
				u16(&lt, l.pc)
				u16(&lt, l.line)
			}
			u16(&attrs, cp.Utf8("LineNumberTable"))
			u32(&attrs, uint32(lt.Len()))
			attrs.Write(lt.Bytes())
			nattr++
		}
		if smt := m.Code.stackMapTable(); smt != nil {
			u16(&attrs, cp.Utf8("StackMapTable"))
			u32(&attrs, uint32(len(smt)))
			attrs.Write(smt)
			nattr++
		}
		var body bytes.Buffer
		u16(&body, m.Code.maxStack)
		u16(&body, m.Code.maxLocals)
		u32(&body, uint32(len(code)))
		body.Write(code)
		u16(&body, 0) // exception table
		u16(&body, nattr)
		body.Write(attrs.Bytes())

		u16(&methods, 1)
		u16(&methods, cp.Utf8("Code"))
		u32(&methods, uint32(body.Len()))
		methods.Write(body.Bytes())
	}

	var extra bytes.Buffer
	nextra := 0
	if cl.SourceFile != "" {
		u16(&extra, cp.Utf8("SourceFile"))
		u32(&extra, 2)
		u16(&extra, cp.Utf8(cl.SourceFile))
		nextra++
	}

	if cp.Count() > 65535 {
		return nil, fmt.Errorf("constant pool too large")
	}
	var out bytes.Buffer
	u32(&out, 0xCAFEBABE)
	u16(&out, 0)
	u16(&out, 65) // Java 21
	u16(&out, cp.Count())
	out.Write(cp.Bytes())
	u16(&out, int(cl.Access))
	u16(&out, thisIdx)
	u16(&out, superIdx)
	u16(&out, len(ifaceIdx))
	for _, i := range ifaceIdx {
		u16(&out, i)
	}
	u16(&out, len(cl.Fields))
	out.Write(fields.Bytes())
	u16(&out, len(cl.Methods))
	out.Write(methods.Bytes())
	u16(&out, nextra)
	out.Write(extra.Bytes())
	return out.Bytes(), nil
}

// WriteJar writes a jar containing the given classes. mainClass may be empty
// (library jar). resources maps jar paths to file contents.
func WriteJar(w io.Writer, mainClass string, classes map[string][]byte, resources map[string][]byte) error {
	zw := zip.NewWriter(w)
	mf, err := zw.Create("META-INF/MANIFEST.MF")
	if err != nil {
		return err
	}
	fmt.Fprintf(mf, "Manifest-Version: 1.0\r\n")
	if mainClass != "" {
		fmt.Fprintf(mf, "Main-Class: %s\r\n", strings.ReplaceAll(mainClass, "/", "."))
	}
	fmt.Fprintf(mf, "Created-By: gojvm\r\n\r\n")
	write := func(name string, data []byte) error {
		f, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		return err
	}
	for name, data := range classes {
		if err := write(name+".class", data); err != nil {
			return err
		}
	}
	for name, data := range resources {
		if err := write(name, data); err != nil {
			return err
		}
	}
	return zw.Close()
}
