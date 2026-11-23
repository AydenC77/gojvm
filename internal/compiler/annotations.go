package compiler

import (
	"go/ast"
	"strconv"
	"strings"

	"github.com/AydenC77/gojvm/internal/jvm"
)

// directiveLines returns, per directive key, the raw text following each
// "//gojvm:key" comment line (preserving quotes and spaces).
func directiveLines(groups ...*ast.CommentGroup) map[string][]string {
	out := map[string][]string{}
	for _, g := range groups {
		if g == nil {
			continue
		}
		for _, cm := range g.List {
			line := strings.TrimSpace(strings.TrimPrefix(cm.Text, "//"))
			if !strings.HasPrefix(line, "gojvm:") {
				continue
			}
			rest := strings.TrimPrefix(line, "gojvm:")
			key, tail, _ := strings.Cut(rest, " ")
			out[key] = append(out[key], strings.TrimSpace(tail))
		}
	}
	return out
}

// splitTokens splits on spaces outside quotes and [ ] brackets.
func splitTokens(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote, depth := false, 0
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case !inQuote && r == '[':
			depth++
			cur.WriteRune(r)
		case !inQuote && r == ']':
			depth--
			cur.WriteRune(r)
		case !inQuote && depth == 0 && (r == ' ' || r == '\t'):
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// parseAnnotations turns the //gojvm:annotation lines of a declaration into
// annotations, checking element names and types against the annotation class.
func (c *Compiler) parseAnnotations(n ast.Node, lines []string) []jvm.Annotation {
	var out []jvm.Annotation
	for _, ln := range lines {
		toks := splitTokens(ln)
		if len(toks) == 0 {
			c.fail(n, "//gojvm:annotation needs a class name")
		}
		class := strings.ReplaceAll(toks[0], ".", "/")
		ci := c.cp.Lookup(class)
		if ci == nil {
			c.fail(n, "annotation class %s not found (add its jar with dependencies or classpath)", class)
		}
		if ci.Access&0x2000 == 0 {
			c.fail(n, "%s is not an annotation", class)
		}
		ann := jvm.Annotation{Type: "L" + class + ";"}
		for _, t := range toks[1:] {
			name, val, ok := strings.Cut(t, "=")
			if !ok {
				name, val = "value", t
			}
			var desc string
			for _, m := range ci.Methods {
				if m.Name == name && m.Desc[:2] == "()" {
					desc = m.Desc[2:]
				}
			}
			if desc == "" {
				c.fail(n, "annotation %s has no element %q", class, name)
			}
			ann.Names = append(ann.Names, name)
			ann.Values = append(ann.Values, c.annValue(n, class, name, desc, val))
		}
		out = append(out, ann)
	}
	return out
}

func (c *Compiler) annValue(n ast.Node, class, elem, desc, val string) jvm.AnnValue {
	bad := func() jvm.AnnValue {
		c.fail(n, "annotation element %s.%s expects type %s, got %q", class, elem, TypeFromDesc(desc), val)
		return jvm.AnnValue{}
	}
	switch desc[0] {
	case '[':
		inner := strings.TrimSuffix(strings.TrimPrefix(val, "["), "]")
		var arr []jvm.AnnValue
		if strings.TrimSpace(inner) != "" {
			for _, p := range splitList(inner) {
				arr = append(arr, c.annValue(n, class, elem, desc[1:], strings.TrimSpace(p)))
			}
		}
		return jvm.AnnValue{Tag: '[', Array: arr}
	case 'Z':
		if val != "true" && val != "false" {
			return bad()
		}
		v := int64(0)
		if val == "true" {
			v = 1
		}
		return jvm.AnnValue{Tag: 'Z', Int: v}
	case 'B', 'S', 'I', 'J':
		i, err := strconv.ParseInt(val, 0, 64)
		if err != nil {
			return bad()
		}
		return jvm.AnnValue{Tag: desc[0], Int: i}
	case 'F', 'D':
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return bad()
		}
		return jvm.AnnValue{Tag: desc[0], Float: f}
	case 'L':
		if desc == "Ljava/lang/String;" {
			s, err := strconv.Unquote(val)
			if err != nil {
				s = strings.Trim(val, `"`)
			}
			return jvm.AnnValue{Tag: 's', Str: s}
		}
		enumClass := desc[1 : len(desc)-1]
		if ci := c.cp.Lookup(enumClass); ci != nil && ci.Access&0x4000 != 0 {
			name := val[strings.LastIndexAny(val, "./")+1:]
			found := false
			for _, f := range ci.Fields {
				if f.Name == name && f.Access&0x4000 != 0 {
					found = true
				}
			}
			if !found {
				c.fail(n, "%s is not a constant of enum %s", name, strings.ReplaceAll(enumClass, "/", "."))
			}
			return jvm.AnnValue{Tag: 'e', EnumType: desc, Str: name}
		}
	}
	return bad()
}

func splitList(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ',' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	out = append(out, cur.String())
	return out
}
