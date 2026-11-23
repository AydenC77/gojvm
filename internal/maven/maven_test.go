package maven

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func pomXML(group, artifact, version, body string) string {
	return `<project><groupId>` + group + `</groupId><artifactId>` + artifact + `</artifactId><version>` + version + `</version>` + body + `</project>`
}

func dep(g, a, v, extra string) string {
	return `<dependency><groupId>` + g + `</groupId><artifactId>` + a + `</artifactId><version>` + v + `</version>` + extra + `</dependency>`
}

func TestResolveTransitive(t *testing.T) {
	files := map[string]string{
		// app -> lib (compile), junit (test), tool (provided), opt (optional), managed (version from dependencyManagement)
		"/org/app/1.0/app-1.0.pom": pomXML("org", "app", "1.0", `
			<properties><lib.version>2.0</lib.version></properties>
			<dependencyManagement><dependencies>`+dep("org", "managed", "3.0", "")+`</dependencies></dependencyManagement>
			<dependencies>`+
			dep("org", "lib", "${lib.version}", "")+
			dep("org", "junit", "4.0", "<scope>test</scope>")+
			dep("org", "tool", "1.0", "<scope>provided</scope>")+
			dep("org", "opt", "1.0", "<optional>true</optional>")+
			`<dependency><groupId>org</groupId><artifactId>managed</artifactId></dependency>
			</dependencies>`),
		"/org/app/1.0/app-1.0.jar":         "jar-app",
		"/org/lib/2.0/lib-2.0.pom":         pomXML("org", "lib", "2.0", ""),
		"/org/lib/2.0/lib-2.0.jar":         "jar-lib",
		"/org/managed/3.0/managed-3.0.pom": pomXML("org", "managed", "3.0", ""),
		"/org/managed/3.0/managed-3.0.jar": "jar-managed",
		// snapshot
		"/org/snap/1.0-SNAPSHOT/maven-metadata.xml":             `<metadata><versioning><snapshotVersions><snapshotVersion><extension>jar</extension><value>1.0-20240101.120000-7</value></snapshotVersion></snapshotVersions></versioning></metadata>`,
		"/org/snap/1.0-SNAPSHOT/snap-1.0-20240101.120000-7.pom": pomXML("org", "snap", "1.0-SNAPSHOT", ""),
		"/org/snap/1.0-SNAPSHOT/snap-1.0-20240101.120000-7.jar": "jar-snap",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := files[r.URL.Path]; ok {
			w.Write([]byte(body))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	r := NewResolver([]string{srv.URL}, nil)
	r.Cache = t.TempDir()
	jars, err := r.Resolve([]string{"org:app:1.0", "org:snap:1.0-SNAPSHOT"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, j := range jars {
		got[filepath.Base(j)] = true
	}
	for _, want := range []string{"app-1.0.jar", "lib-2.0.jar", "managed-3.0.jar", "snap-1.0-20240101.120000-7.jar"} {
		if !got[want] {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	for _, bad := range []string{"junit-4.0.jar", "tool-1.0.jar", "opt-1.0.jar"} {
		if got[bad] {
			t.Errorf("%s should have been filtered out", bad)
		}
	}

	// a second resolve must be served from the cache with the server gone
	srv.Close()
	r2 := NewResolver([]string{srv.URL}, nil)
	r2.Cache = r.Cache
	again, err := r2.Resolve([]string{"org:app:1.0", "org:snap:1.0-SNAPSHOT"})
	if err != nil || len(again) != len(jars) {
		t.Fatalf("cached resolve: %v %v", again, err)
	}

	// a missing direct dependency is an error
	r3 := NewResolver(nil, nil)
	r3.Cache = t.TempDir()
	r3.Repos = []string{"http://127.0.0.1:1/"}
	if _, err := r3.Resolve([]string{"org:nope:1.0"}); err == nil || !strings.Contains(err.Error(), "org:nope:1.0") {
		t.Fatalf("expected error naming the dependency, got %v", err)
	}
}

func TestParseCoord(t *testing.T) {
	c, err := ParseCoord("io.papermc.paper:paper-api:1.21.4-R0.1-SNAPSHOT")
	if err != nil || c.Group != "io.papermc.paper" || c.Version != "1.21.4-R0.1-SNAPSHOT" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := ParseCoord("nope"); err == nil {
		t.Fatal("expected error")
	}
}
