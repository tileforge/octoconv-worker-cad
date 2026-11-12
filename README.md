# octoconv-worker-cad

Cad worker for OctoConv. One process serves one approved manifest over Worker Protocol v1.

The process reads `CAD_PROTOCOL_CORE_URL`, `CAD_PROTOCOL_CLIENT_CERT`, `CAD_PROTOCOL_CLIENT_KEY`, `CAD_PROTOCOL_CLIENT_CA`, `CAD_PROTOCOL_WORKER_ID`, and `CAD_PROTOCOL_MANIFEST_PATH`. It does not read `DATABASE_URL` or `REDIS_ADDR`.

`schema/manifest-cad-convert.json` is capability `octoconv.convert.cad`: step to stl, iges to stl, dxf to svg, dxf to pdf. `schema/manifest-cad-render.json` is capability `octoconv.render.cad`: step to png, iges to png, stl to png, dxf to png. Each capability needs its own worker identity.
