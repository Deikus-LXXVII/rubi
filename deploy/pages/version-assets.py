#!/usr/bin/env python3
"""Adds ?v=<version> to the panel's own scripts, styles and images, so browsers fetch a new release at once
instead of keeping old files from their cache (rubi-panel.com lets browsers cache them for hours).

Every reference to a module gets the same version, so each module still loads exactly once.
Usage: version-assets.py <panel dir> <version>
"""
import pathlib
import re
import sys

root, version = pathlib.Path(sys.argv[1]), sys.argv[2]
assert re.fullmatch(r"[0-9A-Za-z._-]+", version), version
local = r'(?!https?:|data:|#|/)([^"?#]+\.(?:js|css|svg|png|webp))'

for page in root.glob("*.html"):
    s = page.read_text()
    s = re.sub(r'((?:src|href)=")' + local + '"', lambda m: f'{m[1]}{m[2]}?v={version}"', s)
    page.write_text(s)

for script in root.glob("js/*.js"):
    s = script.read_text()
    s = re.sub(r'(from\s+")(\.\.?/[^"?]+\.js)"', lambda m: f'{m[1]}{m[2]}?v={version}"', s)
    script.write_text(s)
