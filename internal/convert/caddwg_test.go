package convert

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDrawingPairsAdmitDWG(t *testing.T) {
	want := map[Pair]string{
		{From: "dwg", To: "svg"}: OperationConvert,
		{From: "dwg", To: "pdf"}: OperationConvert,
		{From: "dwg", To: "png"}: OperationRender,
	}
	got := map[Pair]string{}
	for _, p := range (CADDXFConverter{}).Pairs() {
		got[p] = OperationConvert
	}
	for _, p := range (CADDXFRenderConverter{}).Pairs() {
		got[p] = OperationRender
	}
	for p, op := range want {
		if got[p] != op {
			t.Fatalf("%s to %s is %q", p.From, p.To, got[p])
		}
	}
}

func TestDWGRouteFitsEngineTimeout(t *testing.T) {
	model := CADDefaultGuardTimeout + CADDefaultMeshTimeout + CADDefaultRenderTimeout + CADIOAllowance
	drawing := CADDefaultGuardTimeout + CADDefaultDWGTimeout + CADDefaultDXFTimeout + 2*CADDefaultRsvgTimeout + CADIOAllowance
	if CADDefaultEngineTimeout != model {
		t.Fatalf("engine timeout %s, model route %s", CADDefaultEngineTimeout, model)
	}
	if drawing > CADDefaultEngineTimeout {
		t.Fatalf("dwg route %s exceeds engine timeout %s", drawing, CADDefaultEngineTimeout)
	}
	if err := ValidateCADTimeouts(CADDefaultGuardTimeout, CADDefaultMeshTimeout, CADDefaultRenderTimeout, CADDefaultDXFTimeout, CADDefaultDWGTimeout, CADDefaultRsvgTimeout, CADDefaultEngineTimeout); err != nil {
		t.Fatal(err)
	}
}

func TestIntermediateDXFKeepsTheDXFCeiling(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, cadDWGSourceName)
	out := filepath.Join(dir, "out.svg")
	if err := os.WriteFile(in, []byte("0\nSECTION\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cadDXFAdmit(in, out); err != nil {
		t.Fatal(err)
	}
	dwg := filepath.Join(dir, "in.dwg")
	if _, err := cadDXFNames(in, out, cadDWGSourceName); err == nil {
		t.Fatal("an input named source.dxf must be reserved before the dwg stage writes it")
	}
	if _, err := cadDXFNames(dwg, in, cadDWGSourceName); err == nil {
		t.Fatal("an output named source.dxf must be reserved before the dwg stage writes it")
	}
}

func TestDWGApplicabilityMatchesDXF(t *testing.T) {
	view := CADOpts{View: CADViewIso}
	for _, format := range []string{"dxf", "dwg"} {
		err := ValidateCADApplicability(EngineCAD, OperationConvert, format, "svg", view)
		if err == nil || !strings.Contains(err.Error(), "no option applies to a "+format+" conversion") {
			t.Fatalf("%s convert: %v", format, err)
		}
		err = ValidateCADApplicability(EngineCAD, OperationRender, format, "png", view)
		if err == nil || !strings.Contains(err.Error(), "view does not apply to a "+format+" source") {
			t.Fatalf("%s render: %v", format, err)
		}
	}
}
