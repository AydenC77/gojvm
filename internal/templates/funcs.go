package templates

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"unicode"

	"github.com/AydenC77/gojvm/internal/maven"
)

const (
	paperRepo  = "https://repo.papermc.io/repository/maven-public/"
	spigotRepo = "https://hub.spigotmc.org/nexus/content/repositories/snapshots/"
)

var funcs = template.FuncMap{
	"lower":      strings.ToLower,
	"upper":      strings.ToUpper,
	"pascal":     pascal,
	"camel":      camel,
	"identifier": identifier,
	"nospace":    func(s string) string { return strings.Join(strings.Fields(s), "") },
	"yamlstr":    yamlStr,
	"paperApi":   paperAPIVersion,
	"spigotApi":  func(mc string) string { return mc + "-R0.1-SNAPSHOT" },
	"apiVersion": apiVersion,
}

var providers = map[string]func() ([]string, error){
	"paper_versions":  func() ([]string, error) { return mcVersions(paperRepo, "io.papermc.paper", "paper-api") },
	"spigot_versions": func() ([]string, error) { return mcVersions(spigotRepo, "org.spigotmc", "spigot-api") },
	"bungee_versions": bungeeVersions,
}

var numRe = regexp.MustCompile(`\d+`)

// cmpVer compares two version strings by their numeric components.
func cmpVer(a, b string) int {
	x, y := numRe.FindAllString(a, -1), numRe.FindAllString(b, -1)
	for i := 0; i < len(x) || i < len(y); i++ {
		var p, q int
		if i < len(x) {
			p, _ = strconv.Atoi(x[i])
		}
		if i < len(y) {
			q, _ = strconv.Atoi(y[i])
		}
		if p != q {
			if p < q {
				return -1
			}
			return 1
		}
	}
	return 0
}

var releaseRe = regexp.MustCompile(`^\d+(\.\d+)+$`)

// minJava21 is the first Minecraft release that requires Java 21; gojvm
// targets Java 21, so older versions (which run on Java 17 or older) are not offered.
const minJava21 = "1.20.5"

func mcVersions(repo, group, artifact string) ([]string, error) {
	vs, err := maven.NewResolver([]string{repo}, nil).Versions(group, artifact)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, v := range vs {
		mc := ""
		switch {
		case strings.HasSuffix(v, "-R0.1-SNAPSHOT"):
			mc = strings.TrimSuffix(v, "-R0.1-SNAPSHOT")
		case strings.Contains(v, ".build.") && strings.HasSuffix(v, "-stable"):
			mc = v[:strings.Index(v, ".build.")]
		}
		if mc != "" && releaseRe.MatchString(mc) && cmpVer(mc, minJava21) >= 0 {
			set[mc] = true
		}
	}
	var out []string
	for v := range set {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return cmpVer(out[i], out[j]) > 0 })
	if len(out) == 0 {
		return nil, fmt.Errorf("no versions found")
	}
	return out, nil
}

func bungeeVersions() ([]string, error) {
	vs, err := maven.NewResolver(nil, nil).Versions("net.md-5", "bungeecord-api")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, v := range vs {
		if !strings.Contains(v, "SNAPSHOT") && !strings.Contains(v, "deprecated") {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return cmpVer(out[i], out[j]) > 0 })
	if len(out) == 0 {
		return nil, fmt.Errorf("no versions found")
	}
	return out, nil
}

// paperAPIVersion maps a Minecraft version to the paper-api artifact version
// (1.21.4 -> 1.21.4-R0.1-SNAPSHOT; the newer scheme 26.1 -> 26.1.build.N-stable).
func paperAPIVersion(mc string) (string, error) {
	vs, err := maven.NewResolver([]string{paperRepo}, nil).Versions("io.papermc.paper", "paper-api")
	if err != nil {
		// offline: assume the classic scheme
		return mc + "-R0.1-SNAPSHOT", nil
	}
	best := ""
	for _, v := range vs {
		if v == mc+"-R0.1-SNAPSHOT" {
			return v, nil
		}
		if strings.HasPrefix(v, mc+".build.") && strings.HasSuffix(v, "-stable") && (best == "" || cmpVer(v, best) > 0) {
			best = v
		}
	}
	if best == "" {
		return "", fmt.Errorf("no paper-api version for Minecraft %s", mc)
	}
	return best, nil
}

// apiVersion gives the major.minor used by plugin.yml's api-version.
func apiVersion(mc string) string {
	p := strings.Split(mc, ".")
	if len(p) >= 2 {
		return p[0] + "." + p[1]
	}
	return mc
}

func words(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

func pascal(s string) string {
	var sb strings.Builder
	for _, w := range words(s) {
		sb.WriteString(strings.ToUpper(w[:1]) + w[1:])
	}
	out := sb.String()
	if out == "" || unicode.IsDigit(rune(out[0])) {
		out = "Plugin" + out
	}
	return out
}

func camel(s string) string {
	p := pascal(s)
	return strings.ToLower(p[:1]) + p[1:]
}

// identifier reduces s to letters and digits (a legal Go/Java identifier body).
func identifier(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_') {
			sb.WriteRune(r)
		}
	}
	out := sb.String()
	if out == "" || unicode.IsDigit(rune(out[0])) {
		out = "p" + out
	}
	return out
}

func yamlStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
