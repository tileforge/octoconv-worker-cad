package convert

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

const cadDWGSourceName = "source.dxf"

var cadDWGEngines = []string{"cad-dwg"}

func (c CADConverter) openDrawing(ctx context.Context, inPath, outPath, format string, st cadStager) (string, string, error) {
	if format == "dxf" {
		jobDir, err := cadDXFAdmit(inPath, outPath)
		return jobDir, inPath, err
	}
	if format != "dwg" {
		return "", "", fmt.Errorf("cad: unsupported drawing source %s", format)
	}
	jobDir, err := cadDXFNames(inPath, outPath, cadDWGSourceName)
	if err != nil {
		return "", "", err
	}
	dxfPath, err := c.writeDWGDXF(ctx, jobDir, inPath, outPath, st)
	if err != nil {
		return "", "", err
	}
	return jobDir, dxfPath, nil
}

func (c CADConverter) writeDWGDXF(ctx context.Context, jobDir, inPath, outPath string, st cadStager) (string, error) {
	dxfPath := filepath.Join(jobDir, cadDWGSourceName)
	if err := cadDXFClear(dxfPath); err != nil {
		return "", err
	}
	st.enter(cadStageDWG)
	if _, err := c.dwgStage(ctx, jobDir, inPath, dxfPath); err != nil {
		os.Remove(dxfPath)
		return "", err
	}
	if _, err := cadDXFAdmit(dxfPath, outPath); err != nil {
		os.Remove(dxfPath)
		return "", err
	}
	return dxfPath, nil
}

func (c CADConverter) dwgStage(ctx context.Context, jobDir, inPath, outPath string) (cadVerdict, error) {
	x, err := c.cadStage(jobDir, nil, cadDWGEngines)
	if err != nil {
		return cadVerdict{}, err
	}
	timeout := c.dwgTimeout()
	stageCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, cpu, err := x.runBounded(stageCtx, cadOutputLimit, "cad-dwg", inPath, outPath)
	return classifyCADStage(cadStageDWG, ctx, stageCtx, timeout, out, cpu, err)
}
