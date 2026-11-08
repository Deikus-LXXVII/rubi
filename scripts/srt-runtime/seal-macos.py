#!/usr/bin/env python3
"""Seal nested Mach-O helpers before the app; update their integrity manifests."""
import hashlib,pathlib,subprocess,sys
app=pathlib.Path(sys.argv[1]);mac=app/"Contents/MacOS"
magics={bytes.fromhex(x) for x in ["cffaedfe","cefaedfe","feedfacf","feedface","cafebabe","bebafeca","cafebabf","bfbafeca"]}
for path in sorted(mac.rglob("*")):
 if path.is_file():
  with path.open("rb") as f: magic=f.read(4)
  if magic in magics: subprocess.run(["codesign","--sign","-","--force","--timestamp=none","--preserve-metadata=entitlements,flags,runtime",str(path)],check=True)
for marker in mac.glob("srt-runtime/*/*/COMPLETE"):
 root=marker.parent;sums=[]
 for f in sorted(root.rglob("*")):
  if f.is_symlink():raise ValueError("runtime symlinks refused")
  if f.is_file() and f.name not in ["COMPLETE","SHA256SUMS"]:
   h=hashlib.sha256()
   with f.open("rb") as stream:
    for b in iter(lambda:stream.read(1024*1024),b""):h.update(b)
   sums.append(h.hexdigest()+"  "+f.relative_to(root).as_posix())
 (root/"SHA256SUMS").write_text("\n".join(sums)+"\n")
