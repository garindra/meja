package server

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type synchronizedField struct {
	packageName string
	typeName    string
	fieldName   string
	syncType    string
}

func (f synchronizedField) key() string {
	return f.packageName + "." + f.typeName + "." + f.fieldName
}

// Every atomic field is a deliberate cross-goroutine publication point:
//
// server.Pane.metadata
//
//	owner: pane actor; writers: pane actor; readers: client/input actors;
//	published value: immutable terminal metadata; actor messaging would add a
//	synchronous round trip to every input-routing hot path.
//
// server.Pane.stopping
//
//	owner: pane lifecycle; writers/readers: PTY, process, and teardown workers;
//	published value: monotonic teardown fence; no single actor remains alive
//	for the whole teardown interval.
//
// server.Pane.processActivityPending, server.processActivity.pending, and
// server.processWatch.activityPending
//
//	owner: process-monitor edge protocol; writers/readers: pane producers and
//	monitor worker; published value: one outstanding activity edge; actor
//	messaging alone cannot make producer-side enqueue nonblocking and bounded.
//
// server.paneRenderMetrics.*
//
//	owner: pane render pipeline; writers: pane actor, PTY adapter cancellation,
//	and its single confirmer;
//	readers: diagnostics and tests; published values: monotonic counters for
//	semantic publication, output backpressure, and physical confirmation.
//
// client.runtimeState.dropConnectionEvents
//
//	owner: frontend runtime; writer: reconnect coordinator; readers: connection
//	workers; published value: reconnect event fence; workers must stop emitting
//	before the UI actor accepts the replacement.
//
// client.runtimeState.appliedLayoutRevision
//
//	owner: frontend runtime; writer: display actor; readers: input/resize
//	forwarders; published value: immutable installed-layout revision; forwarding
//	must stamp input without a synchronous UI round trip.
//
// client.runtimeState.promptInputStatus
//
//	owner: frontend runtime; writer: control decoder; reader: terminal-input
//	forwarder; published value: the greatest status revision observed together
//	with whether raw terminal input must be routed through the display actor's
//	client-local prompt editor instead of to the server.
//
// client.liveConnection.lastContact
//
//	owner: live connection; writers: connection readers; reader: watchdog;
//	published value: monotonic contact timestamp; actor messaging would turn a
//	watchdog heartbeat into additional queued traffic.
var atomicFieldContract = map[string]string{
	"server.Pane.metadata":                                "atomic.Pointer",
	"server.Pane.stopping":                                "atomic.Bool",
	"server.Pane.processActivityPending":                  "atomic.Bool",
	"server.processActivity.pending":                      "*atomic.Bool",
	"server.processWatch.activityPending":                 "*atomic.Bool",
	"server.paneRenderMetrics.ptyBytes":                   "atomic.Uint64",
	"server.paneRenderMetrics.ptyDrainOpportunities":      "atomic.Uint64",
	"server.paneRenderMetrics.ptyDrainsCompleted":         "atomic.Uint64",
	"server.paneRenderMetrics.ptyDrainReads":              "atomic.Uint64",
	"server.paneRenderMetrics.ptyDrainDurationNanos":      "atomic.Uint64",
	"server.paneRenderMetrics.ptyDrainStoppedEmpty":       "atomic.Uint64",
	"server.paneRenderMetrics.ptyDrainStoppedByteBudget":  "atomic.Uint64",
	"server.paneRenderMetrics.ptyDrainStoppedTimeBudget":  "atomic.Uint64",
	"server.paneRenderMetrics.ptyDrainStoppedEOF":         "atomic.Uint64",
	"server.paneRenderMetrics.ptyDrainStoppedError":       "atomic.Uint64",
	"server.paneRenderMetrics.ptyDrainStoppedCancelled":   "atomic.Uint64",
	"server.paneRenderMetrics.ptyDrainPublications":       "atomic.Uint64",
	"server.paneRenderMetrics.ptyDrainPresents":           "atomic.Uint64",
	"server.paneRenderMetrics.ptyDrainCancelledCells":     "atomic.Uint64",
	"server.paneRenderMetrics.publications":               "atomic.Uint64",
	"server.paneRenderMetrics.presents":                   "atomic.Uint64",
	"server.paneRenderMetrics.candidateCells":             "atomic.Uint64",
	"server.paneRenderMetrics.changedCells":               "atomic.Uint64",
	"server.paneRenderMetrics.changedRuns":                "atomic.Uint64",
	"server.paneRenderMetrics.keyframes":                  "atomic.Uint64",
	"server.paneRenderMetrics.deltas":                     "atomic.Uint64",
	"server.paneRenderMetrics.uncompressedBytes":          "atomic.Uint64",
	"server.paneRenderMetrics.physicalWrites":             "atomic.Uint64",
	"server.paneRenderMetrics.publicationBufferStarved":   "atomic.Uint64",
	"server.paneRenderMetrics.confirmerWriteBlockedNanos": "atomic.Uint64",
	"server.paneRenderMetrics.cancelledCells":             "atomic.Uint64",
	"client.runtimeState.dropConnectionEvents":            "atomic.Bool",
	"client.runtimeState.appliedLayoutRevision":           "atomic.Uint64",
	"client.runtimeState.promptInputStatus":               "atomic.Pointer",
	"client.liveConnection.lastContact":                   "atomic.Int64",
}

func TestProductionSynchronizationOwnershipContract(t *testing.T) {
	t.Helper()
	// Loading the canonical packages through go/types ensures the contract is
	// anchored to named standard-library synchronization types, while the AST
	// scan preserves source field names and excludes test files deterministically.
	typeImporter := importer.Default()
	for _, packagePath := range []string{"sync", "sync/atomic"} {
		pkg, err := typeImporter.Import(packagePath)
		if err != nil {
			t.Fatalf("load %s types: %v", packagePath, err)
		}
		if pkg.Scope().Lookup("Mutex") == nil && packagePath == "sync" {
			t.Fatalf("sync.Mutex type is unavailable")
		}
		if pkg.Scope().Lookup("Bool") == nil && packagePath == "sync/atomic" {
			t.Fatalf("atomic.Bool type is unavailable")
		}
		objectName := "Mutex"
		if packagePath == "sync/atomic" {
			objectName = "Bool"
		}
		if rendered := types.TypeString(pkg.Scope().Lookup(objectName).Type(), nil); rendered == "" {
			t.Fatalf("%s.%s has no named type", packagePath, objectName)
		}
	}

	fields, forbidden, sessionStateMethod, identityPointerFields := scanProductionOwnership(t)
	actualAtomics := make(map[string]string)
	typeFields := make(map[string][]synchronizedField)
	for _, field := range fields {
		typeKey := field.packageName + "." + field.typeName
		typeFields[typeKey] = append(typeFields[typeKey], field)
		if strings.Contains(field.syncType, "atomic.") {
			actualAtomics[field.key()] = field.syncType
		}
	}
	if diff := exactStringMapDiff(atomicFieldContract, actualAtomics); diff != "" {
		t.Fatalf("repository atomic ownership contract changed:\n%s", diff)
	}

	for _, typeKey := range []string{
		"server.SessionState",
		"server.GroupState",
		"server.Window",
		"server.ClientIdentity",
		"server.ClientInstance",
	} {
		if synchronized := typeFields[typeKey]; len(synchronized) != 0 {
			t.Fatalf("%s has synchronized fields: %v", typeKey, synchronized)
		}
	}
	var daemonMutexes []string
	for _, field := range typeFields["server.Daemon"] {
		if field.syncType == "sync.Map" || strings.Contains(field.syncType, "atomic.") {
			t.Fatalf("Daemon has forbidden synchronized field %s %s", field.fieldName, field.syncType)
		}
		if field.syncType == "sync.Mutex" || field.syncType == "sync.RWMutex" {
			daemonMutexes = append(daemonMutexes, field.fieldName+" "+field.syncType)
		}
	}
	sort.Strings(daemonMutexes)
	if got, want := strings.Join(daemonMutexes, ", "), "logMu sync.Mutex, quicMu sync.Mutex"; got != want {
		t.Fatalf("Daemon mutex contract = %q, want %q", got, want)
	}
	if len(identityPointerFields) != 0 {
		t.Fatalf("ClientInstance retains daemon-owned *ClientIdentity fields: %v", identityPointerFields)
	}
	if sessionStateMethod {
		t.Fatal("production reintroduced (*ClientInstance).sessionState() *SessionState")
	}
	if len(forbidden) != 0 {
		t.Fatalf("production reintroduced removed secondary indexes: %v", forbidden)
	}
}

func scanProductionOwnership(t *testing.T) ([]synchronizedField, []string, bool, []string) {
	t.Helper()
	fset := token.NewFileSet()
	var synchronized []synchronizedField
	var forbidden []string
	var sessionStateMethod bool
	var identityPointerFields []string
	for _, directory := range []string{".", filepath.Join("..", "client")} {
		entries, err := filepath.Glob(filepath.Join(directory, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, filename := range entries {
			if strings.HasSuffix(filename, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, filename, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", filename, err)
			}
			imports := importedAliases(file)
			for _, declaration := range file.Decls {
				switch declaration := declaration.(type) {
				case *ast.GenDecl:
					for _, spec := range declaration.Specs {
						typeSpec, ok := spec.(*ast.TypeSpec)
						if !ok {
							continue
						}
						structType, ok := typeSpec.Type.(*ast.StructType)
						if !ok {
							continue
						}
						for _, field := range structType.Fields.List {
							syncType := synchronizationType(field.Type, imports)
							for _, name := range field.Names {
								switch name.Name {
								case "sessionIndex", "groupIndex", "paneIndex", "windowIndex":
									forbidden = append(forbidden, name.Name+"@"+fset.Position(name.Pos()).String())
								}
								if syncType != "" {
									synchronized = append(synchronized, synchronizedField{
										packageName: file.Name.Name,
										typeName:    typeSpec.Name.Name,
										fieldName:   name.Name,
										syncType:    syncType,
									})
								}
								if typeSpec.Name.Name == "ClientInstance" && isPointerToNamed(field.Type, "ClientIdentity") {
									identityPointerFields = append(identityPointerFields, name.Name)
								}
							}
						}
					}
				case *ast.FuncDecl:
					if declaration.Name.Name == "sessionState" &&
						receiverNamed(declaration.Recv, "ClientInstance") &&
						returnsPointerToNamed(declaration.Type.Results, "SessionState") {
						sessionStateMethod = true
					}
				}
			}
		}
	}
	sort.Strings(forbidden)
	sort.Strings(identityPointerFields)
	return synchronized, forbidden, sessionStateMethod, identityPointerFields
}

func importedAliases(file *ast.File) map[string]string {
	result := make(map[string]string)
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := filepath.Base(path)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		result[name] = path
	}
	return result
}

func synchronizationType(expression ast.Expr, imports map[string]string) string {
	pointer := false
	if star, ok := expression.(*ast.StarExpr); ok {
		pointer = true
		expression = star.X
	}
	switch generic := expression.(type) {
	case *ast.IndexExpr:
		expression = generic.X
	case *ast.IndexListExpr:
		expression = generic.X
	}
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	qualifier, ok := selector.X.(*ast.Ident)
	if !ok {
		return ""
	}
	path := imports[qualifier.Name]
	var result string
	switch {
	case path == "sync/atomic":
		result = "atomic." + selector.Sel.Name
	case path == "sync" && (selector.Sel.Name == "Map" || selector.Sel.Name == "Mutex" || selector.Sel.Name == "RWMutex"):
		result = "sync." + selector.Sel.Name
	default:
		return ""
	}
	if pointer {
		return "*" + result
	}
	return result
}

func isPointerToNamed(expression ast.Expr, name string) bool {
	star, ok := expression.(*ast.StarExpr)
	if !ok {
		return false
	}
	ident, ok := star.X.(*ast.Ident)
	return ok && ident.Name == name
}

func receiverNamed(fields *ast.FieldList, name string) bool {
	if fields == nil || len(fields.List) != 1 {
		return false
	}
	expression := fields.List[0].Type
	if star, ok := expression.(*ast.StarExpr); ok {
		expression = star.X
	}
	ident, ok := expression.(*ast.Ident)
	return ok && ident.Name == name
}

func returnsPointerToNamed(fields *ast.FieldList, name string) bool {
	return fields != nil && len(fields.List) == 1 && isPointerToNamed(fields.List[0].Type, name)
}

func exactStringMapDiff(want, got map[string]string) string {
	var lines []string
	for key, wantType := range want {
		switch gotType, ok := got[key]; {
		case !ok:
			lines = append(lines, "- missing "+key+" "+wantType)
		case gotType != wantType:
			lines = append(lines, "~ "+key+" got "+gotType+" want "+wantType)
		}
	}
	for key, gotType := range got {
		if _, ok := want[key]; !ok {
			lines = append(lines, "+ unexpected "+key+" "+gotType)
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
