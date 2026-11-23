package templates

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFuncs(t *testing.T) {
	cases := []struct{ got, want string }{
		{pascal("my cool-plugin"), "MyCoolPlugin"},
		{pascal("MyPlugin"), "MyPlugin"},
		{pascal("9lives"), "Plugin9lives"},
		{identifier("My Plugin!"), "MyPlugin"},
		{apiVersion("1.21.4"), "1.21"},
		{apiVersion("26.1.2"), "26.1"},
		{yamlStr(`a "b": c`), `"a \"b\": c"`},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %q want %q", c.got, c.want)
		}
	}
	if cmpVer("1.21.10", "1.21.9") <= 0 || cmpVer("26.1", "1.21.11") <= 0 || cmpVer("1.20.5", "1.20.5") != 0 {
		t.Error("cmpVer ordering wrong")
	}
}

// Every built-in template must render with its defaults into valid files.
func TestGenerateAll(t *testing.T) {
	all, err := All()
	if err != nil || len(all) == 0 {
		t.Fatalf("no templates: %v", err)
	}
	for _, tpl := range all {
		t.Run(tpl.ID, func(t *testing.T) {
			dir := t.TempDir()
			vals := tpl.Defaults()
			vals["PROJECT_NAME"] = "Demo Thing"
			vals["GROUP_ID"] = "org.example"
			created, notes, err := tpl.Generate(vals, dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(created) == 0 || notes == "" {
				t.Fatalf("created %v notes %q", created, notes)
			}
			b, err := os.ReadFile(filepath.Join(dir, "gojvm.json"))
			if err != nil {
				t.Fatal(err)
			}
			var cfg map[string]any
			if err := json.Unmarshal(b, &cfg); err != nil {
				t.Fatalf("gojvm.json is not valid JSON: %v\n%s", err, b)
			}
			for _, f := range created {
				body, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f)))
				if strings.Contains(string(body), "[[") && !strings.HasSuffix(f, ".md") {
					t.Errorf("%s still contains template markers:\n%s", f, body)
				}
			}
			// refuses to overwrite
			if _, _, err := tpl.Generate(vals, dir); err == nil {
				t.Error("second Generate into the same dir should fail")
			}
		})
	}
}

func TestSelectValidation(t *testing.T) {
	tpl, _ := Find("paper-plugin")
	vals := tpl.Defaults()
	vals["MC_VERSION"] = "0.0.1"
	if _, _, err := tpl.Generate(vals, t.TempDir()); err == nil {
		t.Error("an unknown Minecraft version should be rejected")
	}
	if _, err := Find("nope"); err == nil || !strings.Contains(err.Error(), "paper-plugin") {
		t.Errorf("unknown template error should list the available ones: %v", err)
	}
}
