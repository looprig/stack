package stack_test

import (
	"bytes"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const looprigPrefix = "github.com/looprig/"

// forbiddenModules maps a looprig module to why no file in stack, test or
// not, may import it. The key is matched as a whole module name.
var forbiddenModules = map[string]string{
	"tools":      "tools are the product's choice; the stack names none",
	"llm":        "providers are the product's choice; the stack names none",
	"sandbox":    "sandboxing is the product's choice (and its Init is main's)",
	"carbon":     "product repository",
	"client":     "product repository",
	"kosa":       "product repository",
	"policy53":   "product repository",
	"capstan":    "product repository",
	"tui":        "UI repository",
	"wui":        "retired UI repository",
	"tests":      "integration repository; it consumes stack",
	"controller": "Kubernetes placement is out of scope for stack v0.x",
}

// backendHomes maps a backend import to the ONE package whose production files
// may import it. Backends live behind subpackages so the root never drags a
// storage technology into a consumer's graph.
var backendHomes = map[string]string{
	looprigPrefix + "fsstore":          "localdisk",
	looprigPrefix + "storage/memstore": "memory",
}

// importVerdict is why a file in package directory dir (module-relative,
// "." for the root) may not import path, or "" when it may.
func importVerdict(dir string, test bool, path string) string {
	if rest, ok := strings.CutPrefix(path, looprigPrefix); ok {
		module, _, _ := strings.Cut(rest, "/")
		if reason, forbidden := forbiddenModules[module]; forbidden {
			return reason
		}
	}
	if test {
		return ""
	}
	for backend, home := range backendHomes {
		if (path == backend || strings.HasPrefix(path, backend+"/")) && dir != home {
			return "only stack/" + home + " may import " + backend + " outside tests"
		}
	}
	return ""
}

func TestImportVerdictClassifiesEveryRule(t *testing.T) {
	for _, tc := range []struct {
		dir, path string
		test      bool
		forbidden bool
	}{
		{".", "github.com/looprig/factory", false, false},
		{".", "github.com/looprig/host/harnessruntime", false, false},
		{".", "github.com/looprig/tools/bash", false, true},
		{".", "github.com/looprig/tools", true, true},
		{"examples/browser-app", "github.com/looprig/llm/auto", false, true},
		{".", "github.com/looprig/sandbox", false, true},
		{".", "github.com/looprig/toolsmith", false, false}, // a different module
		{".", "github.com/looprig/fsstore", false, true},
		{".", "github.com/looprig/fsstore", true, false},
		{"localdisk", "github.com/looprig/fsstore", false, false},
		{"memory", "github.com/looprig/fsstore", false, true},
		{".", "github.com/looprig/storage/memstore", false, true},
		{"memory", "github.com/looprig/storage/memstore", false, false},
		{".", "github.com/looprig/storage", false, false},
		{".", "github.com/looprig/storage/memstorex", false, false},
	} {
		if got := importVerdict(tc.dir, tc.test, tc.path) != ""; got != tc.forbidden {
			t.Errorf("importVerdict(%q, test=%v, %q) forbidden = %v, want %v", tc.dir, tc.test, tc.path, got, tc.forbidden)
		}
	}
}

func TestModuleImportsStayWithinBoundary(t *testing.T) {
	fset := token.NewFileSet()
	files, production := 0, 0
	dirs := map[string]bool{}
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != "." && (strings.HasPrefix(entry.Name(), ".") || entry.Name() == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		parsed, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		files++
		test := strings.HasSuffix(path, "_test.go")
		if !test {
			production++
		}
		dir := filepath.ToSlash(filepath.Dir(path))
		dirs[dir] = true
		for _, spec := range parsed.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if reason := importVerdict(dir, test, imported); reason != "" {
				t.Errorf("%s imports %s: %s", path, imported, reason)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A guard that saw nothing proves nothing.
	if files == 0 || production == 0 {
		t.Fatalf("scanned %d files (%d production); the guard reached nothing", files, production)
	}
	for _, want := range []string{".", "localdisk", "memory", "devauth", "examples/browser-app"} {
		if !dirs[want] {
			t.Errorf("the scan never reached %s", want)
		}
	}
}

// publishedPins are the exact looprig versions this module names. A version
// must exist on its module's remote before it is named here.
var publishedPins = map[string]string{
	"core":         "v0.13.1",
	"factory":      "v0.15.0",
	"fsstore":      "v0.6.0",
	"harness":      "v0.44.0",
	"host":         "v0.16.0", // the first with host/harnessruntime
	"inference":    "v0.15.0",
	"sessionstore": "v0.14.0",
	"storage":      "v0.9.0",
}

func TestModuleContract(t *testing.T) {
	mod, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	sawGo := false
	for _, line := range bytes.Split(mod, []byte{'\n'}) {
		fields := strings.Fields(strings.TrimPrefix(strings.TrimSpace(string(line)), "require "))
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "module":
			if len(fields) < 2 || fields[1] != "github.com/looprig/stack" {
				t.Errorf("module line = %q", line)
			}
		case "go":
			sawGo = true
			if len(fields) < 2 || fields[1] != "1.26.8" {
				t.Errorf("go directive = %q, want 1.26.8", line)
			}
		case "toolchain", "replace":
			t.Errorf("go.mod must not carry a %s directive: %q", fields[0], line)
		}
		if module, ok := strings.CutPrefix(fields[0], looprigPrefix); ok && len(fields) >= 2 {
			want, pinned := publishedPins[module]
			if !pinned {
				t.Errorf("go.mod names %s %s, which is not in publishedPins", fields[0], fields[1])
				continue
			}
			if fields[1] != want {
				t.Errorf("go.mod pins %s %s, want %s", fields[0], fields[1], want)
			}
			seen[module] = true
		}
	}
	if !sawGo {
		t.Error("go.mod has no go directive")
	}
	for module := range publishedPins {
		if !seen[module] {
			t.Errorf("go.mod does not require %s%s", looprigPrefix, module)
		}
	}
	if _, err := os.Stat("vendor"); err == nil {
		t.Error("the module must not vendor dependencies")
	}
}
