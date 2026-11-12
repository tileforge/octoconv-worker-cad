package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
