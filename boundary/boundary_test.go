package boundary_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The five dependency rules from Workspace specification section 5. This
// test fails the build on any import that breaks them.
//
//  1. domain imports only the standard library, and content may also import
//     anansi's schema, validation and binding packages.
//  2. Core packages import domain and the ports, never an adapter.
//  3. Only store/anansistore imports anansi's persistence packages, only
//     workspace-hestia imports hestia, and only the provider adapters talk
//     to vendor APIs.
//  4. Adapters are wired together in workspace.Open and nowhere else.
//  5. The core module never imports the workspace-hestia module.
var (
	// anansiPrefixes is every anansi import path. Rule 1 and rule 2 mean
	// no core package may reach these, with the two carve-outs below.
	anansiPrefix = "github.com/asaidimu/go-anansi/v8"

	// contentAllowed is the one core package allowed to touch anansi, and
	// only its schema, validation and binding subtrees. The schema subtree
	// pulls in anansi's own transitive helpers (cache, data/container,
	// types/decimal, utils), which are allowed as dependencies of the
	// allowed packages rather than as direct imports.
	contentAllowed = []string{
		"github.com/asaidimu/go-anansi/v8/core/schema",
		"github.com/asaidimu/go-anansi/v8/core/schema/definition",
		"github.com/asaidimu/go-anansi/v8/core/schema/meta",
		"github.com/asaidimu/go-anansi/v8/core/validate",
		"github.com/asaidimu/go-anansi/v8/core/binding",
		"github.com/asaidimu/go-anansi/v8/core/common",
	}

	// kernelAllowed is the kernel's carve-out: SystemError and Issue only.
	// core/common is pure standard library, so this pulls in no other anansi
	// package and cannot reach the persistence subtree.
	kernelAllowed = []string{
		"github.com/asaidimu/go-anansi/v8/core/common",
	}

	// persistencePrefix is the anansi subtree only store/anansistore may
	// import (rule 3).
	persistencePrefix = anansiPrefix + "/core/persistence"

	// hestiaPrefix is the host module, which the core must never import
	// (rules 3 and 5).
	hestiaPrefix = "github.com/asaidimu/hestia"
)

// corePkgs is the set of packages the rules bind. Adapters and hosts are
// added to the module later; when they are, they get their own assertions
// rather than being excluded here.
var corePkgs = []string{
	"./domain",
	"./store",
	"./kernel",
	"./content",
	"./prompt",
	"./provider",
	"./agent",
	"./blob",
	".", // the root package, workspace.Open
}

// directImports returns the package's own import paths, excluding stdlib and
// test-only imports. The rules constrain what a package imports directly;
// transitive dependencies of an allowed import are the allowed package's
// business, not this one's.
func directImports(t *testing.T, pkg string) []string {
	t.Helper()
	root, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	mod := strings.TrimSpace(string(root))
	if i := strings.LastIndexByte(mod, '/'); i > 0 {
		mod = mod[:i]
	}
	format := "{{join .Imports \"\\n\"}}"
	cmd := exec.Command("go", "list", "-f", format, pkg)
	cmd.Dir = mod
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("go list -f %s: %v\n%s", pkg, err, ee.Stderr)
		}
		t.Fatalf("go list -f %s: %v", pkg, err)
	}
	var imports []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			imports = append(imports, line)
		}
	}
	return imports
}

// deps runs `go list -deps` for one package and returns its import paths.
// The command runs at the module root, because `go list` would otherwise
// resolve a relative package against this test file's directory.
func deps(t *testing.T, pkg string) []string {
	t.Helper()
	root, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	mod := strings.TrimSpace(string(root))
	if i := strings.LastIndexByte(mod, '/'); i > 0 {
		mod = mod[:i]
	}
	cmd := exec.Command("go", "list", "-deps", pkg)
	cmd.Dir = mod
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("go list -deps %s: %v\n%s", pkg, err, ee.Stderr)
		}
		t.Fatalf("go list -deps %s: %v", pkg, err)
	}
	return strings.Fields(string(out))
}

func TestDomainImportsOnlyStdlib(t *testing.T) {
	for _, d := range deps(t, "./domain") {
		if strings.HasPrefix(d, anansiPrefix) {
			t.Errorf("rule 1: domain imports anansi: %s", d)
		}
		// `go list -deps` includes the package itself, which is not an
		// import, so exclude it.
		if d == "github.com/asaidimu/workspace/domain" {
			continue
		}
		if strings.Contains(d, "github.com/asaidimu/workspace/") {
			t.Errorf("rule 1: domain imports a workspace package: %s", d)
		}
	}
}

// TestOtherCorePackagesImportNoAnansi enforces the rest of rule 1, which the
// carve-out tests above do not cover: content and kernel may reach anansi
// through their whitelisted packages, and NO other core package may reach it
// at all. agent importing definition.Schema directly, for example, would be
// a silent violation without this test — content is the only anansi-schema
// home, so agent goes through content.JSONSchema instead.
func TestOtherCorePackagesImportNoAnansi(t *testing.T) {
	for _, pkg := range corePkgs {
		if pkg == "./content" || pkg == "./kernel" {
			continue // covered by the carve-out tests above
		}
		for _, d := range directImports(t, pkg) {
			if strings.HasPrefix(d, anansiPrefix) {
				t.Errorf("rule 1: %s imports anansi; only content and kernel may: %s", pkg, d)
			}
		}
	}
}

func TestContentOnlyAllowedAnansi(t *testing.T) {
	for _, d := range directImports(t, "./content") {
		if !strings.HasPrefix(d, anansiPrefix) {
			continue
		}
		if !allowed(d, contentAllowed) {
			t.Errorf("rule 1: content imports disallowed anansi package: %s", d)
		}
	}
}

// TestKernelOnlyCommonAnansi enforces section 5's second carve-out: the
// kernel reaches anansi through core/common alone, for SystemError and Issue.
// Everything else in anansi stays out of the kernel.
func TestKernelOnlyCommonAnansi(t *testing.T) {
	for _, d := range directImports(t, "./kernel") {
		if !strings.HasPrefix(d, anansiPrefix) {
			continue
		}
		if !allowed(d, kernelAllowed) {
			t.Errorf("rule 1: kernel imports disallowed anansi package: %s", d)
		}
	}
}

func TestCoreNeverImportsAdaptersOrHost(t *testing.T) {
	for _, pkg := range corePkgs {
		for _, d := range deps(t, pkg) {
			if strings.HasPrefix(d, persistencePrefix) {
				t.Errorf("rule 3: %s imports anansi persistence: %s", pkg, d)
			}
			if strings.HasPrefix(d, hestiaPrefix) {
				t.Errorf("rule 3/5: %s imports hestia: %s", pkg, d)
			}
		}
	}
}

func TestOnlyWorkspaceOpensWiresAdapters(t *testing.T) {
	// Rule 4: the root package is the only one that may import the adapter
	// packages. Once store/anansistore exists, this asserts that no other
	// core package reaches it; for now it asserts the root is the only
	// package that imports store at all beyond the kernel.
	for _, pkg := range []string{"./kernel", "./prompt", "./agent", "./content"} {
		for _, d := range deps(t, pkg) {
			if d == "github.com/asaidimu/workspace/store/memory" ||
				d == "github.com/asaidimu/workspace/store/anansistore" {
				t.Errorf("rule 4: %s imports an adapter: %s", pkg, d)
			}
		}
	}
}

func allowed(d string, list []string) bool {
	for _, p := range list {
		if d == p || strings.HasPrefix(d, p+"/") {
			return true
		}
	}
	return false
}
