package jvm

import (
	"archive/zip"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	AccPublic    = 0x0001
	AccStatic    = 0x0008
	AccFinal     = 0x0010
	AccVarargs   = 0x0080
	AccInterface = 0x0200
	AccAbstract  = 0x0400
	AccSynthetic = 0x1000
	AccBridge    = 0x0040
)

type Member struct {
	Access uint16
	Name   string
	Desc   string
}

type ClassInfo struct {
	Name       string
	Super      string
	Access     uint16
	Interfaces []string
	Fields     []Member
	Methods    []Member
}

func (c *ClassInfo) IsInterface() bool { return c.Access&AccInterface != 0 }

// ParseClass parses the parts of a classfile the compiler needs.
func ParseClass(b []byte) (*ClassInfo, error) {
	r := &rd{b: b}
	if r.u4() != 0xCAFEBABE {
		return nil, fmt.Errorf("bad classfile magic")
	}
	r.u2()
	r.u2()
	n := int(r.u2())
	utf := make([]string, n)
	cls := make([]uint16, n)
	for i := 1; i < n; i++ {
		tag := r.u1()
		switch tag {
		case 1:
			l := int(r.u2())
			utf[i] = string(r.bytes(l))
		case 3, 4, 9, 10, 11, 12, 17, 18:
			r.skip(4)
		case 5, 6:
			r.skip(8)
			i++
		case 7:
			cls[i] = r.u2()
		case 8, 16, 19, 20:
			r.skip(2)
		case 15:
			r.skip(3)
		default:
			return nil, fmt.Errorf("bad constant pool tag %d", tag)
		}
	}
	ci := &ClassInfo{}
	ci.Access = r.u2()
	ci.Name = utf[cls[r.u2()]]
	if s := r.u2(); s != 0 {
		ci.Super = utf[cls[s]]
	}
	for i, k := 0, int(r.u2()); i < k; i++ {
		ci.Interfaces = append(ci.Interfaces, utf[cls[r.u2()]])
	}
	members := func() []Member {
		var out []Member
		for i, k := 0, int(r.u2()); i < k; i++ {
			m := Member{Access: r.u2()}
			m.Name = utf[r.u2()]
			m.Desc = utf[r.u2()]
			for j, na := 0, int(r.u2()); j < na; j++ {
				r.u2()
				r.skip(int(r.u4()))
			}
			out = append(out, m)
		}
		return out
	}
	ci.Fields = members()
	ci.Methods = members()
	return ci, r.err
}

type rd struct {
	b   []byte
	p   int
	err error
}

func (r *rd) bytes(n int) []byte {
	if r.p+n > len(r.b) {
		r.err = fmt.Errorf("truncated classfile")
		r.p = len(r.b)
		return make([]byte, n)
	}
	v := r.b[r.p : r.p+n]
	r.p += n
	return v
}
func (r *rd) skip(n int) { r.bytes(n) }
func (r *rd) u1() byte   { return r.bytes(1)[0] }
func (r *rd) u2() uint16 { return binary.BigEndian.Uint16(r.bytes(2)) }
func (r *rd) u4() uint32 { return binary.BigEndian.Uint32(r.bytes(4)) }

// ---------------------------------------------------------------------------
// jimage (lib/modules) reader

type resLoc struct{ offset, size uint64 }

type JImage struct {
	f         *os.File
	indexSize int64
	classes   map[string]resLoc
	packages  map[string]bool
}

func OpenJImage(path string) (*JImage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	hdr := make([]byte, 28)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		f.Close()
		return nil, err
	}
	le := binary.LittleEndian
	if le.Uint32(hdr) != 0xCAFEDADA {
		f.Close()
		return nil, fmt.Errorf("%s: not a jimage file", path)
	}
	tl := int(le.Uint32(hdr[16:]))
	locSize := int(le.Uint32(hdr[20:]))
	strSize := int(le.Uint32(hdr[24:]))
	idxLen := 28 + tl*8 + locSize + strSize
	idx := make([]byte, idxLen)
	if _, err := f.ReadAt(idx, 0); err != nil {
		f.Close()
		return nil, err
	}
	offs := idx[28+tl*4:]
	locs := idx[28+tl*8 : 28+tl*8+locSize]
	strs := idx[28+tl*8+locSize:]
	getStr := func(o uint64) string {
		if int(o) >= len(strs) {
			return ""
		}
		e := int(o)
		for e < len(strs) && strs[e] != 0 {
			e++
		}
		return string(strs[o:e])
	}
	ji := &JImage{f: f, indexSize: int64(idxLen), classes: map[string]resLoc{}, packages: map[string]bool{}}
	for i := 0; i < tl; i++ {
		p := int(le.Uint32(offs[i*4:]))
		if p >= len(locs) {
			continue
		}
		var attr [8]uint64
		for p < len(locs) {
			bt := locs[p]
			kind := bt >> 3
			if kind == 0 {
				break
			}
			ln := int(bt&7) + 1
			var v uint64
			for j := 0; j < ln; j++ {
				v = v<<8 | uint64(locs[p+1+j])
			}
			if kind < 8 {
				attr[kind] = v
			}
			p += 1 + ln
		}
		if getStr(attr[4]) != "class" {
			continue
		}
		parent, base := getStr(attr[2]), getStr(attr[3])
		name := base
		if parent != "" {
			name = parent + "/" + base
			ji.packages[parent] = true
		}
		if _, dup := ji.classes[name]; !dup {
			ji.classes[name] = resLoc{attr[5], attr[7]}
			if attr[6] != 0 {
				f.Close()
				return nil, fmt.Errorf("%s: compressed jimage resources are not supported", path)
			}
		}
	}
	return ji, nil
}

func (j *JImage) Close() { j.f.Close() }

func (j *JImage) Read(name string) ([]byte, bool) {
	loc, ok := j.classes[name]
	if !ok {
		return nil, false
	}
	buf := make([]byte, loc.size)
	if _, err := j.f.ReadAt(buf, j.indexSize+int64(loc.offset)); err != nil {
		return nil, false
	}
	return buf, true
}

func (j *JImage) HasPackage(p string) bool { return j.packages[p] }

// ClassesIn lists simple class names in a package (used for diagnostics).
func (j *JImage) ClassesIn(pkg string) []string {
	var out []string
	for n := range j.classes {
		if strings.HasPrefix(n, pkg+"/") && !strings.Contains(n[len(pkg)+1:], "/") {
			out = append(out, n[len(pkg)+1:])
		}
	}
	sort.Strings(out)
	return out
}

// FindJDK locates a Java 21+ JDK home containing lib/modules.
func FindJDK(explicit string) (string, error) {
	var cands []string
	if explicit != "" {
		cands = append(cands, explicit)
	}
	if h := os.Getenv("GOJVM_JDK"); h != "" {
		cands = append(cands, h)
	}
	if h := os.Getenv("JAVA_HOME"); h != "" {
		cands = append(cands, h)
	}
	for _, g := range []string{
		`C:\Program Files\Eclipse Adoptium\jdk-2*`, `C:\Program Files\Java\jdk-2*`,
		`C:\Program Files\Microsoft\jdk-2*`, `C:\Program Files\Zulu\zulu-2*`,
		"/usr/lib/jvm/*", "/Library/Java/JavaVirtualMachines/*/Contents/Home",
	} {
		m, _ := filepath.Glob(g)
		sort.Sort(sort.Reverse(sort.StringSlice(m)))
		cands = append(cands, m...)
	}
	for _, c := range cands {
		if _, err := os.Stat(filepath.Join(c, "lib", "modules")); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("no JDK 21 found (set JAVA_HOME or GOJVM_JDK, or pass -jdk)")
}

// ClassPath loads class metadata from the JDK, with caching.
type ClassPath struct {
	img      *JImage
	cache    map[string]*ClassInfo
	Home     string
	jars     []*zip.ReadCloser
	jarFiles map[string]*zip.File
	jarPkgs  map[string]bool
}

// NewClassPath opens the JDK at jdk (auto-detected when empty) plus extra jar files.
func NewClassPath(jdk string, jars ...string) (*ClassPath, error) {
	home, err := FindJDK(jdk)
	if err != nil {
		return nil, err
	}
	img, err := OpenJImage(filepath.Join(home, "lib", "modules"))
	if err != nil {
		return nil, err
	}
	cp := &ClassPath{img: img, cache: map[string]*ClassInfo{}, Home: home,
		jarFiles: map[string]*zip.File{}, jarPkgs: map[string]bool{}}
	for _, j := range jars {
		if j == "" {
			continue
		}
		zr, err := zip.OpenReader(j)
		if err != nil {
			cp.Close()
			return nil, fmt.Errorf("classpath: %w", err)
		}
		cp.jars = append(cp.jars, zr)
		for _, f := range zr.File {
			if !strings.HasSuffix(f.Name, ".class") {
				continue
			}
			name := strings.TrimSuffix(f.Name, ".class")
			if _, dup := cp.jarFiles[name]; !dup {
				cp.jarFiles[name] = f
			}
			if i := strings.LastIndex(name, "/"); i >= 0 {
				cp.jarPkgs[name[:i]] = true
			}
		}
	}
	return cp, nil
}

func (cp *ClassPath) Close() {
	cp.img.Close()
	for _, j := range cp.jars {
		j.Close()
	}
}

func (cp *ClassPath) HasPackage(p string) bool { return cp.img.HasPackage(p) || cp.jarPkgs[p] }

func (cp *ClassPath) readClass(name string) ([]byte, bool) {
	if b, ok := cp.img.Read(name); ok {
		return b, true
	}
	if f, ok := cp.jarFiles[name]; ok {
		rc, err := f.Open()
		if err != nil {
			return nil, false
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		return b, err == nil
	}
	return nil, false
}

// Lookup returns class info for an internal name, or nil if it does not exist.
func (cp *ClassPath) Lookup(name string) *ClassInfo {
	if ci, ok := cp.cache[name]; ok {
		return ci
	}
	var ci *ClassInfo
	if b, ok := cp.readClass(name); ok {
		ci, _ = ParseClass(b)
	}
	cp.cache[name] = ci
	return ci
}

// AbstractMethods returns the abstract methods of class (and its supertypes)
// that no concrete method in the hierarchy implements.
func (cp *ClassPath) AbstractMethods(classes ...string) []Member {
	abstract := map[string]Member{}
	concrete := map[string]bool{}
	seen := map[string]bool{}
	var walk func(string)
	walk = func(c string) {
		if seen[c] {
			return
		}
		seen[c] = true
		ci := cp.Lookup(c)
		if ci == nil {
			return
		}
		for _, m := range ci.Methods {
			if m.Access&(AccStatic|AccSynthetic|AccBridge) != 0 || m.Name == "<init>" || m.Name == "<clinit>" {
				continue
			}
			k := m.Name + m.Desc
			if m.Access&AccAbstract != 0 {
				abstract[k] = m
			} else {
				concrete[k] = true
			}
		}
		if ci.Super != "" {
			walk(ci.Super)
		}
		for _, i := range ci.Interfaces {
			walk(i)
		}
	}
	for _, c := range classes {
		walk(c)
	}
	var out []Member
	for k, m := range abstract {
		if !concrete[k] {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name+out[i].Desc < out[j].Name+out[j].Desc })
	return out
}

// IsSubclass reports whether class a is the same as or a subtype of b.
func (cp *ClassPath) IsSubclass(a, b string) bool {
	if a == b || b == "java/lang/Object" {
		return true
	}
	ci := cp.Lookup(a)
	if ci == nil {
		return false
	}
	if ci.Super != "" && cp.IsSubclass(ci.Super, b) {
		return true
	}
	for _, i := range ci.Interfaces {
		if cp.IsSubclass(i, b) {
			return true
		}
	}
	return false
}

// MethodsNamed collects public methods called name from class, its superclasses and interfaces.
func (cp *ClassPath) MethodsNamed(class, name string) []Member {
	return cp.HierarchyMethods(class, name, false)
}

// HierarchyMethods is MethodsNamed, optionally including protected methods.
func (cp *ClassPath) HierarchyMethods(class, name string, protected bool) []Member {
	var out []Member
	seen := map[string]bool{}
	var walk func(string)
	walk = func(c string) {
		ci := cp.Lookup(c)
		if ci == nil {
			return
		}
		for _, m := range ci.Methods {
			key := m.Desc[:strings.Index(m.Desc, ")")]
			vis := m.Access&AccPublic != 0 || (protected && m.Access&0x0004 != 0)
			if m.Name == name && vis && m.Access&(AccBridge|AccSynthetic) == 0 && !seen[key] {
				seen[key] = true
				out = append(out, m)
			}
		}
		if ci.Super != "" {
			walk(ci.Super)
		}
		for _, i := range ci.Interfaces {
			walk(i)
		}
		if ci.IsInterface() {
			walk("java/lang/Object")
		}
	}
	walk(class)
	return out
}

// FieldNamed finds a public field.
func (cp *ClassPath) FieldNamed(class, name string) (*Member, string) {
	ci := cp.Lookup(class)
	if ci == nil {
		return nil, ""
	}
	for i := range ci.Fields {
		if ci.Fields[i].Name == name && ci.Fields[i].Access&AccPublic != 0 {
			return &ci.Fields[i], class
		}
	}
	if ci.Super != "" {
		if m, o := cp.FieldNamed(ci.Super, name); m != nil {
			return m, o
		}
	}
	for _, i := range ci.Interfaces {
		if m, o := cp.FieldNamed(i, name); m != nil {
			return m, o
		}
	}
	return nil, ""
}

// AllMethods lists the public (and optionally protected) methods of class
// including inherited ones, de-duplicated by name and descriptor.
func (cp *ClassPath) AllMethods(class string, protected bool) []Member {
	var out []Member
	seen := map[string]bool{}
	visited := map[string]bool{}
	var walk func(string)
	walk = func(c string) {
		if visited[c] {
			return
		}
		visited[c] = true
		ci := cp.Lookup(c)
		if ci == nil {
			return
		}
		for _, m := range ci.Methods {
			vis := m.Access&AccPublic != 0 || (protected && m.Access&0x0004 != 0)
			if !vis || m.Name == "<init>" || m.Name == "<clinit>" || m.Access&(AccBridge|AccSynthetic) != 0 {
				continue
			}
			k := m.Name + m.Desc[:strings.Index(m.Desc, ")")]
			if !seen[k] {
				seen[k] = true
				out = append(out, m)
			}
		}
		if ci.Super != "" {
			walk(ci.Super)
		}
		for _, i := range ci.Interfaces {
			walk(i)
		}
		if ci.IsInterface() {
			walk("java/lang/Object")
		}
	}
	walk(class)
	return out
}

// AllFields lists public fields of class including inherited ones.
func (cp *ClassPath) AllFields(class string) []Member {
	var out []Member
	seen := map[string]bool{}
	var walk func(string)
	walk = func(c string) {
		ci := cp.Lookup(c)
		if ci == nil {
			return
		}
		for _, f := range ci.Fields {
			if f.Access&AccPublic != 0 && !seen[f.Name] {
				seen[f.Name] = true
				out = append(out, f)
			}
		}
		if ci.Super != "" {
			walk(ci.Super)
		}
		for _, i := range ci.Interfaces {
			walk(i)
		}
	}
	walk(class)
	return out
}

// ClassesIn lists the public top-level classes of a package (internal name pkg/Simple).
func (cp *ClassPath) ClassesIn(pkg string) []string {
	var names []string
	for n := range cp.img.classes {
		names = append(names, n)
	}
	for n := range cp.jarFiles {
		names = append(names, n)
	}
	var out []string
	seen := map[string]bool{}
	for _, n := range names {
		i := strings.LastIndex(n, "/")
		if i < 0 || n[:i] != pkg || strings.Contains(n[i:], "$") || seen[n] {
			continue
		}
		seen[n] = true
		if ci := cp.Lookup(n); ci != nil && ci.Access&AccPublic != 0 {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// Packages lists user-visible package names (java/..., javax/..., and any in classpath jars).
func (cp *ClassPath) Packages() []string {
	set := map[string]bool{}
	for p := range cp.img.packages {
		if strings.HasPrefix(p, "java/") || strings.HasPrefix(p, "javax/") {
			set[p] = true
		}
	}
	for p := range cp.jarPkgs {
		set[p] = true
	}
	var out []string
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
