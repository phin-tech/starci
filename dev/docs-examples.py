#!/usr/bin/env python3
"""Writes a page for each example into docs/examples, from the scripts there.

A basic example's page has tabs for the script, what it compiles to for GitHub
Actions and what it compiles to for CircleCI; an advanced one has the script
alone, since advanced scripts cannot be compiled. Run by dev/docs.sh after
examples/ has been copied into docs/, and not kept in git.
"""
import re
import sys
from pathlib import Path

root = Path(__file__).resolve().parent.parent
ex = root / "docs" / "examples"
nav = (root / "mkdocs.yml").read_text()


def lead(src):
    """The first paragraph of the comment a script opens with."""
    out = []
    for line in src.splitlines():
        if not line.startswith("#"):
            break
        text = line[1:].removeprefix(" ")
        if not text.strip():
            if out:
                break
            continue
        out.append(text)
    return " ".join(out)


# The one-line descriptions in the index.
index = (ex / "README.md").read_text()
blurb = {}
for m in re.finditer(r"^\| \[([^\]]+)\]\((basic|advanced)/[^)]+\) \| (.*) \|$", index, re.M):
    blurb[(m.group(2), m.group(1))] = m.group(3)


def indent(text, n=4):
    return "\n".join((" " * n + l) if l else "" for l in text.splitlines())


def fenced(lang, text):
    return f"````{lang}\n{text.rstrip()}\n````"


missing = []
pages = 0
for mode, glob, tail in (("basic", "*.ci.star", ".ci.star"), ("advanced", "*.pipeline.star", ".pipeline.star")):
    for script in sorted((ex / mode).glob(glob)):
        name = script.name.removesuffix(tail)
        src = script.read_text()
        tabs = [("Script", fenced("python", src))]
        if mode == "basic":
            for label, target in (("GitHub Actions", "github"), ("CircleCI", "circleci")):
                tabs.append((label, fenced("yaml", (ex / "basic" / "compiled" / target / f"{name}.yml").read_text())))
        body = [f"# {name}", ""]
        if (mode, name) in blurb:
            body += [blurb[(mode, name)], ""]
        if lead(src):
            body += [lead(src), ""]
        for label, content in tabs:
            body += [f'=== "{label}"', "", indent(content), ""]
        body += ["---", ""]
        dl = [f"[{script.name}]({script.name})"]
        if mode == "basic":
            dl += [f"[GitHub Actions](compiled/github/{name}.yml)", f"[CircleCI](compiled/circleci/{name}.yml)"]
        body.append("Download: " + ", ".join(dl))
        if mode == "advanced":
            body += ["", "An advanced script is only known as it runs, so it cannot be compiled."]
        (ex / mode / f"{name}.md").write_text("\n".join(body) + "\n")
        pages += 1
        if f"examples/{mode}/{name}.md" not in nav:
            missing.append(f"examples/{mode}/{name}.md")

# In the index, an example links to its page, not to the bare script.
index = re.sub(r"\]\((basic|advanced)/([^)]+?)\.(?:ci|pipeline)\.star\)", r"](\1/\2.md)", index)
index = index.replace("(basic/compiled/github/events.yml)", "(basic/events.md)").replace("(basic/compiled/github/services.yml)", "(basic/services.md)")
(ex / "README.md").write_text(index)

if missing:
    sys.exit("not in the nav in mkdocs.yml:\n  " + "\n  ".join(missing))
print(f"{pages} example pages")
