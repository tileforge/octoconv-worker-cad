from importlib.machinery import SourceFileLoader
from pathlib import Path

cad = SourceFileLoader("caddxf", str(Path(__file__).with_name("cad-dxf"))).load_module()


def draft(skipped, insunits=4):
    state = cad.new_state()
    state["insunits"] = insunits
    state["skipped"] = skipped
    return cad.measure_draft(state, ((0, 0), (10, 4)))


def main():
    plain = draft({})
    if "skipped" in plain:
        raise SystemExit("empty skips were written: %s" % plain)
    if plain["format"] != "dxf" or plain["null_reason"] != "not_applicable_2d":
        raise SystemExit("draft lost its dxf fields: %s" % plain)
    counted = draft({"IMAGE": 2, "PROXY:ACAD_PROXY_ENTITY": 1})
    if counted["skipped"] != {"IMAGE": 2, "PROXY:ACAD_PROXY_ENTITY": 1}:
        raise SystemExit("skips were not copied: %s" % counted)
    print("ok")


if __name__ == "__main__":
    main()
