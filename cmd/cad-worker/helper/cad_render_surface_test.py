"""Image-level checks for the offscreen renderer. Run with the model image's Python."""
import math
import struct
import subprocess
import sys
import tempfile
from pathlib import Path

import numpy as np
from vtkmodules.util.numpy_support import vtk_to_numpy
from vtkmodules.vtkIOImage import vtkPNGReader


def make_bushing(path):
    # A turned flange, with a chamfered outside diameter and a through bore.
    profile = [(0.5, 0), (1.9, 0), (2, 0.1), (2, 0.55), (1.9, 0.65),
               (1.1, 0.65), (1.1, 1.55), (1, 1.65), (0.5, 1.65)]
    rings = []
    for radius, z in profile:
        rings.append([(radius * math.cos(i * math.tau / 96),
                       radius * math.sin(i * math.tau / 96), z) for i in range(96)])
    facets = []
    for lower, upper in zip(rings, rings[1:] + rings[:1]):
        for i in range(96):
            j = (i + 1) % 96
            facets.extend([(lower[i], lower[j], upper[j]), (lower[i], upper[j], upper[i])])
    with path.open("wb") as f:
        f.write(bytes(80) + struct.pack("<I", len(facets)))
        for a, b, c in facets:
            f.write(struct.pack("<12fH", 0, 0, 0, *a, *b, *c, 0))


def render(src, dst, *options):
    subprocess.run([sys.executable, "-IB", str(Path(__file__).with_name("cad-render")),
                    "--size", "256", *options, str(src), str(dst)], check=True)
    reader = vtkPNGReader()
    reader.SetFileName(str(dst))
    reader.Update()
    scalars = reader.GetOutput().GetPointData().GetScalars()
    return vtk_to_numpy(scalars).reshape(256, 256, -1).astype(int)


def main():
    with tempfile.TemporaryDirectory() as tmp:
        src, dst = Path(tmp) / "bushing.stl", Path(tmp) / "preview.png"
        make_bushing(src)
        # A face-on annulus has constant shading on its top. Ink would leave a
        # dark ring inside that face, especially around the through bore.
        face = render(src, dst, "--view", "top")
        row = face[128, :, :3].mean(axis=1)
        bore = np.flatnonzero(row[128:] < 245)[0] + 128
        assert min(row[bore:bore + 5]) >= row[bore + 8] - 12, row[bore:bore + 10]

        polished = render(src, dst, "--view", "top", "--material", "aluminum", "--finish", "polished")
        top_face = face[:, :, :3].mean(axis=2) < 225
        assert (polished[top_face].mean(axis=1) < 240).mean() > 0.9

        blue = ["--material", "aluminum", "--color", "blue"]
        anodized = render(src, dst, *blue, "--finish", "anodized")
        powder = render(src, dst, *blue, "--finish", "matte_powder_coat")
        copper = render(src, dst, "--material", "copper", "--background", "transparent")
        mask = (copper[:, :, 3] == 255) & (anodized[:, :, 0] < 220)
        assert mask.sum() > 5000
        assert copper[0, 0, 3] == 0
        for painted in (anodized, powder):
            assert np.mean(painted[mask, 2] - painted[mask, 0]) > 20
        assert np.mean(copper[mask, 0] - copper[mask, 2]) > 20
        assert np.abs(anodized[mask] - powder[mask]).mean() > 7
        tinted_copper = render(src, dst, "--material", "copper", "--color", "blue", "--finish", "anodized")
        assert np.abs(anodized[mask] - tinted_copper[mask]).mean() > 3

        # A flat STL viewed from its edge must still make a visible profile.
        src.write_bytes(bytes(80) + struct.pack("<I12fH", 1, 0, 0, 1, -1, 0, 0, 1, 0, 0, 0, 1, 0, 0))
        profile = render(src, dst, "--view", "front")
        ys, xs = np.nonzero(profile[:, :, :3].mean(axis=2) < 230)
        assert xs.max() - xs.min() > 180
        assert ys.max() - ys.min() < 5

        # Viewed along Y, this sheet in x=z projects diagonally across the frame.
        src.write_bytes(bytes(80) + struct.pack("<I12fH", 1, 0, 0, 0, -1, 0, -1, 1, 0, 1, 0, 1, 0, 0))
        diagonal = render(src, dst, "--view", "front")
        ys, xs = np.nonzero(diagonal[:, :, :3].mean(axis=2) < 230)
        assert np.unique(xs).size > 180 and np.unique(ys).size > 180
    print("ok")


if __name__ == "__main__":
    main()
