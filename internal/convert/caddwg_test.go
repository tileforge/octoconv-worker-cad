package convert

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDrawingPairsAdmitDWG(t *testing.T) {
	got := map[string]bool{}
	for _, p := range (CADDXFConverter{}).Pairs() {
		got[OperationConvert+" "+p.From+" "+p.To] = true
	}
	for _, p := range (CADDXFRenderConverter{}).Pairs() {
		got[OperationRender+" "+p.From+" "+p.To] = true
	}
	for _, key := range []string{
		OperationConvert + " dwg svg",
		OperationConvert + " dwg pdf",
		OperationConvert + " dwg png",
		OperationRender + " dwg png",
		OperationRender + " dxf png",
	} {
		if !got[key] {
			t.Fatalf("missing %s", key)
		}
	}
	if got[OperationConvert+" dxf png"] {
		t.Fatal("dxf to png is registered as a convert pair")
	}
	png, ok := CADStages(OperationConvert, "dwg", "png")
	render, renderOK := CADStages(OperationRender, "dwg", "png")
	if !ok || !renderOK || !slices.Equal(png, render) {
		t.Fatalf("convert png route %v, render png route %v", png, render)
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
