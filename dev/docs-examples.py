#!/usr/bin/env python3
"""Writes a page for each example into docs/examples, from the scripts there.

A basic example's page has tabs for the script and what it compiles to for
GitHub Actions, CircleCI and Argo (where Argo can carry it); an advanced one
has the script alone, since advanced scripts cannot be compiled. Run by dev/docs.sh after
examples/ has been copied into docs/, and not kept in git.
"""
import json
import re
import subprocess
import sys
import tempfile
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


# The examples that are release processes are played, from a drawing in
# docs/animations; the rest get their plan as a graph.
played = {p.stem for p in (root / "docs" / "animations").glob("*.dottie")}

binary = None


def plan(script):
    """What starci plans for a push about which nothing is known."""
    global binary
    if binary is None:
        binary = Path(tempfile.mkdtemp()) / "starci"
        subprocess.run(["go", "build", "-o", str(binary), "./cmd/starci"], cwd=root, check=True)
    out = subprocess.run([str(binary), "plan", "--file", str(script)], cwd=root, check=True, capture_output=True, text=True)
    return json.loads(out.stdout)["steps"]


def graph(steps):
    """The steps as a Mermaid flowchart, an arrow for each `needs`."""
    ids = {s["name"]: f"s{i}" for i, s in enumerate(steps)}
    lines = ["flowchart LR"]
    for s in steps:
        lines.append(f'    {ids[s["name"]]}["{s["name"].replace(chr(34), "#quot;")}"]')
    for s in steps:
        for need in s.get("needs") or []:
            if need in ids:
                lines.append(f"    {ids[need]} --> {ids[s['name']]}")
    return "\n".join(lines)


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
            for label, target in (("GitHub Actions", "github"), ("CircleCI", "circleci"), ("Argo", "argo")):
                at = ex / "basic" / "compiled" / target / f"{name}.yml"
                # A pod cannot keep to everything a script can ask for.
                if at.exists():
                    tabs.append((label, fenced("yaml", at.read_text())))
        body = [f"# {name}", ""]
        # The index's one-liner, and the script's own opening paragraph, unless
        # one says what the other does.
        words = lambda t: re.sub(r"[^a-z0-9 ]", "", t.lower().replace("`", ""))
        one, opening = blurb.get((mode, name), ""), lead(src)
        if one and not (opening and (words(one).strip() in words(opening) or words(opening).strip() in words(one))):
            body += [one, ""]
        if opening:
            body += [opening, ""]
        if name in played:
            body += [
                f'<dottie-player src="../../animations/{name}.dottie" label="How a run goes through {name}"></dottie-player>',
                "",
                "Amber steps have effects outside the run, so a pass is never reused. Pause it, or step through it, with the controls.",
                "",
            ]
        elif mode == "basic":
            steps = plan(script)
            if len(steps) > 1:
                body += ["```mermaid", graph(steps), "```", "", "The steps this script plans for a push about which nothing is known. An arrow is a `needs`.", ""]
        for label, content in tabs:
            body += [f'=== "{label}"', "", indent(content), ""]
        body += ["---", ""]
        dl = [f"[{script.name}]({script.name})"]
        if mode == "basic":
            dl += [f"[GitHub Actions](compiled/github/{name}.yml)", f"[CircleCI](compiled/circleci/{name}.yml)"]
            if (ex / "basic" / "compiled" / "argo" / f"{name}.yml").exists():
                dl.append(f"[Argo](compiled/argo/{name}.yml)")
            # A module, not a file: the plan is the part that is the script's.
            if (ex / "basic" / "compiled" / "temporal" / name / "plan.go").exists():
                dl.append(f"[Temporal](compiled/temporal/{name}/plan.go)")
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
