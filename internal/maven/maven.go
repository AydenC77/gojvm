// Package maven resolves "group:artifact:version" coordinates (and their
// transitive compile/runtime dependencies) from Maven repositories into a local
// cache, so gojvm projects can declare dependencies in gojvm.json.
package maven

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Central is always searched last.
const Central = "https://repo.maven.apache.org/maven2/"

// Resolver downloads artifacts into Cache.
type Resolver struct {
	Repos []string
	Cache string
	Log   io.Writer
	HTTP  *http.Client
	poms  map[string]*pom
}

// DefaultCache is ~/.gojvm/maven (override with GOJVM_CACHE).
func DefaultCache() string {
	if d := os.Getenv("GOJVM_CACHE"); d != "" {
		return filepath.Join(d, "maven")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, ".gojvm", "maven")
}

func NewResolver(repos []string, log io.Writer) *Resolver {
	var rs []string
	for _, r := range repos {
		rs = append(rs, strings.TrimRight(r, "/")+"/")
	}
	rs = append(rs, Central)
	if log == nil {
		log = io.Discard
	}
	return &Resolver{Repos: rs, Cache: DefaultCache(), Log: log,
		HTTP: &http.Client{Timeout: 60 * time.Second}, poms: map[string]*pom{}}
}

// Coord is a parsed group:artifact:version[:classifier].
type Coord struct{ Group, Artifact, Version, Classifier string }

func (c Coord) String() string { return c.Group + ":" + c.Artifact + ":" + c.Version }

func ParseCoord(s string) (Coord, error) {
	p := strings.Split(strings.TrimSpace(s), ":")
	if len(p) < 3 || len(p) > 4 || p[0] == "" || p[1] == "" || p[2] == "" {
		return Coord{}, fmt.Errorf("bad dependency %q (want group:artifact:version)", s)
	}
	c := Coord{Group: p[0], Artifact: p[1], Version: p[2]}
	if len(p) == 4 {
		c.Classifier = p[3]
	}
	return c, nil
}

func (r *Resolver) fetch(rel string) ([]byte, error) {
	var lastErr error
	for _, repo := range r.Repos {
		resp, err := r.HTTP.Get(repo + rel)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == 200 {
			b, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				lastErr = err
				continue
			}
			return b, nil
		}
		resp.Body.Close()
		lastErr = fmt.Errorf("%s%s: HTTP %d", repo, rel, resp.StatusCode)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%s: not found", rel)
	}
	return nil, lastErr
}

// cached fetches rel through the on-disk cache.
func (r *Resolver) cached(rel string, maxAge time.Duration) ([]byte, error) {
	p := filepath.Join(r.Cache, filepath.FromSlash(rel))
	if st, err := os.Stat(p); err == nil && (maxAge == 0 || time.Since(st.ModTime()) < maxAge) {
		return os.ReadFile(p)
	}
	b, err := r.fetch(rel)
	if err != nil {
		if old, rerr := os.ReadFile(p); rerr == nil {
			return old, nil // offline: a stale copy beats nothing
		}
		return nil, err
	}
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, b, 0o644)
	return b, nil
}

func groupPath(g string) string { return strings.ReplaceAll(g, ".", "/") }

type metadata struct {
	Versioning struct {
		Latest   string   `xml:"latest"`
		Release  string   `xml:"release"`
		Versions []string `xml:"versions>version"`
		Snapshot struct {
			Timestamp   string `xml:"timestamp"`
			BuildNumber string `xml:"buildNumber"`
		} `xml:"snapshot"`
		SnapshotVersions []struct {
			Classifier string `xml:"classifier"`
			Extension  string `xml:"extension"`
			Value      string `xml:"value"`
		} `xml:"snapshotVersions>snapshotVersion"`
	} `xml:"versioning"`
}

// Versions lists the versions published for group:artifact.
func (r *Resolver) Versions(group, artifact string) ([]string, error) {
	b, err := r.cached(groupPath(group)+"/"+artifact+"/maven-metadata.xml", 6*time.Hour)
	if err != nil {
		return nil, err
	}
	var m metadata
	if err := xml.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m.Versioning.Versions, nil
}

// fileVersion maps a SNAPSHOT version to the timestamped file version.
func (r *Resolver) fileVersion(c Coord) string {
	if !strings.HasSuffix(c.Version, "-SNAPSHOT") {
		return c.Version
	}
	b, err := r.cached(groupPath(c.Group)+"/"+c.Artifact+"/"+c.Version+"/maven-metadata.xml", 12*time.Hour)
	if err != nil {
		return c.Version
	}
	var m metadata
	if xml.Unmarshal(b, &m) != nil {
		return c.Version
	}
	for _, sv := range m.Versioning.SnapshotVersions {
		if sv.Classifier == "" && sv.Extension == "jar" && sv.Value != "" {
			return sv.Value
		}
	}
	s := m.Versioning.Snapshot
	if s.Timestamp != "" && s.BuildNumber != "" {
		return strings.TrimSuffix(c.Version, "-SNAPSHOT") + "-" + s.Timestamp + "-" + s.BuildNumber
	}
	return c.Version
}

func (r *Resolver) artifactRel(c Coord, ext string) string {
	fv := r.fileVersion(c)
	name := c.Artifact + "-" + fv
	if c.Classifier != "" {
		name += "-" + c.Classifier
	}
	return groupPath(c.Group) + "/" + c.Artifact + "/" + c.Version + "/" + name + "." + ext
}

// ---------------------------------------------------------------------------
// POM handling

type pomDep struct {
	Group    string `xml:"groupId"`
	Artifact string `xml:"artifactId"`
	Version  string `xml:"version"`
	Scope    string `xml:"scope"`
	Optional string `xml:"optional"`
	Type     string `xml:"type"`
}

type pom struct {
	Group    string `xml:"groupId"`
	Artifact string `xml:"artifactId"`
	Version  string `xml:"version"`
	Parent   struct {
		Group    string `xml:"groupId"`
		Artifact string `xml:"artifactId"`
		Version  string `xml:"version"`
	} `xml:"parent"`
	Props struct {
		Items []struct {
			XMLName xml.Name
			Value   string `xml:",chardata"`
		} `xml:",any"`
	} `xml:"properties"`
	Managed []pomDep `xml:"dependencyManagement>dependencies>dependency"`
	Deps    []pomDep `xml:"dependencies>dependency"`

	props  map[string]string
	parent *pom
}

func (r *Resolver) loadPOM(c Coord) *pom {
	key := c.String()
	if p, ok := r.poms[key]; ok {
		return p
	}
	r.poms[key] = nil
	b, err := r.cached(groupPath(c.Group)+"/"+c.Artifact+"/"+c.Version+"/"+c.Artifact+"-"+r.fileVersion(c)+".pom", 0)
	if err != nil {
		return nil
	}
	p := &pom{}
	if err := xml.Unmarshal(b, p); err != nil {
		return nil
	}
	p.props = map[string]string{}
	if p.Parent.Artifact != "" {
		p.parent = r.loadPOM(Coord{Group: p.Parent.Group, Artifact: p.Parent.Artifact, Version: p.Parent.Version})
		if p.Group == "" {
			p.Group = p.Parent.Group
		}
		if p.Version == "" {
			p.Version = p.Parent.Version
		}
	}
	for _, it := range p.Props.Items {
		p.props[it.XMLName.Local] = strings.TrimSpace(it.Value)
	}
	r.poms[key] = p
	return p
}

func (p *pom) lookupProp(name string) (string, bool) {
	switch name {
	case "project.version", "pom.version", "version":
		return p.Version, true
	case "project.groupId", "pom.groupId", "groupId":
		return p.Group, true
	case "project.artifactId":
		return p.Artifact, true
	case "project.parent.version":
		return p.Parent.Version, true
	}
	for q := p; q != nil; q = q.parent {
		if v, ok := q.props[name]; ok {
			return v, true
		}
	}
	return "", false
}

func (p *pom) interpolate(s string) string {
	for i := 0; i < 8 && strings.Contains(s, "${"); i++ {
		start := strings.Index(s, "${")
		end := strings.Index(s[start:], "}")
		if end < 0 {
			break
		}
		name := s[start+2 : start+end]
		v, ok := p.lookupProp(name)
		if !ok {
			return s
		}
		s = s[:start] + v + s[start+end+1:]
	}
	return s
}

// managedDep finds a dependencyManagement entry in p or its parents.
func (p *pom) managedDep(group, artifact string) (pomDep, *pom) {
	for q := p; q != nil; q = q.parent {
		for _, d := range q.Managed {
			if q.interpolate(d.Group) == group && q.interpolate(d.Artifact) == artifact {
				return d, q
			}
		}
	}
	return pomDep{}, nil
}

func (r *Resolver) dependenciesOf(c Coord) []Coord {
	p := r.loadPOM(c)
	if p == nil {
		return nil
	}
	var out []Coord
	for _, d := range p.Deps {
		g, a, v := p.interpolate(d.Group), p.interpolate(d.Artifact), p.interpolate(d.Version)
		scope, optional := d.Scope, d.Optional
		if md, owner := p.managedDep(g, a); owner != nil {
			if v == "" {
				v = owner.interpolate(md.Version)
			}
			if scope == "" {
				scope = md.Scope
			}
			if optional == "" {
				optional = md.Optional
			}
		}
		if scope == "test" || scope == "provided" || scope == "system" || scope == "import" || optional == "true" {
			continue
		}
		if d.Type != "" && d.Type != "jar" {
			continue
		}
		if g == "" || a == "" || v == "" || strings.ContainsAny(v, "[(,") || strings.Contains(v, "${") {
			continue // version ranges and unresolved properties are skipped
		}
		out = append(out, Coord{Group: g, Artifact: a, Version: v})
	}
	return out
}

// ---------------------------------------------------------------------------

func (r *Resolver) jar(c Coord) (string, error) {
	rel := r.artifactRel(c, "jar")
	p := filepath.Join(r.Cache, filepath.FromSlash(rel))
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	fmt.Fprintf(r.Log, "gojvm: downloading %s\n", c)
	if _, err := r.cached(rel, 0); err != nil {
		return "", err
	}
	return p, nil
}

// Resolve returns jar paths for the direct coordinates and their transitive
// dependencies (breadth-first, nearest version wins). Failures on transitive
// dependencies are logged and skipped; failures on direct ones are errors.
func (r *Resolver) Resolve(coords []string) ([]string, error) {
	if len(coords) == 0 {
		return nil, nil
	}
	if jars, ok := r.loadResolved(coords); ok {
		return jars, nil
	}
	type item struct {
		c      Coord
		direct bool
	}
	var queue []item
	for _, s := range coords {
		c, err := ParseCoord(s)
		if err != nil {
			return nil, err
		}
		queue = append(queue, item{c, true})
	}
	seen := map[string]bool{}
	var jars []string
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		key := it.c.Group + ":" + it.c.Artifact + ":" + it.c.Classifier
		if seen[key] {
			continue
		}
		seen[key] = true
		jp, err := r.jar(it.c)
		if err != nil {
			if it.direct {
				return nil, fmt.Errorf("dependency %s: %w", it.c, err)
			}
			fmt.Fprintf(r.Log, "gojvm: skipping transitive dependency %s (%v)\n", it.c, shorten(err))
			continue
		}
		jars = append(jars, jp)
		for _, d := range r.dependenciesOf(it.c) {
			queue = append(queue, item{d, false})
		}
	}
	r.saveResolved(coords, jars)
	return jars, nil
}

func shorten(err error) string {
	s := err.Error()
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return s
}

// resolution cache: skip all network work when nothing changed.
type resolved struct {
	Jars []string  `json:"jars"`
	Time time.Time `json:"time"`
}

func (r *Resolver) resolvedPath(coords []string) string {
	h := sha1.New()
	for _, rp := range r.Repos {
		h.Write([]byte(rp + "\n"))
	}
	cs := append([]string(nil), coords...)
	sort.Strings(cs)
	for _, c := range cs {
		h.Write([]byte(c + "\n"))
	}
	return filepath.Join(r.Cache, "_resolved", hex.EncodeToString(h.Sum(nil))+".json")
}

func (r *Resolver) loadResolved(coords []string) ([]string, bool) {
	b, err := os.ReadFile(r.resolvedPath(coords))
	if err != nil {
		return nil, false
	}
	var rs resolved
	if json.Unmarshal(b, &rs) != nil {
		return nil, false
	}
	for _, c := range coords {
		if strings.Contains(c, "SNAPSHOT") && time.Since(rs.Time) > 12*time.Hour {
			return nil, false
		}
	}
	for _, j := range rs.Jars {
		if _, err := os.Stat(j); err != nil {
			return nil, false
		}
	}
	return rs.Jars, true
}

func (r *Resolver) saveResolved(coords, jars []string) {
	p := r.resolvedPath(coords)
	os.MkdirAll(filepath.Dir(p), 0o755)
	b, _ := json.Marshal(resolved{Jars: jars, Time: time.Now()})
	os.WriteFile(p, b, 0o644)
}
