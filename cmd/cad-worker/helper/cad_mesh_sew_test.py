from importlib.machinery import SourceFileLoader
from pathlib import Path

from OCP.BRep import BRep_Builder
from OCP.BRepBuilderAPI import BRepBuilderAPI_Transform
from OCP.BRepMesh import BRepMesh_IncrementalMesh
from OCP.BRepPrimAPI import BRepPrimAPI_MakeBox
from OCP.TopAbs import TopAbs_FACE
from OCP.TopExp import TopExp_Explorer
from OCP.TopoDS import TopoDS, TopoDS_Compound
from OCP.gp import gp_Trsf, gp_Vec

mesh = SourceFileLoader("cadmesh", str(Path(__file__).with_name("cad-mesh"))).load_module()


def faces_of(shape):
    faces = []
    explorer = TopExp_Explorer(shape, TopAbs_FACE)
    while explorer.More():
        faces.append(TopoDS.Face(explorer.Current()))
        explorer.Next()
    return faces


def compound_of(faces):
    builder = BRep_Builder()
    compound = TopoDS_Compound()
    builder.MakeCompound(compound)
    for face in faces:
        builder.Add(compound, face)
    return compound


def translated(face, vec):
    trsf = gp_Trsf()
    trsf.SetTranslation(vec)
    return BRepBuilderAPI_Transform(face, trsf, True).Shape()


def main():
    box = BRepPrimAPI_MakeBox(10, 20, 30).Shape()
    faces = faces_of(box)
    if len(faces) != 6:
        raise SystemExit("box faces: %d" % len(faces))

    sewed = mesh.sew_closed_iges(compound_of(faces))
    solids, closed, volume, area, reason, boxes = mesh.solid_summary(sewed)
    if (solids, closed, volume, reason) != (1, 1, 6000.0, "") or len(boxes) != 1:
        raise SystemExit("closed box: %s %s %s %s boxes=%d" % (solids, closed, volume, reason, len(boxes)))
    if area is None or area <= 0:
        raise SystemExit("closed box area: %s" % area)
    if not BRepMesh_IncrementalMesh(sewed, 0.1, False, 0.5, False).IsDone():
        raise SystemExit("sewed box did not mesh")
    if mesh.face_survey(sewed)[2] <= 0:
        raise SystemExit("sewed box has no triangles")

    sheet = compound_of(faces[:5])
    if mesh.sew_closed_iges(sheet) is not sheet:
        raise SystemExit("open sheet was replaced")
    solids, closed, volume, area, reason, boxes = mesh.solid_summary(sheet)
    if (solids, closed, volume, reason) != (0, 0, None, "no_solids"):
        raise SystemExit("open sheet summary: %s %s %s %s" % (solids, closed, volume, reason))

    gapped = compound_of(faces[:5] + [translated(faces[5], gp_Vec(0, 0, 1))])
    if mesh.sew_closed_iges(gapped) is not gapped:
        raise SystemExit("gapped sheet was replaced")
    solids, closed, volume, area, reason, boxes = mesh.solid_summary(gapped)
    if solids != 0 or volume is not None:
        raise SystemExit("gapped sheet gained a volume: %s %s" % (solids, volume))

    if mesh.sew_closed_iges(box) is not box:
        raise SystemExit("existing solid was replaced")

    empty = compound_of([])
    if mesh.sew_closed_iges(empty) is not empty:
        raise SystemExit("empty compound was replaced")

    saved = mesh.SEW_FACE_CAP
    mesh.SEW_FACE_CAP = 5
    try:
        capped = compound_of(faces)
        if mesh.sew_closed_iges(capped) is not capped:
            raise SystemExit("face cap sewed")
    finally:
        mesh.SEW_FACE_CAP = saved

    pair = faces + [translated(face, gp_Vec(100, 0, 0)) for face in faces]
    sewed = mesh.sew_closed_iges(compound_of(pair))
    solids, closed, volume, area, reason, boxes = mesh.solid_summary(sewed)
    if (solids, closed, volume, reason) != (2, 2, 12000.0, "") or len(boxes) != 2:
        raise SystemExit("two boxes: %s %s %s %s boxes=%d" % (solids, closed, volume, reason, len(boxes)))
    print("ok")


if __name__ == "__main__":
    main()
