#!/usr/bin/env python3
"""Seal nested Mach-O helpers before the app; update their integrity manifests."""
import hashlib,pathlib,subprocess,sys
app=pathlib.Path(sys.argv[1]);contents=app/"Contents";mac=contents/"MacOS"
magics={bytes.fromhex(x) for x in ["cffaedfe","cefaedfe","feedfacf","feedface","cafebabe","bebafeca","cafebabf","bfbafeca"]}
def magic(path):
 with path.open("rb") as f: return f.read(4)
# codesign treats every file in Contents/MacOS as code, so it holds only the flat executables; the runtime's
# scripts and Linux-only helpers live in Contents/Resources.
for path in mac.iterdir():
 if path.is_symlink() or not path.is_file():raise SystemExit(f"only executables belong in Contents/MacOS: {path.relative_to(app)}")
# Deepest first: nested helpers carry their own signature before anything that contains them is signed.
for path in sorted(contents.rglob("*"),key=lambda p:(-len(p.parts),str(p))):
 if path.is_file() and not path.is_symlink() and magic(path) in magics: subprocess.run(["codesign","--sign","-","--force","--timestamp=none","--preserve-metadata=entitlements,flags,runtime",str(path)],check=True)
for marker in contents.glob("Resources/srt-runtime/*/*/COMPLETE"):
 root=marker.parent;sums=[]
 for f in sorted(root.rglob("*")):
  if f.is_symlink():raise ValueError("runtime symlinks refused")
  if f.is_file() and f.name not in ["COMPLETE","SHA256SUMS"]:
   h=hashlib.sha256()
   with f.open("rb") as stream:
    for b in iter(lambda:stream.read(1024*1024),b""):h.update(b)
   sums.append(h.hexdigest()+"  "+f.relative_to(root).as_posix())
 (root/"SHA256SUMS").write_text("\n".join(sums)+"\n")
