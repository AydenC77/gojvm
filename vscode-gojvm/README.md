# gojvm for VS Code

Write Go, run it on the JVM. This extension talks to the `gojvm` language server so the
editor understands Java imports (which `gopls` cannot).

**Requires** the `gojvm` binary on your PATH:

    go install github.com/AydenC77/gojvm@latest

## Features

* **New Project wizard** (`gojvm: New Project…`): Paper, Spigot and BungeeCord plugin templates
  with live Minecraft version lists. `gojvm: New Basic Project…` creates a plain program.
* Live compiler errors as you type (all errors in a file, not just the first).
* Autocomplete for Java: `import "java/…"` paths, classes and `NewClass` constructors after
  `util.`, static members after `lang.Math.`, instance methods after any Java value, fields and
  methods of your structs including inherited Java methods — with Go-style signatures.
* Format on save (gofmt).
* `Ctrl+Alt+R` / the run button / status bar item to run the current package; `gojvm: Build jar`,
  `gojvm: Check`, and `gojvm: Show Java class or package docs…` in the command palette.
* Snippets: `gjmain`, `gjextends`, `gjimpl`, `gjimport`, `gjnew`.

The extension activates in folders containing a `gojvm.json` (`gojvm init` creates one).
gopls is turned off for such workspaces (the extension offers to do it) because it reports
false errors on Java imports.

## Settings

* `gojvm.path` — path to the `gojvm` executable (default `gojvm`).
