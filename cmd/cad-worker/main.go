// Command cad-worker runs one approved cad capability over Worker Protocol v2.
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/tileforge/octoconv-worker-cad/internal/convert"
	"github.com/tileforge/octoconv-worker-cad/internal/sandbox"
)

const cadSandboxDisabledEnv = "CAD_SANDBOX_DISABLED"

const cadPreflightTimeout = 60 * time.Second

func main() {
	if sandbox.RunExecModeIfRequested(os.Args) {
		return
	}
	if err := refuseLegacyStores(); err != nil {
		log.Fatalf("cad protocol: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	sandboxHelper, err := sandbox.ResolveHelper(cadSandboxDisabledEnv)
	if err != nil {
		log.Fatalf("cad sandbox: %v", err)
	}
	conv, err := convert.ConfinedCAD(
		sandboxHelper,
		envInt("CAD_WORKER_CONCURRENCY", 1),
		"CAD_WORKER_CONCURRENCY",
		envDuration("CAD_GUARD_TIMEOUT", convert.CADDefaultGuardTimeout),
		envDuration("CAD_MESH_TIMEOUT", convert.CADDefaultMeshTimeout),
		envDuration("CAD_RENDER_TIMEOUT", convert.CADDefaultRenderTimeout),
		envDuration("CAD_DXF_TIMEOUT", convert.CADDefaultDXFTimeout),
		envDuration("CAD_DWG_TIMEOUT", convert.CADDefaultDWGTimeout),
		envDuration("CAD_RSVG_TIMEOUT", convert.CADDefaultRsvgTimeout),
		envDuration("CAD_ENGINE_TIMEOUT", convert.CADDefaultEngineTimeout),
	)
	if err != nil {
		log.Fatalf("cad sandbox: %v", err)
	}
	preflight := convert.CADPreflight
	switch os.Getenv("CAD_STACK") {
	case "model":
		convert.Default.Replace(conv)
		convert.Default.Replace(convert.CADRenderConverter{CADConverter: conv})
	case "drawing":
		convert.Default.Replace(convert.CADDXFConverter{CADConverter: conv})
		convert.Default.Replace(convert.CADDXFRenderConverter{CADConverter: conv})
		preflight = convert.CADPreflightDrawing
	default:
		log.Fatalf("cad protocol: CAD_STACK must be model or drawing")
	}

	preflightCtx, cancelPreflight := context.WithTimeout(ctx, cadPreflightTimeout)
	err = preflight(preflightCtx, conv)
	cancelPreflight()
	if err != nil {
		log.Fatalf("cad sandbox preflight: %v", err)
	}

	dial, err := loadProtocolDial()
	if err != nil {
		log.Fatalf("cad protocol: %v", err)
	}
	log.Printf("📐 cad protocol worker starting")
	if err := runProtocol(ctx, dial); err != nil {
		log.Fatalf("cad protocol: %v", err)
	}
}

func refuseLegacyStores() error {
	if os.Getenv("DATABASE_URL") != "" || os.Getenv("REDIS_ADDR") != "" {
		return errors.New("DATABASE_URL and REDIS_ADDR are not used")
	}
	return nil
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(firstField(v)); err == nil {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(firstField(v)); err == nil {
			return d
		}
	}
	return def
}

func firstField(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' {
			return s[:i]
		}
	}
	return s
}
