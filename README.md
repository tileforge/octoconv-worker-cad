# octoconv-worker-cad

Cad worker for OctoConv. One process serves one approved manifest over Worker Protocol v1.

The process reads `CAD_STACK` (`model` or `drawing`), `CAD_PROTOCOL_CORE_URL`, `CAD_PROTOCOL_CLIENT_CERT`, `CAD_PROTOCOL_CLIENT_KEY`, `CAD_PROTOCOL_CLIENT_CA`, `CAD_PROTOCOL_WORKER_ID`, and `CAD_PROTOCOL_MANIFEST_PATH`. It does not read `DATABASE_URL` or `REDIS_ADDR`. An empty or unknown `CAD_STACK` stops the process before it dials.

`Dockerfile` is the model image (`CAD_STACK=model`): OCP and VTK, no DXF toolchain. `Dockerfile.drawing` is the drawing image (`CAD_STACK=drawing`): ezdxf and `rsvg-convert`, no OCP and no VTK. Its venv is CPython 3.12, because `numpy==2.5.4` publishes no cp311 wheel. `cad-guard` stays on Debian `python3`. DWG stays out of both images.

`schema/manifest-cad-convert.json` is capability `octoconv.convert.cad`: step to stl, iges to stl. `schema/manifest-cad-render.json` is capability `octoconv.render.cad`: step to png, iges to png, stl to png. `schema/manifest-caddraw-convert.json` is capability `octoconv.convert.caddraw`: dxf to svg, dxf to pdf. `schema/manifest-caddraw-render.json` is capability `octoconv.render.caddraw`: dxf to png. Each capability needs its own worker identity. `legacy.engine` stays `cad` on all four.
