package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/AydenC77/gojvm/internal/jvm"
)

type client struct {
	t     *testing.T
	in    io.Writer
	out   *bufio.Reader
	next  int
	diags map[string][]map[string]any
}

func (c *client) send(m map[string]any) {
	m["jsonrpc"] = "2.0"
	b, _ := json.Marshal(m)
	fmt.Fprintf(c.in, "Content-Length: %d\r\n\r\n%s", len(b), b)
}

func (c *client) readMsg() map[string]any {
	n := 0
	for {
		line, err := c.out.ReadString('\n')
		if err != nil {
			c.t.Fatal(err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "Content-Length:") {
			n, _ = strconv.Atoi(strings.TrimSpace(line[15:]))
		}
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.out, buf); err != nil {
		c.t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(buf, &m)
	return m
}

// request sends a request and returns its result, collecting diagnostics on the way.
func (c *client) request(method string, params any) any {
	c.next++
	c.send(map[string]any{"id": c.next, "method": method, "params": params})
	for {
		m := c.readMsg()
		if m["method"] == "textDocument/publishDiagnostics" {
			p := m["params"].(map[string]any)
			d, _ := p["diagnostics"].([]any)
			var ds []map[string]any
			for _, x := range d {
				ds = append(ds, x.(map[string]any))
			}
			c.diags[p["uri"].(string)] = ds
			continue
		}
		if id, ok := m["id"].(float64); ok && int(id) == c.next {
			return m["result"]
		}
	}
}

// notify sends a notification and drains diagnostics produced by it.
func (c *client) notify(method string, params any, expectDiags int) {
	c.send(map[string]any{"method": method, "params": params})
	for i := 0; i < expectDiags; i++ {
		m := c.readMsg()
		if m["method"] == "textDocument/publishDiagnostics" {
			p := m["params"].(map[string]any)
			d, _ := p["diagnostics"].([]any)
			var ds []map[string]any
			for _, x := range d {
				ds = append(ds, x.(map[string]any))
			}
			c.diags[p["uri"].(string)] = ds
		}
	}
}

func labels(res any) map[string]bool {
	out := map[string]bool{}
	if m, ok := res.(map[string]any); ok {
		for _, it := range m["items"].([]any) {
			out[it.(map[string]any)["label"].(string)] = true
		}
	}
	return out
}

func TestServer(t *testing.T) {
	if _, err := jvm.FindJDK(""); err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	uri := pathToURI(file)
	src := `package main

import (
	"fmt"
	"java/util"
)

type P struct{ X int }

func (p *P) Hit() {}

func main() {
	l := util.NewArrayList()
	l.Add("a")
	var x int = "oops"
	fmt.Println(l, x)
	p := &P{}
	p.Hit()
	util.Collections.Sort(l)
}
`
	os.WriteFile(file, []byte(src), 0o644)

	cr, cw := io.Pipe()
	sr, sw := io.Pipe()
	done := make(chan int)
	go func() { done <- Serve(cr, sw, io.Discard) }()
	c := &client{t: t, in: cw, out: bufio.NewReader(sr), diags: map[string][]map[string]any{}}

	res := c.request("initialize", map[string]any{"processId": nil, "rootUri": pathToURI(dir), "capabilities": map[string]any{}})
	if res.(map[string]any)["capabilities"] == nil {
		t.Fatal("no capabilities")
	}
	c.notify("initialized", map[string]any{}, 0)
	c.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": uri, "languageId": "go", "version": 1, "text": src}}, 1)

	ds := c.diags[uri]
	if len(ds) != 1 || !strings.Contains(ds[0]["message"].(string), `cannot use "oops"`) {
		t.Fatalf("diagnostics: %v", ds)
	}
	if line := ds[0]["range"].(map[string]any)["start"].(map[string]any)["line"].(float64); int(line) != 14 {
		t.Fatalf("diagnostic on line %v, want 14", line)
	}

	complete := func(line, ch int) map[string]bool {
		return labels(c.request("textDocument/completion", map[string]any{
			"textDocument": map[string]any{"uri": uri}, "position": map[string]any{"line": line, "character": ch}}))
	}
	// `l.Add("a")`: after "l." (line 13, col 3)
	if got := complete(13, 3); !got["Add"] || !got["Size"] || !got["Get"] {
		t.Fatalf("instance completion missing members: %d items", len(got))
	}
	// `p.Hit()` on line 17: struct fields + methods
	if got := complete(17, 3); !got["X"] || !got["Hit"] {
		t.Fatalf("struct completion: %v", got)
	}
	// `util.Collections.Sort` line 18, after "util." (col 6): classes and constructors
	if got := complete(18, 6); !got["ArrayList"] || !got["NewArrayList"] || !got["Collections"] {
		t.Fatalf("package completion missing items (%d)", len(got))
	}
	// after "util.Collections." (col 18): static methods
	if got := complete(18, 18); !got["Sort"] || !got["Reverse"] {
		t.Fatalf("static completion: %v", got)
	}
	// plain identifier completion at line 15 (inside main): locals, packages, keywords
	if got := complete(15, 1); !got["fmt"] || !got["l"] || !got["P"] || !got["for"] {
		t.Fatalf("scope completion: %v", got)
	}
	// import path completion
	if got := complete(4, 11); !got["java/util"] || !got["java/util/concurrent"] {
		t.Fatalf("import completion (%d items)", len(got))
	}

	// fixing the error clears diagnostics
	fixed := strings.Replace(src, `"oops"`, `5`, 1)
	c.notify("textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": uri, "version": 2},
		"contentChanges": []any{map[string]any{"text": fixed}}}, 1)
	if len(c.diags[uri]) != 0 {
		t.Fatalf("expected clean diagnostics, got %v", c.diags[uri])
	}

	// formatting
	ugly := strings.Replace(fixed, "func (p *P) Hit() {}", "func   (p *P)   Hit(){}", 1)
	c.notify("textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": uri, "version": 3},
		"contentChanges": []any{map[string]any{"text": ugly}}}, 1)
	edits := c.request("textDocument/formatting", map[string]any{"textDocument": map[string]any{"uri": uri}, "options": map[string]any{}})
	e, _ := edits.([]any)
	if len(e) != 1 || !strings.Contains(e[0].(map[string]any)["newText"].(string), "func (p *P) Hit() {}") {
		t.Fatalf("formatting: %v", edits)
	}

	c.request("shutdown", nil)
	c.send(map[string]any{"method": "exit"})
	if code := <-done; code != 0 {
		t.Fatalf("exit code %d", code)
	}
}
