import tempfile
from importlib.machinery import SourceFileLoader
from pathlib import Path

import ezdxf

cad = SourceFileLoader("caddxf", str(Path(__file__).with_name("cad-dxf"))).load_module()


def viewrep(name, point, layer="0", invisible=0):
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
    if invisible:
        lines.extend(["60", "1"])
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

        hidden_path = str(Path(tmp) / "hidden.dxf")
        save_with_viewrep(sheet_doc(), hidden_path, [("PART", (5.0, 6.0), "0", 1)])
        hidden = ezdxf.readfile(hidden_path)
        cad.promote_viewreps(hidden)
        hidden_inserts = [e for e in hidden.modelspace() if e.dxftype() == "INSERT"]
        if len(hidden_inserts) != 1 or hidden_inserts[0].dxf.invisible != 1:
            raise SystemExit("invisible view-rep was promoted as visible")

        shown = draw_copy(sheet_doc(), tmp, "shown-part", [("PART", (5.0, 6.0))])
        concealed = draw_copy(sheet_doc(), tmp, "concealed-part", [("PART", (5.0, 6.0), "0", 1)])
        if path_count(concealed[1]) >= path_count(shown[1]):
            raise SystemExit("invisible view-rep still drew the block")

        open_draw = draw_copy(hidden_line_doc(), tmp, "layer-open", [("PART", (5.0, 6.0))])
        frozen_draw = draw_copy(hidden_line_doc(), tmp, "layer-frozen", [("PART", (5.0, 6.0))], freeze="HIDE")
        if frozen_draw[0]["entities_drawn"] >= open_draw[0]["entities_drawn"]:
            raise SystemExit("viewport-frozen layer was drawn")
        if path_count(frozen_draw[1]) >= path_count(open_draw[1]):
            raise SystemExit("viewport-frozen layer left ink")

        text_draw = draw_copy(far_text_doc(), tmp, "far-text", [("TOP", (-10053.25, -10043.25))])
        if span(text_draw[2]) > 1000:
            raise SystemExit("text frame uses model coordinates: %s" % (text_draw[2],))
        if [e["text"] for e in text_draw[3]] != ["Hi"]:
            raise SystemExit("sheet text %s" % [e["text"] for e in text_draw[3]])
        text_el = text_draw[3][0]["el"]
        parents = {child: parent for parent in text_draw[1].iter() for child in parent}
        if parents[text_el].get("clip-path") is None:
            raise SystemExit("viewport text has no clip")

        plain = draw_copy(far_curve_doc(False), tmp, "far-plain", [("TOP", (-10053.25, -10043.25))])
        crossed = draw_copy(far_curve_doc(True), tmp, "far-xline", [("TOP", (-10053.25, -10043.25))])
        if span(crossed[2]) > 1000:
            raise SystemExit("xline frame uses model coordinates: %s" % (crossed[2],))
        if path_count(crossed[1]) <= path_count(plain[1]):
            raise SystemExit("viewport xline produced no path")

        model = ezdxf.new("R2018", setup=True)
        model.modelspace().add_line((0, 0), (10, 0))
        model.modelspace().add_text("Ok", dxfattribs={"insert": (1, 1), "height": 2})
        model.modelspace().add_xline((0, 0), (1, 0))
        model_state = cad.new_state()
        model_root, model_box, model_emitted = cad.draw(model, 1000, model_state)
        if [e["text"] for e in model_emitted] != ["Ok"]:
            raise SystemExit("model text %s" % [e["text"] for e in model_emitted])
        if span(model_box) > 1000:
            raise SystemExit("model frame %s" % (model_box,))
        parents = {child: parent for parent in model_root.iter() for child in parent}
        if parents[model_emitted[0]["el"]].get("clip-path"):
            raise SystemExit("model text was clipped to a viewport")
        if path_count(model_root) < 2:
            raise SystemExit("model xline produced no path")

        chosen = ezdxf.new("R2018", setup=True)
        chosen.modelspace().add_line((0, 0), (10, 0))
        chosen.layouts.new("Sheet").add_line((0, 0), (200, 0))
        chosen.layouts.new("Other").add_line((0, 0), (30, 0))

        def drawn_span(layout_name):
            _, box, _ = cad.draw(chosen, 1000, cad.new_state(), layout_name)
            return span(box)

        if drawn_span("") >= 50:
            raise SystemExit("omitted layout left model space: %s" % drawn_span(""))
        if drawn_span("Sheet") <= 150:
            raise SystemExit("named Sheet was not drawn")
        other_span = drawn_span("Other")
        if other_span < 20 or other_span > 80:
            raise SystemExit("named Other span %s" % other_span)
        for name in ("Missing", "Model"):
            try:
                cad.draw(chosen, 1000, cad.new_state(), name)
            except cad.Outcome as out:
                if out.verdict != "failed" or out.reason != "unknown_layout":
                    raise SystemExit("%s outcome %s %s" % (name, out.verdict, out.reason))
            else:
                raise SystemExit("%s was drawn" % name)

        window = ezdxf.new("R2018", setup=True)
        window.modelspace().add_circle((0, 0), radius=5)
        paper = window.layouts.new("Window")
        paper.add_line((0, 0), (10, 0))
        paper.add_viewport(
            center=(40, 40),
            size=(30, 30),
            view_center_point=(0, 0),
            view_height=20,
            status=2,
        )
        window_state = cad.new_state()
        window_root, _, _ = cad.draw(window, 1000, window_state, "Window")
        if path_count(window_root) < 2:
            raise SystemExit("named layout hid the model circle: paths %s drawn %s" % (
                path_count(window_root), window_state["entities_drawn"]))

        named_part = draw_layout(sheet_doc(), tmp, "named-part", [("PART", (5.0, 6.0))], "Sheet")
        if path_count(named_part[1]) != path_count(shown[1]):
            raise SystemExit("named empty-model sheet paths %s, sole sheet %s" % (
                path_count(named_part[1]), path_count(shown[1])))
        if span(named_part[2]) > 1000:
            raise SystemExit("named empty-model sheet uses model coordinates: %s" % (named_part[2],))
    print("ok")


def draw_layout(doc, tmp, name, reps, layout_name, freeze=""):
    path = str(Path(tmp) / (name + ".dxf"))
    save_with_viewrep(doc, path, reps)
    loaded = ezdxf.readfile(path)
    if freeze:
        sheet = loaded.layouts.get("Sheet")
        for entity in sheet:
            if entity.dxftype() == "VIEWPORT" and entity.dxf.status > 1:
                entity.frozen_layers = [freeze]
    state = cad.new_state()
    root, box, emitted = cad.draw(loaded, 1000, state, layout_name)
    return state, root, box, emitted


def draw_copy(doc, tmp, name, reps, freeze=""):
    return draw_layout(doc, tmp, name, reps, "", freeze)


def span(box):
    (lo, hi) = box
    return max(hi[0] - lo[0], hi[1] - lo[1])


def path_count(root):
    return sum(1 for el in root.iter() if str(el.tag).endswith("path"))


def hidden_line_doc():
    doc = ezdxf.new("R2018", setup=True)
    doc.header["$INSUNITS"] = 4
    doc.layers.add("HIDE")
    block = doc.blocks.new("PART")
    block.add_circle((0, 0), radius=2)
    block.add_line((-4, -1), (4, 1), dxfattribs={"layer": "HIDE"})
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


def far_sheet(doc):
    sheet = doc.layouts.new("Sheet")
    sheet.add_line((0, 0), (200, 0))
    sheet.add_viewport(
        center=(100, 120),
        size=(80, 90),
        view_center_point=(-10053.25, -10043.25),
        view_height=50,
        status=2,
    )
    return doc


def far_text_doc():
    doc = ezdxf.new("R2018", setup=True)
    doc.header["$INSUNITS"] = 4
    block = doc.blocks.new("TOP")
    block.add_circle((0, 0), radius=4)
    block.add_text("Hi", dxfattribs={"insert": (0, 0), "height": 3})
    return far_sheet(doc)


def far_curve_doc(with_xline):
    doc = ezdxf.new("R2018", setup=True)
    doc.header["$INSUNITS"] = 4
    block = doc.blocks.new("TOP")
    block.add_circle((0, 0), radius=4)
    if with_xline:
        block.add_xline((0, 0), (1, 0))
        block.add_ray((0, 0), (0, 1))
    return far_sheet(doc)


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
