package httpapi_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/httpapi"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store/memstore"
	"gopkg.in/yaml.v3"
)

var httpMethods = map[string]bool{
	"get":     true,
	"post":    true,
	"put":     true,
	"patch":   true,
	"delete":  true,
	"head":    true,
	"options": true,
	"trace":   true,
}

var pathPlaceholderRe = regexp.MustCompile(`\{([^}]+)\}`)

func TestOpenAPISpecMatchesMuxAndContracts(t *testing.T) {
	specPath := filepath.Join("..", "..", "api", "openapi.yaml")
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read %s: %v", specPath, err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}

	if v, _ := doc["openapi"].(string); !strings.HasPrefix(v, "3.1.") {
		t.Fatalf("expected OpenAPI 3.1.x, got %q", v)
	}
	info, ok := doc["info"].(map[string]any)
	if !ok || info["title"] == "" || info["version"] == "" {
		t.Fatalf("missing info.title or info.version: %+v", info)
	}

	components, ok := doc["components"].(map[string]any)
	if !ok {
		t.Fatal("missing components section")
	}
	secSchemes, ok := components["securitySchemes"].(map[string]any)
	if !ok {
		t.Fatal("missing components.securitySchemes")
	}
	bearer, ok := secSchemes["bearerAuth"].(map[string]any)
	if !ok || bearer["type"] != "http" || bearer["scheme"] != "bearer" {
		t.Fatalf("expected http bearerAuth security scheme, got %+v", bearer)
	}

	schemas, ok := components["schemas"].(map[string]any)
	if !ok {
		t.Fatal("missing components.schemas")
	}
	if _, ok := schemas["ErrorEnvelope"].(map[string]any); !ok {
		t.Fatal("missing components.schemas.ErrorEnvelope")
	}

	// Verify every writeErr(..., "<CODE>", ...) in internal/httpapi/*.go is
	// documented in components.schemas.ErrorCode.enum.
	errCodeSchema, ok := schemas["ErrorCode"].(map[string]any)
	if !ok {
		t.Fatal("missing components.schemas.ErrorCode")
	}
	enumRaw, ok := errCodeSchema["enum"].([]any)
	if !ok || len(enumRaw) == 0 {
		t.Fatal("ErrorCode.enum must be a non-empty list")
	}
	specErrCodes := map[string]bool{}
	for _, v := range enumRaw {
		if s, ok := v.(string); ok {
			specErrCodes[s] = true
		}
	}
	codeErrCodes := extractWriteErrCodes(t, ".")
	for code := range codeErrCodes {
		if !specErrCodes[code] {
			t.Errorf("error code %q used in httpapi writeErr(...) is missing from components.schemas.ErrorCode.enum", code)
		}
	}

	// Verify every $ref anywhere in the document resolves cleanly.
	assertAllRefsResolve(t, doc, doc)

	// Build a production (Dev=false) Server and walk its registered ServeMux patterns.
	st := memstore.New()
	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "v1")
	if err != nil {
		t.Fatal(err)
	}
	pay := mock.NewPayments()
	vend := mock.NewVending()
	remote := mock.NewRemoteCredit()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := registry.New(st, box, vend, remote)
	orch := orchestrator.New(orchestrator.DefaultConfig(), st, reg, box, pay, vend, remote, log)

	srv := &httpapi.Server{
		O:        orch,
		Auth:     auth.DevAuth{},
		Payments: pay,
		Log:      log,
		Dev:      false,
	}
	_ = srv.Routes()

	muxSet := map[string]bool{}
	for _, pat := range srv.RegisteredPatterns() {
		muxSet[pat] = true
	}
	if len(muxSet) == 0 {
		t.Fatal("Server.RegisteredPatterns() returned 0 routes")
	}

	paths, ok := doc["paths"].(map[string]any)
	if !ok || len(paths) == 0 {
		t.Fatal("missing paths in openapi.yaml")
	}

	specSet := map[string]bool{}
	for routePath, itemRaw := range paths {
		if strings.HasPrefix(routePath, "/v1/dev/") {
			t.Errorf("dev-only route %q must not appear in production openapi.yaml", routePath)
		}
		if routePath == "/metrics" {
			t.Errorf("/metrics is internal-only and must not appear on the public OpenAPI spec")
		}
		item, ok := itemRaw.(map[string]any)
		if !ok {
			t.Fatalf("path %q is not an object", routePath)
		}
		pathParams := collectParamNames(t, doc, item["parameters"], "path")

		for k, opRaw := range item {
			method := strings.ToLower(k)
			if !httpMethods[method] {
				continue
			}
			key := strings.ToUpper(method) + " " + routePath
			specSet[key] = true

			op, ok := opRaw.(map[string]any)
			if !ok {
				t.Fatalf("%s operation is not an object", key)
			}
			if id, _ := op["operationId"].(string); strings.TrimSpace(id) == "" {
				t.Errorf("%s is missing operationId", key)
			}
			resps, ok := op["responses"].(map[string]any)
			if !ok || len(resps) == 0 {
				t.Errorf("%s defines no responses", key)
			}

			// Verify every {placeholder} in the path is declared as a path parameter.
			opPathParams := collectParamNames(t, doc, op["parameters"], "path")
			for _, m := range pathPlaceholderRe.FindAllStringSubmatch(routePath, -1) {
				paramName := m[1]
				if !pathParams[paramName] && !opPathParams[paramName] {
					t.Errorf("%s is missing path parameter definition for {%s}", key, paramName)
				}
			}
		}
	}

	missingInSpec, extraInSpec := diffRouteSets(muxSet, specSet)
	if len(missingInSpec) > 0 {
		t.Errorf("mux routes missing from api/openapi.yaml: %v", missingInSpec)
	}
	if len(extraInSpec) > 0 {
		t.Errorf("api/openapi.yaml routes that do not exist on production mux: %v", extraInSpec)
	}

	for _, want := range p2p4AdminRoutes {
		if !muxSet[want] {
			t.Errorf("P2–P4 admin route missing from mux: %s", want)
		}
		if !specSet[want] {
			t.Errorf("P2–P4 admin route missing from api/openapi.yaml: %s", want)
		}
	}
}

// P2 reconciliation, P3 risk/disputes, P4 pilot — must stay on the production mux and OpenAPI.
var p2p4AdminRoutes = []string{
	"GET /v1/admin/reconciliation/exceptions",
	"GET /v1/admin/reconciliation/exceptions/{id}",
	"POST /v1/admin/reconciliation/exceptions/{id}/assign",
	"POST /v1/admin/reconciliation/exceptions/{id}/resolve",
	"POST /v1/admin/reconciliation/exceptions/{id}/adjustments",
	"POST /v1/admin/reconciliation/adjustments/{id}/approve",
	"GET /v1/admin/reconciliation/summary",
	"POST /v1/admin/reconciliation/wallet-topup",
	"GET /v1/admin/risk/rules",
	"PUT /v1/admin/risk/rules/{code}",
	"GET /v1/admin/risk/denylist",
	"POST /v1/admin/risk/denylist",
	"DELETE /v1/admin/risk/denylist/{id}",
	"GET /v1/admin/disputes",
	"GET /v1/admin/disputes/{id}",
	"GET /v1/admin/disputes/{id}/evidence",
	"GET /v1/admin/pilot",
	"POST /v1/admin/pilot/allowlist",
	"DELETE /v1/admin/pilot/allowlist",
	"POST /v1/admin/pilot/pause",
	"POST /v1/admin/pilot/resume",
}

func TestOpenAPIDriftDetection(t *testing.T) {
	mux := map[string]bool{"GET /healthz": true, "POST /v1/meters": true}
	spec := map[string]bool{"GET /healthz": true, "DELETE /v1/phantom": true}
	missing, extra := diffRouteSets(mux, spec)
	if len(missing) != 1 || missing[0] != "POST /v1/meters" {
		t.Fatalf("expected missing [POST /v1/meters], got %v", missing)
	}
	if len(extra) != 1 || extra[0] != "DELETE /v1/phantom" {
		t.Fatalf("expected extra [DELETE /v1/phantom], got %v", extra)
	}
}

func diffRouteSets(muxSet, specSet map[string]bool) (missingInSpec, extraInSpec []string) {
	for pat := range muxSet {
		if !specSet[pat] {
			missingInSpec = append(missingInSpec, pat)
		}
	}
	sort.Strings(missingInSpec)
	for pat := range specSet {
		if !muxSet[pat] {
			extraInSpec = append(extraInSpec, pat)
		}
	}
	sort.Strings(extraInSpec)
	return missingInSpec, extraInSpec
}

func assertAllRefsResolve(t *testing.T, root map[string]any, node any) {
	t.Helper()
	switch v := node.(type) {
	case map[string]any:
		if ref, ok := v["$ref"].(string); ok {
			if resolveRef(root, ref) == nil {
				t.Errorf("unresolved $ref %q", ref)
			}
		}
		for _, child := range v {
			assertAllRefsResolve(t, root, child)
		}
	case []any:
		for _, elem := range v {
			assertAllRefsResolve(t, root, elem)
		}
	}
}

func resolveRef(root map[string]any, ref string) any {
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}
	var cur any = root
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = m[part]
		if !ok {
			return nil
		}
	}
	return cur
}

func collectParamNames(t *testing.T, root map[string]any, raw any, location string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	list, ok := raw.([]any)
	if !ok {
		return out
	}
	for _, entry := range list {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if ref, ok := m["$ref"].(string); ok {
			resolved, ok := resolveRef(root, ref).(map[string]any)
			if !ok {
				t.Fatalf("invalid parameter $ref %q", ref)
			}
			m = resolved
		}
		if m["in"] == location {
			if name, ok := m["name"].(string); ok && name != "" {
				out[name] = true
			}
		}
	}
	return out
}

func extractWriteErrCodes(t *testing.T, dir string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 3 {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok || ident.Name != "writeErr" {
				return true
			}
			lit, ok := call.Args[2].(*ast.BasicLit)
			if ok && lit.Kind == token.STRING {
				codes[strings.Trim(lit.Value, `"`)] = true
			}
			return true
		})
	}
	return codes
}
