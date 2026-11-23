# gojvm

**Write Go. Run it on the JVM.** gojvm compiles Go source directly to Java 21 bytecode and
packages it as a jar — no Java source step, no interpreter. Call any Java library from Go.
Build programs, or plugins and mods for anything that runs on the JVM (Minecraft included).

## Install

You need Go 1.22+ and a JDK 21 (gojvm finds it via `JAVA_HOME`, common install paths, or `-jdk`).

    go install github.com/AydenC77/gojvm@latest
    gojvm version          # shows the version and the JDK it found

For the editor experience, install the VS Code extension (see below).

## Quick start

    gojvm init hello       # creates main.go, gojvm.json, .vscode settings
    cd hello
    gojvm run              # compile and run
    gojvm build            # -> build/hello.jar   (java -jar build/hello.jar)

| Command | What it does |
|---|---|
| `gojvm init [dir] [-lib name]` | New project (`-lib`: a plugin/library package) |
| `gojvm run [path] [-- args]` | Compile and run |
| `gojvm build [path] [-o out.jar]` | Compile to a jar (a library jar when the package isn't `main`) |
| `gojvm check [path]` | Compile only; report every error with the source line |
| `gojvm doc java/util/ArrayList [filter]` | Show what a Java class/package offers, as Go |
| `gojvm lsp` | Language server (the VS Code extension runs this) |

`path` is a directory (default `.`) or `.go` files. Project settings go in `gojvm.json`:

```json
{
  "classpath": ["libs/paper-api.jar"],
  "resources": "resources",
  "output": "build/myplugin.jar",
  "jdk": ""
}
```

## VS Code

    cd vscode-gojvm && npm install && npx @vscode/vsce package --skip-license
    code --install-extension gojvm-0.1.0.vsix

(CI also publishes the `.vsix` as a build artifact.) In a folder with a `gojvm.json` you get:

* live compiler errors as you type — all of them, with positions
* **autocomplete for Java**: `import "java/…"` paths, classes and `NewClass` constructors
  after `util.`, static members after `lang.Math.`, methods/fields after any Java value, your
  structs' fields and methods (including inherited Java ones) — with Go-style signatures
* format on save, `Ctrl+Alt+R` to run, build/check/doc commands, snippets
* gopls is switched off for the workspace (it can't resolve Java imports and would show false errors)

## Calling Java

```go
import (
	"fmt"
	"java/util"
	"java/lang"
)

func main() {
	l := util.NewArrayList()          // constructor: pkg.New<Class>(args)
	l.Add("x")                        // Java add -> Go Add
	lang.System.Out.Println("hi")     // static fields and methods
	s := l.Get(0).(string)            // generics are erased: assert the real type
	fmt.Println(s, l.Size())
}
```

* Overloads are resolved at compile time from the argument types; primitives box automatically.
* Go typing is strict: Go `int` is a JVM `long`; `int32` is `int`; Java `long` is `int64`. Use
  `int32(x)` / `int64(x)` to call methods that take `int` / `long`.
* `any` is `java.lang.Object`. `fmt`, `math`, and `os.Exit` are built in.

## Structs, methods, and extending Java classes

Structs compile to JVM classes with Go's value semantics (copied on assign/pass/return;
`&x` and pointer receivers alias the same object). A struct can extend a Java class and
implement Java interfaces:

```go
//gojvm:extends api/Plugin
//gojvm:implements api/Listener
type Hello struct{ Count int }

func (h *Hello) OnEnable() { h.Log("hi") }   // overrides onEnable(); Log is the inherited protected log()
```

A Go method overrides a Java method when the name matches (`Run` → `run`) and its Go signature
maps to exactly the Java descriptor; a near miss is a compile error that shows the expected
signature. Unimplemented abstract methods are compile errors. Add other jars (a server API) with
`classpath` in `gojvm.json` or `-cp`. A package that is not `main` builds a library jar with no
entry point, its classes in a JVM package named after the Go package; `resources` bundles files
like `plugin.yml`. See [`_examples/plugin`](_examples/plugin).

## Language support

**Supported:** functions, `var`/`const`/`iota`, `:=`, bool/int8/16/32/64/int/float32/64/string,
all operators, `if`/`for`/`range` (ints, Java arrays)/`switch`/labels/`break`/`continue`, string
concat/compare, structs (fields, methods, literals, `&`/`new`, `String()` used by fmt), Java
arrays, type assertions, `println`/`panic`/`min`/`max`/`new`/`len`.

**Not yet:** Go interfaces, embedded structs, struct `==`, slices, maps, multiple return values,
closures, goroutines, `defer`, generics, unsigned types, importing other Go packages,
constructor arguments for Java superclasses, comma-ok assertions.

**Differences from Go:** shifts by ≥ the bit width follow the JVM; `%v` in `Printf` maps to
Java's `%s`; a panic is a Java `RuntimeException`; `p := &x; x = y` replaces `x`'s object
instead of updating it in place (so `p` stops aliasing `x`).

## Development

    go test ./...        # needs a JDK 21; programs are compiled and run with -Xverify:all

Layout: `internal/jvm` (classfile writer, StackMapTable frames, jimage/jar reader),
`internal/compiler` (type checker + code generator), `internal/project` (config, builds),
`internal/lsp` (language server), `vscode-gojvm` (extension).
