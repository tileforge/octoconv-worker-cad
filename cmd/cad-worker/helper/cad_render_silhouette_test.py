import struct
import tempfile
from importlib.machinery import SourceFileLoader
from pathlib import Path
from types import SimpleNamespace

import numpy as np
from vtkmodules.vtkCommonCore import vtkPoints
from vtkmodules.vtkCommonDataModel import vtkCellArray, vtkPolyData, vtkTriangle


def load():
    path = Path(__file__).with_name("cad-render")
    return SourceFileLoader("cad_render", str(path)).load_module()


def test_shared_edge_of_opposite_faces_is_a_silhouette(render):
    points = np.array([[0, 0, 0], [1, 0, 0], [0, 1, 0], [0, 0, -1]], dtype=np.float64)
    triangles = np.array([[0, 1, 2], [0, 2, 3]], dtype=np.int64)
    edges = render.silhouette_edges(points, triangles, (0, 0, 1))
    assert edges.tolist() == [[0, 2]], edges.tolist()


def test_two_front_faces_have_no_silhouette(render):
    points = np.array([[0, 0, 0], [1, 0, 0], [0, 1, 0], [1, 1, 0]], dtype=np.float64)
    triangles = np.array([[0, 1, 2], [1, 3, 2]], dtype=np.int64)
    edges = render.silhouette_edges(points, triangles, (0, 0, 1))
    assert edges.size == 0


def test_a_small_mesh_keeps_its_silhouette(render):
    points = np.array([[0, 0, 0], [1, 0, 0], [0, 1, 0], [0, 0, -1]], dtype=np.float64) * 1e-8
    triangles = np.array([[0, 1, 2], [0, 2, 3]], dtype=np.int64)
    edges = render.silhouette_edges(points, triangles, (0, 0, 1))
    assert edges.tolist() == [[0, 2]], edges.tolist()


def test_opposite_winding_on_one_plane_is_not_a_silhouette(render):
    points = vtkPoints()
    for xyz in ((0, 0, 0), (1, 0, 0), (0.5, 1, 0), (0.5, -1, 0)):
        points.InsertNextPoint(*xyz)
    cells = vtkCellArray()
    for ids in ((0, 1, 2), (0, 1, 3)):
        tri = vtkTriangle()
        for i, point in enumerate(ids):
            tri.GetPointIds().SetId(i, point)
        cells.InsertNextCell(tri)
    poly = vtkPolyData()
    poly.SetPoints(points)
    poly.SetPolys(cells)
    assert render.silhouette_lines(poly, (0, 0, 1)) is None


def test_a_boundary_is_not_a_silhouette(render):
    points = np.array([[0, 0, 0], [1, 0, 0], [0, 1, 0]], dtype=np.float64)
    triangles = np.array([[0, 1, 2]], dtype=np.int64)
    edges = render.silhouette_edges(points, triangles, (0, 0, 1))
    assert edges.size == 0


def test_polydata_lines_use_the_same_edge(render):
    points = vtkPoints()
    for xyz in ((0, 0, 0), (1, 0, 0), (0, 1, 0), (0, 0, -1)):
        points.InsertNextPoint(*xyz)
    cells = vtkCellArray()
    for ids in ((0, 1, 2), (0, 2, 3)):
        tri = vtkTriangle()
        for i, point in enumerate(ids):
            tri.GetPointIds().SetId(i, point)
        cells.InsertNextCell(tri)
    poly = vtkPolyData()
    poly.SetPoints(points)
    poly.SetPolys(cells)
    lines = render.silhouette_lines(poly, (0, 0, 1))
    assert lines.GetNumberOfLines() == 1
    assert lines.GetNumberOfPoints() == 4


def stl_bytes(triangles):
    body = bytearray(b"octoconv".ljust(80, b"\0"))
    body += struct.pack("<I", len(triangles))
    for tri in triangles:
        body += struct.pack("<12fH", 0, 0, 0, *tri[0], *tri[1], *tri[2], 0)
    return bytes(body)


def test_render_keeps_a_folded_pair(render):
    triangles = (
        ((0, 0, 0), (1, 0, 0), (0, 1, 0)),
        ((0, 0, 0), (0, 1, 0), (0, 0, -1)),
    )
    with tempfile.TemporaryDirectory() as directory:
        src = Path(directory) / "fold.stl"
        dst = Path(directory) / "fold.png"
        src.write_bytes(stl_bytes(triangles))
        args = SimpleNamespace(view="front", size=256, background="opaque", src=str(src), dst=str(dst))
        code = render.render(1, args)
        assert code == 0, code
        assert dst.stat().st_size > 0


def main():
    render = load()
    test_shared_edge_of_opposite_faces_is_a_silhouette(render)
    test_two_front_faces_have_no_silhouette(render)
    test_a_boundary_is_not_a_silhouette(render)
    test_a_small_mesh_keeps_its_silhouette(render)
    test_opposite_winding_on_one_plane_is_not_a_silhouette(render)
    test_polydata_lines_use_the_same_edge(render)
    test_render_keeps_a_folded_pair(render)
    print("ok")


if __name__ == "__main__":
    main()
