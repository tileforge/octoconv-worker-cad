package main

import (
	"context"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tileforge/octoconv-worker-cad/internal/convert"
)

func TestProtocolDialReadsCadVariables(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "cert.pem")
	key := filepath.Join(dir, "key.pem")
	ca := filepath.Join(dir, "ca.pem")
	manifest := filepath.Join(dir, "manifest.json")
	for _, path := range []string{cert, key, ca, manifest} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	id := uuid.New()
	t.Setenv("CAD_PROTOCOL_CORE_URL", "https://core.example")
	t.Setenv("CAD_PROTOCOL_CLIENT_CERT", cert)
	t.Setenv("CAD_PROTOCOL_CLIENT_KEY", key)
	t.Setenv("CAD_PROTOCOL_CLIENT_CA", ca)
	t.Setenv("CAD_PROTOCOL_WORKER_ID", id.String())
	t.Setenv("CAD_PROTOCOL_MANIFEST_PATH", manifest)
	t.Setenv("CAD_WORKER_CONCURRENCY", "2")
	dial, err := loadProtocolDial()
	if err != nil {
		t.Fatal(err)
	}
	if !dial.ready || dial.workerID != id || dial.freeSlots != 2 || dial.core.Host != "core.example" {
		t.Fatalf("dial %+v", dial)
	}
}

func TestProtocolDialRejectsPartialEnvironment(t *testing.T) {
	t.Setenv("CAD_PROTOCOL_CORE_URL", "https://core.example")
	if _, err := loadProtocolDial(); err == nil {
		t.Fatal("partial dial")
	}
}

func TestRefuseLegacyStores(t *testing.T) {
	if err := refuseLegacyStores(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_URL", "postgres://db.example/octo")
	if err := refuseLegacyStores(); err == nil {
		t.Fatal("database url")
	}
	t.Setenv("DATABASE_URL", "")
	t.Setenv("REDIS_ADDR", "redis.example:6379")
	if err := refuseLegacyStores(); err == nil {
		t.Fatal("redis")
	}
}

func TestManifestOperation(t *testing.T) {
	convertManifest, err := os.ReadFile(filepath.Join("..", "..", "schema", "manifest-cad-convert.json"))
	if err != nil {
		t.Fatal(err)
	}
	renderManifest, err := os.ReadFile(filepath.Join("..", "..", "schema", "manifest-cad-render.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := manifestOperation(convertManifest)
	if err != nil || got != convert.OperationConvert {
		t.Fatalf("convert operation %q %v", got, err)
	}
	got, err = manifestOperation(renderManifest)
	if err != nil || got != convert.OperationRender {
		t.Fatalf("render operation %q %v", got, err)
	}
}

func TestCadEngineCode(t *testing.T) {
	if cadEngineCode(convert.ErrCADMeshTimeout) != "cad_mesh_timeout" {
		t.Fatal("mesh timeout")
	}
	if cadEngineCode(convert.ErrCADUnreadable) != "cad_unreadable" {
		t.Fatal("unreadable")
	}
	if cadEngineCode(convert.ErrEngineSandboxUnavailable) != "timeout" {
		t.Fatal("sandbox")
	}
}

func TestWorkerSourcesDoNotImportStores(t *testing.T) {
	forbidden := []string{
		"database/sql",
		"github.com/jackc/pgx",
		"github.com/redis/go-redis",
		"github.com/hibiken/asynq",
		"github.com/minio/minio-go",
	}
	set := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		file, err := parser.ParseFile(set, entry.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			path := strings.Trim(spec.Path.Value, `"`)
			for _, bad := range forbidden {
				if path == bad || strings.HasPrefix(path, bad+"/") {
					t.Fatalf("%s imports %s", entry.Name(), path)
				}
			}
		}
	}
}

func TestAssignmentOutputs(t *testing.T) {
	primary := fileLinkJSON{Role: "primary", Href: "https://files/primary", Format: "png"}
	measure := fileLinkJSON{Role: "measurements", Href: "https://files/measure", Format: "json"}
	got, extra, ok := assignmentOutputs([]fileLinkJSON{primary})
	if !ok || extra != nil || got.Href != primary.Href {
		t.Fatalf("primary only: %+v extra=%v ok=%v", got, extra, ok)
	}
	_, extra, ok = assignmentOutputs([]fileLinkJSON{primary, measure})
	if !ok || extra == nil || extra.Href != measure.Href {
		t.Fatalf("measurements: %+v ok=%v", extra, ok)
	}
	if _, _, ok = assignmentOutputs([]fileLinkJSON{primary, primary}); ok {
		t.Fatal("two primaries accepted")
	}
	if _, _, ok = assignmentOutputs([]fileLinkJSON{primary, {Role: "sidecar", Href: "https://files/side", Format: "json"}}); ok {
		t.Fatal("sidecar accepted")
	}
	if _, _, ok = assignmentOutputs([]fileLinkJSON{primary, {Role: "measurements", Format: "json"}}); ok {
		t.Fatal("measurements without href accepted")
	}
}

func TestHeartbeatTerminateWakesIdleSession(t *testing.T) {
	c := &protocolClient{term: make(chan struct{})}
	c.applyHeartbeat(controlJSON{Terminate: true})
	if !c.sessionTerminating() {
		t.Fatal("idle session still accepts work")
	}
}

func TestHeartbeatTerminateNotifiesActiveAttempt(t *testing.T) {
	reported := make(chan []byte, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		reported <- raw
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	core, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	flag := &stopFlag{}
	_, cancel := context.WithCancelCause(context.Background())
	c := &protocolClient{
		core:         core,
		coreHTTP:     srv.Client(),
		term:         make(chan struct{}),
		sessionID:    "sess",
		sessionToken: "token",
		active: &liveAttempt{
			parent: context.Background(),
			cancel: cancel,
			flag:   flag,
			asgn:   assignmentJSON{AttemptID: "attempt", AttemptToken: "attempt-token"},
		},
	}
	c.applyHeartbeat(controlJSON{Terminate: true})
	if !c.sessionTerminating() {
		t.Fatal("session still accepts work")
	}
	if flag.outcome() != "terminate" {
		t.Fatalf("outcome %q", flag.outcome())
	}
	var body []byte
	select {
	case body = <-reported:
	case <-time.After(2 * time.Second):
		t.Fatal("stopped request")
	}
	if !strings.Contains(string(body), `"reason":"terminate"`) {
		t.Fatalf("stopped body %s", body)
	}
}
