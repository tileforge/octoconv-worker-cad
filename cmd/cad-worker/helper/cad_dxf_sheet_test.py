import tempfile
from importlib.machinery import SourceFileLoader
from pathlib import Path

import ezdxf

cad = SourceFileLoader("caddxf", str(Path(__file__).with_name("cad-dxf"))).load_module()


def viewrep(name, point, layer="0"):
    lines = [
        "0", "ACDBVIEWREPBLOCKREFERENCE",
        "8", layer,
        "100", "AcDbEntity",
        "100", "AcDbBlockReference",
        "2", name,
        "10", str(point[0]),
        "20", str(point[1]),
        "30", "0.0",
    ]
    return "\n".join(lines) + "\n"


def save_with_viewrep(doc, path, reps):
    doc.saveas(path)
    text = Path(path).read_text(encoding="utf-8")
    newline = "\r\n" if "\r\n" in text else "\n"
    marker = "ENTITIES" + newline
    body = newline.join(line for rep in reps for line in viewrep(*rep).splitlines() if line)
    if not body.endswith(newline):
        body += newline
    if text.count(marker) != 1:
        raise SystemExit("entities section missing")
    Path(path).write_text(text.replace(marker, marker + body, 1), encoding="utf-8")


def sheet_doc():
    doc = ezdxf.new("R2018", setup=True)
    doc.header["$INSUNITS"] = 4
    doc.blocks.new("PART").add_circle((0, 0), radius=2)
    sheet = doc.layouts.new("Sheet")
    sheet.add_line((0, 0), (200, 0))
    sheet.add_viewport(
        center=(100, 80),
        size=(60, 40),
        view_center_point=(5, 6),
        view_height=20,
        status=2,
    )
    return doc


def main():
    with tempfile.TemporaryDirectory() as tmp:
        path = str(Path(tmp) / "sheet.dxf")
        save_with_viewrep(sheet_doc(), path, [("PART", (5.0, 6.0))])
        doc = ezdxf.readfile(path)
        kinds = [e.dxftype() for e in doc.modelspace()]
        if kinds != ["ACDBVIEWREPBLOCKREFERENCE"]:
            raise SystemExit("view-rep missing: %s" % kinds)
        if cad.sole_sheet(doc) is None or cad.sole_sheet(doc).name != "Sheet":
            raise SystemExit("expected the single sheet")
        before = len(list(doc.modelspace()))
        cad.promote_viewreps(doc)
        inserts = [e for e in doc.modelspace() if e.dxftype() == "INSERT"]
        if len(inserts) != 1 or inserts[0].dxf.name != "PART":
            raise SystemExit("insert was not added")
        if abs(inserts[0].dxf.insert.x - 5.0) > 1e-9 or abs(inserts[0].dxf.insert.y - 6.0) > 1e-9:
            raise SystemExit("insert point %s" % inserts[0].dxf.insert)
        if len(list(doc.modelspace())) != before + 1:
            raise SystemExit("view-rep was removed")

        doc.modelspace().add_line((0, 0), (10, 0))
        if cad.sole_sheet(doc) is None:
            raise SystemExit("a model line must not hide the sheet lookup")

        second = doc.layouts.new("Other")
        second.add_viewport(
            center=(10, 10),
            size=(10, 10),
            view_center_point=(0, 0),
            view_height=10,
            status=2,
        )
        if cad.sole_sheet(doc) is not None:
            raise SystemExit("two sheets must not be chosen")

        absent_path = str(Path(tmp) / "absent.dxf")
        save_with_viewrep(sheet_doc(), absent_path, [("ABSENT", (1.0, 2.0))])
        absent = ezdxf.readfile(absent_path)
        cad.promote_viewreps(absent)
        if any(e.dxftype() == "INSERT" for e in absent.modelspace()):
            raise SystemExit("missing block was inserted")

        close_path = str(Path(tmp) / "close.dxf")
        save_with_viewrep(close_views(), close_path, [
            ("TOP", (-10053.25, -10043.25)),
            ("SIDE", (-10053.25, -10011.25)),
        ])
        close = ezdxf.readfile(close_path)
        cad.promote_viewreps(close)
        sheet = close.layouts.get("Sheet")
        views = [e for e in sheet if e.dxftype() == "VIEWPORT" and e.dxf.status > 1]
        if len(views) != 2:
            raise SystemExit("expected two viewports, got %s" % len(views))

        def owned(vp):
            found = []
            for entity in cad.entities_at_view(vp, close.modelspace()):
                if entity.dxftype() == "INSERT":
                    found.append(entity.dxf.name)
                else:
                    found.append(entity.dxftype())
            return found

        by_status = {e.dxf.status: e for e in views}
        if owned(by_status[2]) != ["ACDBVIEWREPBLOCKREFERENCE", "TOP"]:
            raise SystemExit("top viewport owns %s" % owned(by_status[2]))
        if owned(by_status[3]) != ["ACDBVIEWREPBLOCKREFERENCE", "SIDE"]:
            raise SystemExit("side viewport owns %s" % owned(by_status[3]))
        page = sheet.add_viewport(
            center=(200, 150),
            size=(400, 280),
            view_center_point=(0, 0),
            view_height=280,
            status=1,
        )
        if list(cad.entities_at_view(page, close.modelspace())):
            raise SystemExit("page viewport took a view block")
        shifted = by_status[2]
        shifted.dxf.view_target_point = (1.0, 2.0, 0.0)
        shifted.dxf.view_center_point = (-10054.25, -10045.25)
        if owned(shifted) != ["ACDBVIEWREPBLOCKREFERENCE", "TOP"]:
            raise SystemExit("target offset owns %s" % owned(shifted))
    print("ok")


def close_views():
    doc = ezdxf.new("R2018", setup=True)
    doc.blocks.new("TOP").add_circle((0, 0), radius=4)
    doc.blocks.new("SIDE").add_line((-10, -2), (10, 2))
    sheet = doc.layouts.new("Sheet")
    sheet.add_line((0, 0), (200, 0))
    sheet.add_viewport(
        center=(100, 120),
        size=(80, 90),
        view_center_point=(-10053.25, -10043.25),
        view_height=50,
        status=2,
    )
    sheet.add_viewport(
        center=(100, 40),
        size=(80, 20),
        view_center_point=(-10053.25, -10011.25),
        view_height=16,
        status=3,
    )
    return doc


if __name__ == "__main__":
    main()
