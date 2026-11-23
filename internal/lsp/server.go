// Package lsp implements a small Language Server Protocol server for gojvm:
// live compiler diagnostics, Java-aware completion, and gofmt formatting.
package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"go/format"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/AydenC77/gojvm/internal/compiler"
	"github.com/AydenC77/gojvm/internal/project"
)

type message struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  any              `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lspRange struct {
	Start position `json:"start"`
	End   position `json:"end"`
}

type server struct {
	in        *bufio.Reader
	out       io.Writer
	log       io.Writer
	docs      map[string]string // normalized path -> text (open documents)
	realPath  map[string]string // normalized path -> path as sent by the client
	sessions  map[string]*project.Session
	published map[string]bool // uris we have published diagnostics for
	warned    map[string]bool
	shutdown  bool
}

// Serve runs the language server until the client exits. It returns the exit code.
func Serve(in io.Reader, out, log io.Writer) int {
	s := &server{
		in: bufio.NewReader(in), out: out, log: log,
		docs: map[string]string{}, realPath: map[string]string{},
		sessions: map[string]*project.Session{}, published: map[string]bool{}, warned: map[string]bool{},
	}
	defer func() {
		for _, sess := range s.sessions {
			sess.Close()
		}
	}()
	for {
		msg, err := s.read()
		if err != nil {
			if err != io.EOF {
				fmt.Fprintln(log, "gojvm lsp: read:", err)
			}
			return 0
		}
		if s.handle(msg) {
			if s.shutdown {
				return 0
			}
			return 1
		}
	}
}

func (s *server) read() (*message, error) {
	length := -1
	for {
		line, err := s.in.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			length, _ = strconv.Atoi(strings.TrimSpace(line[len("content-length:"):]))
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("missing Content-Length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(s.in, body); err != nil {
		return nil, err
	}
	var m message
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *server) write(m *message) {
	m.JSONRPC = "2.0"
	b, _ := json.Marshal(m)
	fmt.Fprintf(s.out, "Content-Length: %d\r\n\r\n%s", len(b), b)
}

func (s *server) reply(id *json.RawMessage, result any) {
	if result == nil {
		result = json.RawMessage("null")
	}
	s.write(&message{ID: id, Result: result})
}

func (s *server) notify(method string, params any) {
	b, _ := json.Marshal(params)
	s.write(&message{Method: method, Params: b})
}

func (s *server) showMessage(typ int, text string) {
	s.notify("window/showMessage", map[string]any{"type": typ, "message": text})
}

// handle processes one message; it returns true when the server should exit.
func (s *server) handle(m *message) (exit bool) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(s.log, "gojvm lsp: panic handling %s: %v\n", m.Method, r)
			if m.ID != nil {
				s.write(&message{ID: m.ID, Error: &rpcError{Code: -32603, Message: fmt.Sprint(r)}})
			}
		}
	}()
	switch m.Method {
	case "initialize":
		s.reply(m.ID, map[string]any{
			"capabilities": map[string]any{
				"textDocumentSync": map[string]any{"openClose": true, "change": 1, "save": map[string]any{}},
				"completionProvider": map[string]any{
					"triggerCharacters": []string{"."},
					"resolveProvider":   false,
				},
				"documentFormattingProvider": true,
			},
			"serverInfo": map[string]any{"name": "gojvm"},
		})
	case "initialized", "$/cancelRequest", "workspace/didChangeConfiguration", "workspace/didChangeWatchedFiles":
	case "shutdown":
		s.shutdown = true
		s.reply(m.ID, nil)
	case "exit":
		return true
	case "textDocument/didOpen":
		var p struct {
			TextDocument struct {
				URI  string `json:"uri"`
				Text string `json:"text"`
			} `json:"textDocument"`
		}
		json.Unmarshal(m.Params, &p)
		s.setDoc(p.TextDocument.URI, p.TextDocument.Text)
		s.publish(uriToPath(p.TextDocument.URI))
	case "textDocument/didChange":
		var p struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
			ContentChanges []struct {
				Text string `json:"text"`
			} `json:"contentChanges"`
		}
		json.Unmarshal(m.Params, &p)
		if n := len(p.ContentChanges); n > 0 {
			s.setDoc(p.TextDocument.URI, p.ContentChanges[n-1].Text)
			s.publish(uriToPath(p.TextDocument.URI))
		}
	case "textDocument/didSave":
		var p struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		json.Unmarshal(m.Params, &p)
		s.publish(uriToPath(p.TextDocument.URI))
	case "textDocument/didClose":
		var p struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		json.Unmarshal(m.Params, &p)
		path := uriToPath(p.TextDocument.URI)
		delete(s.docs, norm(path))
		delete(s.realPath, norm(path))
		s.publishDiags(p.TextDocument.URI, nil)
	case "textDocument/completion":
		s.reply(m.ID, s.completion(m.Params))
	case "textDocument/formatting":
		s.reply(m.ID, s.formatting(m.Params))
	default:
		if m.ID != nil {
			s.write(&message{ID: m.ID, Error: &rpcError{Code: -32601, Message: "method not found: " + m.Method}})
		}
	}
	return false
}

func norm(p string) string {
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

func uriToPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return uri
	}
	p := u.Path
	if runtime.GOOS == "windows" && len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	return filepath.FromSlash(p)
}

func pathToURI(p string) string {
	sl := filepath.ToSlash(p)
	if !strings.HasPrefix(sl, "/") {
		sl = "/" + sl
	}
	return (&url.URL{Scheme: "file", Path: sl}).String()
}

func (s *server) setDoc(uri, text string) {
	p := uriToPath(uri)
	s.docs[norm(p)] = text
	s.realPath[norm(p)] = p
}

func (s *server) session(path string) *project.Session {
	cfg, err := project.Find(path)
	if err != nil {
		s.warnOnce("cfg"+path, "gojvm: "+err.Error())
		return nil
	}
	key := cfg.Dir + "\x00" + strings.Join(cfg.Jars(), "\x00") + "\x00" + cfg.JDK
	if sess, ok := s.sessions[key]; ok {
		return sess
	}
	sess, err := project.Open(cfg, "")
	if err != nil {
		s.warnOnce(key, "gojvm: "+err.Error())
		return nil
	}
	s.sessions[key] = sess
	return sess
}

func (s *server) warnOnce(key, msg string) {
	if !s.warned[key] {
		s.warned[key] = true
		s.showMessage(1, msg)
	}
}

// packageFiles returns the .go files of the package containing path plus an
// overlay of unsaved editor buffers.
func (s *server) packageFiles(path string) ([]string, map[string]string) {
	dir := filepath.Dir(path)
	files, _ := project.GoFiles(dir)
	have := map[string]bool{}
	overlay := map[string]string{}
	for _, f := range files {
		have[norm(f)] = true
		if t, ok := s.docs[norm(f)]; ok {
			overlay[f] = t
		}
	}
	if !have[norm(path)] {
		files = append(files, path)
		if t, ok := s.docs[norm(path)]; ok {
			overlay[path] = t
		}
	}
	return files, overlay
}

func (s *server) fileText(path string) string {
	if t, ok := s.docs[norm(path)]; ok {
		return t
	}
	b, _ := os.ReadFile(path)
	return string(b)
}

func isIdent(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= 0x80
}

func (s *server) publish(path string) {
	sess := s.session(path)
	if sess == nil {
		return
	}
	files, overlay := s.packageFiles(path)
	_, diags := sess.Build(files, overlay)
	byFile := map[string][]compiler.Diag{}
	for _, d := range diags {
		f := d.File
		if f == "" {
			f = path
		}
		byFile[norm(f)] = append(byFile[norm(f)], d)
	}
	// publish for every file in the package (clearing ones that are now clean)
	for _, f := range files {
		var out []map[string]any
		text := s.fileText(f)
		lines := strings.Split(text, "\n")
		for _, d := range byFile[norm(f)] {
			line := max(d.Line-1, 0)
			col := max(d.Col-1, 0)
			end := col + 1
			if line < len(lines) {
				l := strings.TrimRight(lines[line], "\r")
				if col < len(l) {
					e := col
					for e < len(l) && isIdent(l[e]) {
						e++
					}
					if e > col {
						end = e
					}
				}
				col, end = byteToUTF16(l, col), byteToUTF16(l, end)
			}
			out = append(out, map[string]any{
				"range":    lspRange{position{line, col}, position{line, end}},
				"severity": 1,
				"source":   "gojvm",
				"message":  d.Msg,
			})
		}
		s.publishDiags(pathToURI(f), out)
	}
}

func byteToUTF16(line string, b int) int {
	if b > len(line) {
		b = len(line)
	}
	n := 0
	for _, r := range line[:b] {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// utf16ToByte converts a UTF-16 column to a byte offset within line.
func utf16ToByte(line string, col int) int {
	n := 0
	for i, r := range line {
		if n >= col {
			return i
		}
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return len(line)
}

func (s *server) publishDiags(uri string, diags []map[string]any) {
	if diags == nil {
		diags = []map[string]any{}
		if !s.published[uri] {
			s.published[uri] = true
		}
	} else {
		s.published[uri] = true
	}
	s.notify("textDocument/publishDiagnostics", map[string]any{"uri": uri, "diagnostics": diags})
}

var importLine = regexp.MustCompile(`^\s*(import\s+)?([A-Za-z_]\w*\s+)?"([^"]*)$`)

func (s *server) completion(params json.RawMessage) any {
	var p struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
		Position position `json:"position"`
	}
	json.Unmarshal(params, &p)
	path := uriToPath(p.TextDocument.URI)
	sess := s.session(path)
	if sess == nil {
		return nil
	}
	text := s.fileText(path)
	lines := strings.Split(text, "\n")
	if p.Position.Line >= len(lines) {
		return nil
	}
	cur := strings.TrimRight(lines[p.Position.Line], "\r")
	col := utf16ToByte(cur, p.Position.Character)

	// import path completion
	if m := importLine.FindStringSubmatch(cur[:col]); m != nil {
		prefix := m[3]
		startChar := byteToUTF16(cur, col-len(prefix))
		var items []map[string]any
		for _, pkg := range append([]string{"fmt", "math", "os"}, sess.CP.Packages()...) {
			if strings.HasPrefix(pkg, prefix) {
				items = append(items, map[string]any{
					"label": pkg, "kind": 9,
					"textEdit": map[string]any{
						"range":   lspRange{position{p.Position.Line, startChar}, p.Position},
						"newText": pkg,
					},
				})
			}
		}
		return map[string]any{"isIncomplete": false, "items": items}
	}

	// substitute the marker for the identifier under the cursor
	start := col
	for start > 0 && isIdent(cur[start-1]) {
		start--
	}
	end := col
	for end < len(cur) && isIdent(cur[end]) {
		end++
	}
	lines[p.Position.Line] = cur[:start] + compiler.CompleteMarker + cur[end:]
	modified := strings.Join(lines, "\n")

	files, overlay := s.packageFiles(path)
	for f := range overlay {
		if norm(f) == norm(path) {
			delete(overlay, f)
		}
	}
	target := path
	for _, f := range files {
		if norm(f) == norm(path) {
			target = f
		}
	}
	overlay[target] = modified
	items := sess.Complete(files, overlay)

	out := []map[string]any{}
	for _, it := range items {
		m := map[string]any{"label": it.Label, "kind": it.Kind}
		if it.Detail != "" {
			m["detail"] = it.Detail
		}
		switch it.Kind {
		case compiler.KindMethod, compiler.KindFunction, compiler.KindConstructor:
			m["insertTextFormat"] = 2
			if strings.HasPrefix(it.Detail, "()") && !strings.Contains(it.Detail, "overloads") {
				m["insertText"] = it.Label + "()"
			} else {
				m["insertText"] = it.Label + "($0)"
			}
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i]["label"].(string) < out[j]["label"].(string) })
	return map[string]any{"isIncomplete": false, "items": out}
}

func (s *server) formatting(params json.RawMessage) any {
	var p struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
	}
	json.Unmarshal(params, &p)
	path := uriToPath(p.TextDocument.URI)
	text := s.fileText(path)
	out, err := format.Source([]byte(text))
	if err != nil || string(out) == text {
		return []any{}
	}
	lines := strings.Split(text, "\n")
	last := lines[len(lines)-1]
	return []map[string]any{{
		"range":   lspRange{position{0, 0}, position{len(lines) - 1, utf8.RuneCountInString(last)}},
		"newText": string(out),
	}}
}
