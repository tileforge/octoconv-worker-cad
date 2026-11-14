package main

import (
	"context"
	"encoding/json"
	"errors"
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
	drawConvert, err := os.ReadFile(filepath.Join("..", "..", "schema", "manifest-caddraw-convert.json"))
	if err != nil {
		t.Fatal(err)
	}
	drawRender, err := os.ReadFile(filepath.Join("..", "..", "schema", "manifest-caddraw-render.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err = manifestOperation(drawConvert)
	if err != nil || got != convert.OperationConvert {
		t.Fatalf("drawing convert operation %q %v", got, err)
	}
	got, err = manifestOperation(drawRender)
	if err != nil || got != convert.OperationRender {
		t.Fatalf("drawing render operation %q %v", got, err)
	}
}

func TestManifestPairsStayDisjoint(t *testing.T) {
	type pair struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	type document struct {
		Capabilities []struct {
			ID     string `json:"id"`
			Legacy struct {
				Engine string `json:"engine"`
			} `json:"legacy"`
			Formats []pair `json:"formats"`
		} `json:"capabilities"`
	}
	type expect struct {
		id    string
		pairs []pair
	}
	want := map[string]expect{
		"manifest-cad-convert.json": {
			id:    "octoconv.convert.cad",
			pairs: []pair{{From: "step", To: "stl"}, {From: "iges", To: "stl"}},
		},
		"manifest-cad-render.json": {
			id: "octoconv.render.cad",
			pairs: []pair{
				{From: "step", To: "png"},
				{From: "iges", To: "png"},
				{From: "stl", To: "png"},
			},
		},
		"manifest-caddraw-convert.json": {
			id:    "octoconv.convert.caddraw",
			pairs: []pair{{From: "dxf", To: "svg"}, {From: "dxf", To: "pdf"}},
		},
		"manifest-caddraw-render.json": {
			id:    "octoconv.render.caddraw",
			pairs: []pair{{From: "dxf", To: "png"}},
		},
	}
	seen := map[pair]string{}
	for name, spec := range want {
		raw, err := os.ReadFile(filepath.Join("..", "..", "schema", name))
		if err != nil {
			t.Fatal(err)
		}
		var parsed document
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatal(err)
		}
		if len(parsed.Capabilities) != 1 {
			t.Fatalf("%s capabilities = %d", name, len(parsed.Capabilities))
		}
		gotCap := parsed.Capabilities[0]
		if gotCap.ID != spec.id || gotCap.Legacy.Engine != "cad" {
			t.Fatalf("%s id %s engine %s", name, gotCap.ID, gotCap.Legacy.Engine)
		}
		if len(gotCap.Formats) != len(spec.pairs) {
			t.Fatalf("%s formats = %d", name, len(gotCap.Formats))
		}
		got := map[pair]struct{}{}
		for _, format := range gotCap.Formats {
			got[format] = struct{}{}
			if prev, ok := seen[format]; ok {
				t.Fatalf("pair %s to %s in %s and %s", format.From, format.To, prev, name)
			}
			seen[format] = name
		}
		for _, format := range spec.pairs {
			if _, ok := got[format]; !ok {
				t.Fatalf("%s missing %s to %s", name, format.From, format.To)
			}
		}
	}
	if len(seen) != 8 {
		t.Fatalf("pairs = %d", len(seen))
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

type deadlineTrip struct {
	base http.RoundTripper
	ok   bool
	left time.Duration
}

func (d *deadlineTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	if !d.ok {
		if dl, ok := r.Context().Deadline(); ok {
			d.ok = true
			d.left = time.Until(dl)
		}
	}
	return d.base.RoundTrip(r)
}

func protocolTestClient(t *testing.T, handler http.Handler, trip *deadlineTrip) *protocolClient {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	core, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := srv.Client()
	if trip != nil {
		trip.base = httpClient.Transport
		httpClient.Transport = trip
	}
	return &protocolClient{
		core:     core,
		coreHTTP: httpClient,
		term:     make(chan struct{}),
	}
}

func TestHelloRequestHasDeadline(t *testing.T) {
	trip := &deadlineTrip{}
	c := protocolTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"session_id":"sess","session_token":"token"}`)
	}), trip)
	if _, err := c.hello(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !trip.ok {
		t.Fatal("hello has no deadline")
	}
	if trip.left > helloTimeout || trip.left < helloTimeout-2*time.Second {
		t.Fatalf("hello deadline %s", trip.left)
	}
}

func TestPullRequestAllowsLongPoll(t *testing.T) {
	const wait = 20
	trip := &deadlineTrip{}
	c := protocolTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"assignments":[]}`)
	}), trip)
	c.pullWait = wait
	c.sessionID = "sess"
	c.sessionToken = "token"
	if _, err := c.pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !trip.ok {
		t.Fatal("pull has no deadline")
	}
	limit := time.Duration(wait)*time.Second + pullGrace
	if trip.left > limit || trip.left < limit-2*time.Second {
		t.Fatalf("pull deadline %s, want about %s", trip.left, limit)
	}
}

func TestLeaseDrainIsRemembered(t *testing.T) {
	deadline := time.Now().Add(time.Hour)
	body := `{"lease_expires_at":"` + deadline.Format(time.RFC3339Nano) + `","control":{"drain":true}}`
	c := protocolTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/lease") {
			t.Errorf("path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}), nil)
	parent, stop := context.WithCancel(context.Background())
	attempt, cancelAttempt := context.WithCancelCause(parent)
	flag := &stopFlag{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.renewLoop(parent, attempt, cancelAttempt, flag, assignmentJSON{AttemptID: "attempt", AttemptToken: "tok"}, time.Now().Add(time.Minute), deadline)
	}()
	waitUntil := time.Now().Add(2 * time.Second)
	for time.Now().Before(waitUntil) && !c.sessionDraining() {
		time.Sleep(5 * time.Millisecond)
	}
	if !c.sessionDraining() {
		stop()
		t.Fatal("drain was not recorded")
	}
	if attempt.Err() != nil || flag.outcome() != "" {
		stop()
		t.Fatalf("drain stopped the attempt: %v %q", attempt.Err(), flag.outcome())
	}
	stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("renew loop did not return")
	}
}

func TestAttemptDeadlineStopsStalledDownload(t *testing.T) {
	failed := make(chan []byte, 1)
	c := protocolTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			<-r.Context().Done()
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(r.URL.Path, "/fail") {
			failed <- raw
		}
		w.WriteHeader(http.StatusNoContent)
	}), nil)
	c.fileHTTP = c.coreHTTP
	c.operation = convert.OperationConvert
	href := c.core.String() + "/objects"
	asgn := assignmentJSON{
		AttemptID:       "attempt",
		AttemptToken:    "tok",
		AttemptDeadline: time.Now().Add(300 * time.Millisecond).Format(time.RFC3339Nano),
		LeaseExpiresAt:  time.Now().Add(time.Hour).Format(time.RFC3339Nano),
		Inputs:          []fileLinkJSON{{Role: "source", Ordinal: 0, Href: href, Format: "step"}},
		Outputs:         []fileLinkJSON{{Role: "primary", Ordinal: 0, Href: href, Format: "stl"}},
	}
	done := make(chan error, 1)
	go func() {
		done <- c.runAssignment(context.Background(), asgn, controlJSON{})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("attempt ignored the deadline")
	}
	select {
	case body := <-failed:
		if !strings.Contains(string(body), `"engine_code":"timeout"`) {
			t.Fatalf("fail body %s", body)
		}
	default:
		t.Fatal("fail was not posted")
	}
}

func TestProgressDeadlineReportsTimeout(t *testing.T) {
	failed := make(chan []byte, 4)
	puts := make(chan struct{}, 4)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	c := protocolTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/progress") {
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("step"))
			return
		}
		if r.Method == http.MethodPut {
			select {
			case puts <- struct{}{}:
			default:
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(r.URL.Path, "/fail") {
			w.WriteHeader(http.StatusNoContent)
			select {
			case failed <- raw:
			default:
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}), nil)
	c.fileHTTP = c.coreHTTP
	c.operation = convert.OperationConvert
	href := c.core.String() + "/objects"
	asgn := assignmentJSON{
		AttemptID:       "attempt",
		AttemptToken:    "tok",
		AttemptDeadline: time.Now().Add(300 * time.Millisecond).Format(time.RFC3339Nano),
		LeaseExpiresAt:  time.Now().Add(time.Hour).Format(time.RFC3339Nano),
		Inputs:          []fileLinkJSON{{Role: "source", Ordinal: 0, Href: href, Format: "step"}},
		Outputs:         []fileLinkJSON{{Role: "primary", Ordinal: 0, Href: href, Format: "stl"}},
	}
	done := make(chan error, 1)
	go func() {
		done <- c.runAssignment(context.Background(), asgn, controlJSON{})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("progress ignored the deadline")
	}
	select {
	case body := <-failed:
		if !strings.Contains(string(body), `"engine_code":"timeout"`) {
			t.Fatalf("fail body %s", body)
		}
	default:
		t.Fatal("fail was not posted")
	}
	select {
	case <-puts:
		t.Fatal("upload after the progress deadline")
	default:
	}
	select {
	case extra := <-failed:
		t.Fatalf("second fail %s", extra)
	default:
	}
}

func TestProgressLeaseLossStopsAssignment(t *testing.T) {
	stopped := make(chan []byte, 2)
	failed := make(chan struct{}, 2)
	c := protocolTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("step"))
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(r.URL.Path, "/progress") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"attempt_not_active"}`))
			return
		}
		if strings.Contains(r.URL.Path, "/stopped") {
			w.WriteHeader(http.StatusNoContent)
			stopped <- raw
			return
		}
		if strings.Contains(r.URL.Path, "/fail") {
			w.WriteHeader(http.StatusNoContent)
			failed <- struct{}{}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}), nil)
	c.fileHTTP = c.coreHTTP
	c.operation = convert.OperationConvert
	href := c.core.String() + "/objects"
	later := time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	asgn := assignmentJSON{
		AttemptID:       "attempt",
		AttemptToken:    "tok",
		AttemptDeadline: later,
		LeaseExpiresAt:  later,
		Inputs:          []fileLinkJSON{{Role: "source", Ordinal: 0, Href: href, Format: "step"}},
		Outputs:         []fileLinkJSON{{Role: "primary", Ordinal: 0, Href: href, Format: "stl"}},
	}
	done := make(chan error, 1)
	go func() {
		done <- c.runAssignment(context.Background(), asgn, controlJSON{})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("assignment continued after lease loss")
	}
	select {
	case body := <-stopped:
		if !strings.Contains(string(body), `"reason":"lease_lost"`) {
			t.Fatalf("stopped body %s", body)
		}
	default:
		t.Fatal("stopped was not posted")
	}
	select {
	case <-failed:
		t.Fatal("fail after lease loss")
	default:
	}
}

func TestProtocolFileCapCoversCADTransfers(t *testing.T) {
	floors := []struct {
		name string
		n    int64
	}{
		{name: "iges input", n: convert.CADMaxIGESBytes},
		{name: "dxf input", n: convert.CADMaxDXFBytes},
		{name: "svg output", n: convert.CADMaxDXFSVGBytes},
		{name: "stl", n: convert.CADMaxSTLBytes},
	}
	for _, floor := range floors {
		if int64(maxProtocolFileBytes) < floor.n {
			t.Errorf("maxProtocolFileBytes %d is below %s %d", maxProtocolFileBytes, floor.name, floor.n)
		}
	}
	if maxProtocolFileBytes <= 100<<20 {
		t.Fatalf("maxProtocolFileBytes = %d, want a ceiling above 100 MiB", maxProtocolFileBytes)
	}
}

func TestReadLimitedJobFileHonorsAssignmentCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.svg")
	if err := os.WriteFile(path, []byte("abcd"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readLimitedJobFile(path, convert.CADMaxDXFSVGBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := readLimitedJobFile(path, 3); !errors.Is(err, errOutputTooLarge) {
		t.Fatalf("readLimitedJobFile: %v", err)
	}
}
