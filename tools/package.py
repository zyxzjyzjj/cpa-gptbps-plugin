"""Build one native plugin release archive and its checksum.

Requires Go and a native C compiler. Run separately on each target platform.
"""
import argparse
import hashlib
import os
from pathlib import Path
import subprocess
import zipfile


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--version", default="0.1.1")
    args = parser.parse_args()
    version = args.version.removeprefix("v")
    if not version or any(c not in "0123456789.-abcdefghijklmnopqrstuvwxyz" for c in version):
        parser.error("invalid version")
    root = Path(__file__).resolve().parents[1]
    goos = subprocess.check_output(["go", "env", "GOOS"], text=True).strip()
    goarch = subprocess.check_output(["go", "env", "GOARCH"], text=True).strip()
    suffix = {"windows": ".dll", "linux": ".so", "darwin": ".dylib"}[goos]
    out = root / "dist" / f"{goos}-{goarch}"
    out.mkdir(parents=True, exist_ok=True)
    plugin = out / ("codex-bps" + suffix)
    helper = out / ("bps-login" + (".exe" if goos == "windows" else ""))
    env = dict(os.environ, CGO_ENABLED="1")
    subprocess.run(["go", "build", "-trimpath", "-buildmode=c-shared",
                    "-ldflags", f"-s -w -X main.version={version}", "-o", str(plugin), "."],
                   cwd=root, env=env, check=True)
    subprocess.run(["go", "build", "-trimpath", "-ldflags", "-s -w",
                    "-o", str(helper), "./cmd/bps-login"], cwd=root, env=env, check=True)
    archive = root / "dist" / f"codex-bps_{version}_{goos}_{goarch}.zip"
    with zipfile.ZipFile(archive, "w", compression=zipfile.ZIP_DEFLATED) as bundle:
        # CPA requires the library directly in the zip root.
        bundle.write(plugin, plugin.name)
        bundle.write(helper, helper.name)
        for filename in ("README.md", "LICENSE", "config.example.yaml", "NOTICE"):
            bundle.write(root / filename, filename)
        for path in (root / "licenses").glob("*"):
            bundle.write(path, "licenses/" + path.name)
        for name in ("WORKLOG.md", "upstream-manifest.json", "validation-results.json"):
            path = root / "docs" / name
            if path.exists():
                bundle.write(path, "docs/" + name)
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    (root / "dist" / f"{archive.stem}.sha256").write_text(f"{digest}  {archive.name}\n", encoding="utf-8")
    print(archive.name, digest)


if __name__ == "__main__":
    main()
