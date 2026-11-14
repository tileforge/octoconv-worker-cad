import numpy as np
from importlib.machinery import SourceFileLoader
from pathlib import Path

mesh = SourceFileLoader("cadmesh", str(Path(__file__).with_name("cad-mesh"))).load_module()


def idx(*tris):
    flat = np.array(tris, dtype=np.int64).reshape(-1)
    return flat, int(flat.max()) + 1


CASES = {
    "triangle": (idx([0, 1, 2]), ["stl_open"]),
    "same_wind": (idx([0, 1, 2], [0, 1, 3]), ["stl_open", "stl_unoriented"]),
    "fan": (idx([0, 1, 2], [0, 1, 3], [0, 1, 4]), ["stl_open", "stl_non_manifold"]),
    "tet": (idx([0, 1, 2], [0, 2, 3], [0, 3, 1], [1, 3, 2]), []),
    "degenerate": (idx([0, 0, 1]), []),
    "mixed": (idx([0, 1, 2], [0, 1, 3], [4, 4, 5]), ["stl_open", "stl_unoriented"]),
    "open_touching_deg": (idx([0, 1, 2], [0, 0, 1]), ["stl_open"]),
    "tet_plus_deg": (idx([0, 1, 2], [0, 2, 3], [0, 3, 1], [1, 3, 2], [4, 4, 5]), []),
}


def main():
    for name, ((index, nv), want) in CASES.items():
        got = mesh.stats_findings(index, nv)
        if got != want:
            raise SystemExit(f"{name}: {got} != {want}")


if __name__ == "__main__":
    main()
