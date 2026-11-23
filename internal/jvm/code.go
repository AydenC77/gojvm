package jvm

import (
	"bytes"
	"fmt"
	"math"
)

// Opcodes used by the compiler.
const (
	AconstNull      = 1
	Iconst0         = 3
	Lconst0         = 9
	Fconst0         = 11
	Dconst0         = 14
	Bipush          = 16
	Sipush          = 17
	Ldc             = 18
	LdcW            = 19
	Ldc2W           = 20
	Iload           = 21
	Istore          = 54
	Iaload          = 46
	Iastore         = 79
	Pop             = 87
	Pop2            = 88
	Dup             = 89
	Dup2            = 92
	Swap            = 95
	Iadd            = 96
	Isub            = 100
	Imul            = 104
	Idiv            = 108
	Irem            = 112
	Ineg            = 116
	Ishl            = 120
	Ishr            = 122
	Iand            = 126
	Ior             = 128
	Ixor            = 130
	I2l             = 133
	I2f             = 134
	I2d             = 135
	L2i             = 136
	L2f             = 137
	L2d             = 138
	F2i             = 139
	F2l             = 140
	F2d             = 141
	D2i             = 142
	D2l             = 143
	D2f             = 144
	I2b             = 145
	I2c             = 146
	I2s             = 147
	Lcmp            = 148
	Fcmpl           = 149
	Fcmpg           = 150
	Dcmpl           = 151
	Dcmpg           = 152
	Ifeq            = 153
	Ifne            = 154
	Iflt            = 155
	Ifge            = 156
	Ifgt            = 157
	Ifle            = 158
	IfIcmpeq        = 159
	IfIcmpne        = 160
	IfIcmplt        = 161
	IfIcmpge        = 162
	IfIcmpgt        = 163
	IfIcmple        = 164
	IfAcmpeq        = 165
	IfAcmpne        = 166
	Goto            = 167
	Getstatic       = 178
	Putstatic       = 179
	Getfield        = 180
	Putfield        = 181
	Invokevirtual   = 182
	Invokespecial   = 183
	Invokestatic    = 184
	Invokeinterface = 185
	New             = 187
	Newarray        = 188
	Anewarray       = 189
	Arraylength     = 190
	Athrow          = 191
	Checkcast       = 192
	Instanceof      = 193
	Ifnull          = 198
	Ifnonnull       = 199
)

// VT is a JVM verification type.
type VT struct {
	Tag  byte // 0 top, 1 int, 2 float, 3 double, 4 long, 5 null, 7 object, 8 uninit, 255 continuation
	Name string
	Off  int
}

var (
	VTop    = VT{Tag: 0}
	VInt    = VT{Tag: 1}
	VFloat  = VT{Tag: 2}
	VDouble = VT{Tag: 3}
	VLong   = VT{Tag: 4}
	VNull   = VT{Tag: 5}
	vCont   = VT{Tag: 255}
)

func VObj(name string) VT { return VT{Tag: 7, Name: name} }

func (v VT) Size() int {
	if v.Tag == 3 || v.Tag == 4 {
		return 2
	}
	return 1
}

// Class returns the opcode offset class: 0=int 1=long 2=float 3=double 4=ref.
func (v VT) Class() int {
	switch v.Tag {
	case 1:
		return 0
	case 4:
		return 1
	case 2:
		return 2
	case 3:
		return 3
	}
	return 4
}

// VTFromDesc converts a field descriptor to a verification type.
func VTFromDesc(d string) VT {
	switch d[0] {
	case 'Z', 'B', 'C', 'S', 'I':
		return VInt
	case 'J':
		return VLong
	case 'F':
		return VFloat
	case 'D':
		return VDouble
	case 'L':
		return VObj(d[1 : len(d)-1])
	case '[':
		return VObj(d)
	}
	panic("bad descriptor " + d)
}

// SplitDesc splits a method descriptor into parameter descriptors and the return descriptor.
func SplitDesc(desc string) (params []string, ret string) {
	i := 1
	for desc[i] != ')' {
		j := i
		for desc[j] == '[' {
			j++
		}
		if desc[j] == 'L' {
			for desc[j] != ';' {
				j++
			}
		}
		params = append(params, desc[i:j+1])
		i = j + 1
	}
	return params, desc[i+1:]
}

type Label struct {
	pc     int
	placed bool
	jumped bool
	stack  []VT
}

type patch struct {
	opPos int
	at    int
	l     *Label
}

type frame struct {
	locals []VT
	stack  []VT
}

type lineEntry struct{ pc, line int }

// Code is a bytecode emitter that simulates the operand stack and locals so
// StackMapTable frames can be generated.
type Code struct {
	cp        *ConstPool
	buf       []byte
	stack     []VT
	locals    []VT
	maxStack  int
	maxLocals int
	frames    map[int]frame
	frameKeys []int
	patches   []patch
	lines     []lineEntry
	dead      bool
	err       error
}

func NewCode(cp *ConstPool, params []VT) *Code {
	c := &Code{cp: cp, frames: map[int]frame{}}
	for _, p := range params {
		c.NewLocal(p)
	}
	return c
}

func (c *Code) Err() error { return c.err }
func (c *Code) Dead() bool { return c.dead }
func (c *Code) PC() int    { return len(c.buf) }

func (c *Code) fail(format string, a ...any) {
	if c.err == nil {
		c.err = fmt.Errorf(format, a...)
	}
}

// State is a snapshot used to roll back dry-run emission.
type State struct {
	n       int
	stack   []VT
	locals  []VT
	dead    bool
	patches int
	lines   int
	fkeys   int
	maxS    int
	maxL    int
}

func (c *Code) Save() State {
	return State{len(c.buf), append([]VT(nil), c.stack...), append([]VT(nil), c.locals...), c.dead,
		len(c.patches), len(c.lines), len(c.frameKeys), c.maxStack, c.maxLocals}
}

func (c *Code) Restore(s State) {
	c.buf = c.buf[:s.n]
	c.stack = append(c.stack[:0], s.stack...)
	c.locals = append(c.locals[:0], s.locals...)
	c.dead = s.dead
	c.patches = c.patches[:s.patches]
	c.lines = c.lines[:s.lines]
	for _, k := range c.frameKeys[s.fkeys:] {
		delete(c.frames, k)
	}
	c.frameKeys = c.frameKeys[:s.fkeys]
	c.maxStack, c.maxLocals = s.maxS, s.maxL
}

func (c *Code) stackSize() int {
	n := 0
	for _, v := range c.stack {
		n += v.Size()
	}
	return n
}

func (c *Code) push(v VT) {
	c.stack = append(c.stack, v)
	if n := c.stackSize(); n > c.maxStack {
		c.maxStack = n
	}
}

func (c *Code) pop(n int) {
	if len(c.stack) < n {
		c.fail("internal error: operand stack underflow")
		c.stack = c.stack[:0]
		return
	}
	c.stack = c.stack[:len(c.stack)-n]
}

func (c *Code) b(v ...byte) { c.buf = append(c.buf, v...) }
func (c *Code) w(v int)     { c.buf = append(c.buf, byte(v>>8), byte(v)) }

// Op emits a simple instruction popping n entries and pushing the given types.
func (c *Code) Op(op byte, pop int, push ...VT) {
	if c.dead {
		return
	}
	c.b(op)
	c.pop(pop)
	for _, p := range push {
		c.push(p)
	}
}

func (c *Code) Line(n int) {
	if c.dead || n <= 0 {
		return
	}
	if l := len(c.lines); l > 0 && c.lines[l-1].line == n {
		return
	}
	c.lines = append(c.lines, lineEntry{len(c.buf), n})
}

func (c *Code) Null() {
	if c.dead {
		return
	}
	c.b(AconstNull)
	c.push(VNull)
}

func (c *Code) Iconst(v int32) {
	if c.dead {
		return
	}
	switch {
	case v >= -1 && v <= 5:
		c.b(byte(Iconst0 + v))
	case v >= -128 && v <= 127:
		c.b(Bipush, byte(v))
	case v >= -32768 && v <= 32767:
		c.b(Sipush, byte(v>>8), byte(v))
	default:
		c.ldc(c.cp.Int(v))
	}
	c.push(VInt)
}

func (c *Code) ldc(idx int) {
	if idx < 256 {
		c.b(Ldc, byte(idx))
	} else {
		c.b(LdcW)
		c.w(idx)
	}
}

func (c *Code) Lconst(v int64) {
	if c.dead {
		return
	}
	if v == 0 || v == 1 {
		c.b(byte(Lconst0 + v))
	} else {
		c.b(Ldc2W)
		c.w(c.cp.Long(v))
	}
	c.push(VLong)
}

func (c *Code) Fconst(v float32) {
	if c.dead {
		return
	}
	if (v == 0 && !math.Signbit(float64(v))) || v == 1 || v == 2 {
		c.b(byte(Fconst0 + int(v)))
	} else {
		c.ldc(c.cp.Float(v))
	}
	c.push(VFloat)
}

func (c *Code) Dconst(v float64) {
	if c.dead {
		return
	}
	if (v == 0 && !math.Signbit(v)) || v == 1 {
		c.b(byte(Dconst0 + int(v)))
	} else {
		c.b(Ldc2W)
		c.w(c.cp.Double(v))
	}
	c.push(VDouble)
}

func (c *Code) Sconst(s string) {
	if c.dead {
		return
	}
	c.ldc(c.cp.String(s))
	c.push(VObj("java/lang/String"))
}

// NewLocal declares a new local variable slot (works even in dead code so scoping stays correct).
func (c *Code) NewLocal(v VT) int {
	slot := len(c.locals)
	c.locals = append(c.locals, v)
	if v.Size() == 2 {
		c.locals = append(c.locals, vCont)
	}
	if len(c.locals) > c.maxLocals {
		c.maxLocals = len(c.locals)
	}
	return slot
}

func (c *Code) NumLocals() int { return len(c.locals) }
func (c *Code) Truncate(n int) {
	if n < len(c.locals) {
		c.locals = c.locals[:n]
	}
}

func (c *Code) localOp(base byte, slot int, v VT) {
	op := base + byte(v.Class())
	if slot < 256 {
		c.b(op, byte(slot))
	} else {
		c.b(196, op)
		c.w(slot)
	}
}

func (c *Code) Load(slot int, v VT) {
	if c.dead {
		return
	}
	c.localOp(Iload, slot, v)
	c.push(v)
}

// Store pops the top of stack into slot. The declared type of the slot is in the locals table.
func (c *Code) Store(slot int, v VT) {
	if c.dead {
		return
	}
	c.localOp(Istore, slot, v)
	c.pop(1)
}

func (c *Code) Dup() {
	if c.dead {
		return
	}
	if len(c.stack) == 0 {
		c.fail("internal error: dup on empty stack")
		return
	}
	t := c.stack[len(c.stack)-1]
	if t.Size() == 2 {
		c.b(Dup2)
	} else {
		c.b(Dup)
	}
	c.push(t)
}

// Dup2 duplicates the top two single-slot entries.
func (c *Code) Dup2() {
	if c.dead {
		return
	}
	n := len(c.stack)
	if n < 2 {
		c.fail("internal error: dup2 on short stack")
		return
	}
	a, b := c.stack[n-2], c.stack[n-1]
	c.b(Dup2)
	c.push(a)
	c.push(b)
}

func (c *Code) Pop() {
	if c.dead {
		return
	}
	if len(c.stack) == 0 {
		c.fail("internal error: pop on empty stack")
		return
	}
	if c.stack[len(c.stack)-1].Size() == 2 {
		c.b(Pop2)
	} else {
		c.b(Pop)
	}
	c.pop(1)
}

func (c *Code) Field(op byte, owner, name, desc string) {
	if c.dead {
		return
	}
	c.b(op)
	c.w(c.cp.Field(owner, name, desc))
	t := VTFromDesc(desc)
	switch op {
	case Getstatic:
		c.push(t)
	case Putstatic:
		c.pop(1)
	case Getfield:
		c.pop(1)
		c.push(t)
	case Putfield:
		c.pop(2)
	}
}

func (c *Code) Invoke(op byte, owner, name, desc string, iface bool) {
	if c.dead {
		return
	}
	params, ret := SplitDesc(desc)
	n := len(params)
	slots := 0
	for _, p := range params {
		slots += VTFromDesc(p).Size()
	}
	if op != Invokestatic {
		n++
		slots++
	}
	switch op {
	case Invokeinterface:
		c.b(op)
		c.w(c.cp.IMethod(owner, name, desc))
		c.b(byte(slots), 0)
	default:
		c.b(op)
		if iface {
			c.w(c.cp.IMethod(owner, name, desc))
		} else {
			c.w(c.cp.Method(owner, name, desc))
		}
	}
	var recv VT
	if op == Invokespecial && name == "<init>" && len(c.stack) >= n {
		recv = c.stack[len(c.stack)-n]
	}
	c.pop(n)
	if recv.Tag == 8 {
		obj := VObj(owner)
		for i, v := range c.stack {
			if v == recv {
				c.stack[i] = obj
			}
		}
		for i, v := range c.locals {
			if v == recv {
				c.locals[i] = obj
			}
		}
	}
	if ret != "V" {
		c.push(VTFromDesc(ret))
	}
}

func (c *Code) New(class string) {
	if c.dead {
		return
	}
	off := len(c.buf)
	c.b(New)
	c.w(c.cp.Class(class))
	c.push(VT{Tag: 8, Off: off})
}

func (c *Code) Anewarray(class string) {
	if c.dead {
		return
	}
	c.b(Anewarray)
	c.w(c.cp.Class(class))
	c.pop(1)
	if class[0] == '[' {
		c.push(VObj("[" + class))
	} else {
		c.push(VObj("[L" + class + ";"))
	}
}

// NewPrimArray emits newarray with the given JVM atype code and pushes the array type desc.
func (c *Code) NewPrimArray(atype byte, desc string) {
	if c.dead {
		return
	}
	c.b(Newarray, atype)
	c.pop(1)
	c.push(VObj(desc))
}

func (c *Code) Checkcast(class string) {
	if c.dead {
		return
	}
	c.b(Checkcast)
	c.w(c.cp.Class(class))
	c.pop(1)
	c.push(VObj(class))
}

func (c *Code) Instanceof(class string) {
	if c.dead {
		return
	}
	c.b(Instanceof)
	c.w(c.cp.Class(class))
	c.pop(1)
	c.push(VInt)
}

func (c *Code) Athrow() {
	if c.dead {
		return
	}
	c.b(Athrow)
	c.dead = true
}

// Return emits a return instruction; v is nil for void.
func (c *Code) Return(v *VT) {
	if c.dead {
		return
	}
	if v == nil {
		c.b(177)
	} else {
		c.b(byte(172 + v.Class()))
	}
	c.dead = true
}

func (c *Code) NewLabel() *Label { return &Label{pc: -1} }

func (c *Code) snapshot() frame {
	return frame{append([]VT(nil), c.locals...), append([]VT(nil), c.stack...)}
}

// Jump emits a branch instruction to l.
func (c *Code) Jump(op byte, l *Label) {
	if c.dead {
		return
	}
	switch {
	case op == Goto:
	case op >= Ifeq && op <= Ifle, op == Ifnull, op == Ifnonnull:
		c.pop(1)
	default:
		c.pop(2)
	}
	if !l.jumped && !l.placed {
		l.stack = append([]VT(nil), c.stack...)
	}
	l.jumped = true
	pos := len(c.buf)
	c.b(op)
	if l.placed {
		off := l.pc - pos
		if off < -32768 {
			c.fail("method too large (branch out of range)")
		}
		c.w(off)
	} else {
		c.patches = append(c.patches, patch{pos, len(c.buf), l})
		c.w(0)
	}
	if op == Goto {
		c.dead = true
	}
}

// Place binds l to the current position. loop marks a backward-branch target.
func (c *Code) Place(l *Label, loop bool) {
	if c.dead && !l.jumped {
		// unreachable and nobody jumps here: stay dead. A loop head that is dead stays dead.
		l.placed = true
		l.pc = len(c.buf)
		return
	}
	if c.dead {
		c.stack = append(c.stack[:0], l.stack...)
		c.dead = false
	}
	l.placed = true
	l.pc = len(c.buf)
	if _, ok := c.frames[l.pc]; !ok {
		c.frameKeys = append(c.frameKeys, l.pc)
	}
	c.frames[l.pc] = c.snapshot()
}

func (c *Code) Finish() ([]byte, error) {
	if c.err != nil {
		return nil, c.err
	}
	for _, p := range c.patches {
		if !p.l.placed {
			return nil, fmt.Errorf("internal error: unplaced label")
		}
		off := p.l.pc - p.opPos
		if off > 32767 || off < -32768 {
			return nil, fmt.Errorf("method too large (branch out of range)")
		}
		c.buf[p.at] = byte(off >> 8)
		c.buf[p.at+1] = byte(off)
	}
	return c.buf, nil
}

func (c *Code) writeVT(b *bytes.Buffer, v VT) {
	b.WriteByte(v.Tag)
	switch v.Tag {
	case 7:
		u16(b, c.cp.Class(v.Name))
	case 8:
		u16(b, v.Off)
	}
}

// stackMapTable serializes frames as full_frame entries; returns nil if none.
func (c *Code) stackMapTable() []byte {
	if len(c.frames) == 0 {
		return nil
	}
	keys := make([]int, 0, len(c.frames))
	for k := range c.frames {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	var b bytes.Buffer
	u16(&b, len(keys))
	prev := -1
	for _, k := range keys {
		f := c.frames[k]
		b.WriteByte(255)
		u16(&b, k-prev-1)
		prev = k
		var locs []VT
		for _, v := range f.locals {
			if v.Tag != 255 {
				locs = append(locs, v)
			}
		}
		for len(locs) > 0 && locs[len(locs)-1].Tag == 0 {
			locs = locs[:len(locs)-1]
		}
		u16(&b, len(locs))
		for _, v := range locs {
			c.writeVT(&b, v)
		}
		u16(&b, len(f.stack))
		for _, v := range f.stack {
			c.writeVT(&b, v)
		}
	}
	return b.Bytes()
}

// ResetStack discards the simulated operand stack and revives the emitter. It
// is used after a compile error so later statements can still be analysed.
func (c *Code) ResetStack() {
	c.stack = c.stack[:0]
	c.dead = false
}
