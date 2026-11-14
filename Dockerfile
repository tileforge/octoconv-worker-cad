FROM debian:bookworm-slim@sha256:99c0b25c46d0214d5a2091092e7908907dcaecd6a962cde0ac700acee222b6f3 AS venv
ENV DEBIAN_FRONTEND=noninteractive PIP_DISABLE_PIP_VERSION_CHECK=1
RUN apt-get update \
 && apt-get install -y --no-install-recommends python3 python3-venv \
 && rm -rf /var/lib/apt/lists/*
RUN python3 -m venv /opt/venv
COPY cmd/cad-worker/requirements-mesh.txt /req/requirements-mesh.txt
RUN /opt/venv/bin/pip install --no-cache-dir --only-binary=:all: --no-deps --require-hashes \
      -r /req/requirements-mesh.txt
COPY cmd/cad-worker/requirements-vtk.txt /req/requirements-vtk.txt
RUN /opt/venv/bin/pip install --no-cache-dir --only-binary=:all: --no-deps --require-hashes \
      -r /req/requirements-vtk.txt
COPY cmd/cad-worker/vtk_keeplist.py cmd/cad-worker/vtk-keeplist.txt /tmp/keeplist/
RUN /opt/venv/bin/python -I -B /tmp/keeplist/vtk_keeplist.py prune \
      --keeplist /tmp/keeplist/vtk-keeplist.txt --site /opt/venv/lib/python3.11/site-packages \
 && rm -rf /tmp/keeplist

FROM debian:bookworm-slim@sha256:99c0b25c46d0214d5a2091092e7908907dcaecd6a962cde0ac700acee222b6f3 AS engine
ENV DEBIAN_FRONTEND=noninteractive CAD_STACK=model
RUN apt-get update \
 && apt-get install -y --no-install-recommends python3 libgl1 libx11-6 libosmesa6 ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=venv /opt/venv /opt/venv
COPY cmd/cad-worker/helper/cad-mesh /usr/local/bin/cad-mesh
COPY cmd/cad-worker/helper/cad-guard /usr/local/bin/cad-guard
COPY cmd/cad-worker/helper/cad-render /usr/local/bin/cad-render
RUN chmod 0755 /usr/local/bin/cad-mesh /usr/local/bin/cad-guard /usr/local/bin/cad-render
RUN /opt/venv/bin/python -I -B -c "import OCP.STEPControl, OCP.IGESControl, OCP.BRepMesh, OCP.StlAPI, OCP.BRepBndLib, OCP.Interface, OCP.IMeshData, OCP.BRep, OCP.TopLoc, OCP.TopoDS, vtkmodules.vtkCommonCore, vtkmodules.vtkFiltersCore, vtkmodules.vtkIOGeometry, vtkmodules.vtkIOImage, vtkmodules.vtkRenderingCore, vtkmodules.vtkRenderingOpenGL2"
USER nobody

FROM golang:1.27.3-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/cad-worker ./cmd/cad-worker

FROM engine
COPY --from=build /out/cad-worker /usr/local/bin/cad-worker
USER nobody
ENTRYPOINT ["/usr/local/bin/cad-worker"]
