#!/usr/bin/env python3
"""Stage pinned release assets; no publishing or npm lifecycle scripts."""
import argparse, hashlib, json, os, pathlib, platform, shutil, subprocess, tarfile, tempfile
ROOT = pathlib.Path(__file__).resolve().parents[2]
PINS = ROOT / "internal/agentsandbox/runtime"
def identity():
    return hashlib.sha256((PINS/"package-lock.json").read_bytes()+(PINS/"sources.json").read_bytes()).hexdigest()
def digest(path):
    h=hashlib.sha256()
    with open(path,"rb") as f:
        for b in iter(lambda:f.read(1024*1024),b""): h.update(b)
    return h.hexdigest()
def download(asset, cache):
    dest=cache/asset["url"].rsplit("/",1)[-1]
    if not dest.exists():
        subprocess.run(["curl","--proto","=https","--tlsv1.2","--fail","--location","--silent","--show-error","--retry","2","--output",str(dest),asset["url"]],check=True)
    if digest(dest)!=asset["sha256"]: raise ValueError("runtime artifact checksum mismatch: "+dest.name)
    return dest
def keep_regular(member,path):
    # The runtime never ships links, and the upstream links (node's npm, npx, corepack shims) aren't needed.
    # Dropping them also avoids Python builds whose "data" filter misjudges a relative link as escaping.
    if member.issym() or member.islnk(): return None
    return tarfile.data_filter(member,path)
def unpack(source,dest):
    with tarfile.open(source) as t: t.extractall(dest,filter=keep_regular)
def stage(target, dest, cache, cc):
    if target not in ["linux-amd64","linux-arm64","darwin-amd64","darwin-arm64"]: raise ValueError("unsupported runtime target")
    pins=json.loads((PINS/"sources.json").read_text()); assets=pins["assets"]
    for name, checksum in pins["recipes"].items():
        if digest(pathlib.Path(__file__).parent/name) != checksum: raise ValueError("runtime build recipe changed; update sources.json pin")
    out=dest/"srt-runtime"/identity()/target
    if out.exists(): raise ValueError("refusing to overwrite a staged runtime")
    out.mkdir(parents=True); (out/"bin").mkdir(); (out/"licenses").mkdir()
    with tempfile.TemporaryDirectory() as tmp:
        tmp=pathlib.Path(tmp)
        for kind in ["node","rg"]:
            folder=tmp/kind;folder.mkdir();unpack(download(assets[kind+"-"+target],cache),folder)
            base=next(folder.iterdir());binary=base/"bin/node" if kind=="node" else base/"rg"
            shutil.copy2(binary,out/"bin"/kind)
            for name in ["LICENSE","COPYING","LICENSE-MIT","UNLICENSE"]:
                if (base/name).is_file(): shutil.copy2(base/name,out/"licenses"/(kind+"-"+name))
        npm=out/"npm";npm.mkdir()
        for name in ["package.json","package-lock.json"]: shutil.copy2(PINS/name,npm/name)
        env=dict(os.environ,NPM_CONFIG_USERCONFIG=os.devnull)
        subprocess.run(["npm","ci","--prefix",str(npm),"--ignore-scripts","--no-bin-links","--omit=dev","--no-audit","--no-fund"],check=True,env=env)
        if target.startswith("linux-"):
            source=download(assets["socat-source"],cache);folder=tmp/"socat";folder.mkdir();unpack(source,folder);base=next(folder.iterdir())
            env=dict(os.environ,CC=cc,CFLAGS="-O2",LDFLAGS="",SOURCE_DATE_EPOCH="0")
            options=["./configure","--disable-openssl","--disable-readline","--disable-libwrap"]
            if target=="linux-arm64": options += ["--host=aarch64-linux-gnu"]
            subprocess.run(options,cwd=base,env=env,check=True)
            subprocess.run(["make","-j2","socat"],cwd=base,env=env,check=True)
            shutil.copy2(base/"socat",out/"bin/socat")
            shutil.copy2(base/"COPYING",out/"licenses/socat-COPYING")
            # GPL corresponding source and exact build recipe accompany the binary.
            shutil.copy2(source,out/source.name)
            (out/"socat-BUILD.txt").write_text("Source: "+assets["socat-source"]["url"]+"\nSHA256: "+assets["socat-source"]["sha256"]+"\ntar -xjf "+source.name+"\ncd socat-"+pins["socat"]+"\nCC="+cc+" CFLAGS=-O2 LDFLAGS= "+" ".join(options)+"\nmake -j2 socat\nTLS/readline/libwrap disabled. System glibc is dynamically linked (release baseline: Ubuntu 22.04, glibc 2.35).\n")
        (out/"bin/srt").write_text('#!/bin/sh\nroot=${0%/*}/..\nroot=$(CDPATH= cd -P "$root" && pwd) || exit 125\nexec "$root/bin/node" "$root/npm/node_modules/@anthropic-ai/sandbox-runtime/dist/cli.js" "$@"\n')
        for f in (out/"bin").iterdir(): f.chmod(0o755)
        # Lock files and original package licences remain in the distribution.
        shutil.copy2(PINS/"sources.json",out/"sources.json")
        sums=[]
        for f in sorted(out.rglob("*")):
            if f.is_symlink(): raise ValueError("runtime must not contain symlinks")
            if f.is_file(): sums.append(digest(f)+"  "+f.relative_to(out).as_posix())
        (out/"SHA256SUMS").write_text("\n".join(sums)+"\n")
        (out/"COMPLETE").write_text(identity()+"\n")
    print("Staged pinned runtime:",out)
if __name__=="__main__":
    p=argparse.ArgumentParser();p.add_argument("--platform",required=True);p.add_argument("--dest",type=pathlib.Path,required=True);p.add_argument("--cache",type=pathlib.Path,default=pathlib.Path(".runtime-cache"));p.add_argument("--cc",default="cc");a=p.parse_args();a.cache.mkdir(parents=True,exist_ok=True);stage(a.platform,a.dest.resolve(),a.cache.resolve(),a.cc)
