package convert

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSurfaceOptionsReachRenderer(t *testing.T) {
	dir := t.TempDir()
	const renderer = `#!/bin/sh
while [ "$#" -gt 2 ]; do
  case "$1" in
    --material) material=$2 ;;
    --color) color=$2 ;;
    --finish) finish=$2 ;;
  esac
  shift 2
done
printf '%s %s %s' "$material" "$color" "$finish" > "$2"
printf '%s\n' 'CADVERDICT {"stage":"render","verdict":"ok","view":"iso","size":1280,"background":"opaque"}'
`
	if err := os.WriteFile(filepath.Join(dir, "cad-render"), []byte(renderer), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	for _, tc := range []struct{ name, options, want string }{
		{"default", `{}`, "steel natural bare"},
		{"polished", `{"material":"copper","finish":"polished"}`, "copper natural polished"},
		{"anodized", `{"material":"aluminum","color":"blue","finish":"anodized"}`, "aluminum blue anodized"},
		{"powder coat", `{"material":"steel","color":"black","finish":"matte_powder_coat"}`, "steel black matte_powder_coat"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := ParseCADOpts([]byte(tc.options))
			if err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(dir, "out.png")
			if err := (CADRenderConverter{}).renderStage(t.Context(), dir, "mesh.stl", output, opts.Resolved()); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(output)
			if err != nil || string(got) != tc.want {
				t.Fatalf("renderer got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestSurfaceOptionsApplyOnlyToModelRenders(t *testing.T) {
	for key, value := range map[string]string{"material": "steel", "color": "natural", "finish": "bare"} {
		opts, err := CADOptsFromMap(map[string]any{key: value})
		if err != nil {
			t.Fatal(err)
		}
		for _, pair := range (CADRenderConverter{}).Pairs() {
			if err := ValidateCADApplicability(EngineCAD, OperationRender, pair.From, pair.To, opts); err != nil {
				t.Errorf("%s render with %s: %v", pair.From, key, err)
			}
		}
		for _, format := range []string{"dxf", "dwg"} {
			if err := ValidateCADApplicability(EngineCAD, OperationRender, format, "png", opts); err == nil {
				t.Errorf("%s render accepted %s", format, key)
			}
		}
		if err := ValidateCADApplicability(EngineCAD, OperationConvert, "step", "stl", opts); err == nil {
			t.Errorf("mesh conversion accepted %s", key)
		}
		for _, invalid := range []any{"", "unknown", []string{value}, nil} {
			_, err := CADOptsFromMap(map[string]any{key: invalid})
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Errorf("%s = %#v: %v", key, invalid, err)
			}
		}
	}
}
