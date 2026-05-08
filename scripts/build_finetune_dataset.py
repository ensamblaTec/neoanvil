#!/usr/bin/env python3
"""build_finetune_dataset.py — Scaffold para dataset contrastivo de fine-tuning embedding.

Épica 303.F: genera pares (query, snippet) desde el workspace neoanvil para fine-tuning
de un modelo de embeddings especializado en código Go + logs + esquemas industriales.

Flujo post-dataset:
  1. python build_finetune_dataset.py --workspace /path/to/neoanvil --out dataset.jsonl
  2. sentence-transformers/train_mn.py --model nomic-embed-text --dataset dataset.jsonl
  3. llama.cpp/quantize model.bin model_q8.gguf Q8_0
  4. ollama create neo-embed -f Modelfile.finetune

Uso:
  python scripts/build_finetune_dataset.py \\
    --workspace /home/ensamblatec/go/src/github.com/ensamblatec/neoanvil \\
    --out .neo/finetune/dataset.jsonl \\
    --target 500
"""
import argparse
import ast
import json
import os
import re
import sys
from pathlib import Path


# ---------------------------------------------------------------------------
# Extractors
# ---------------------------------------------------------------------------

def extract_go_pairs(go_file: Path) -> list[dict]:
    """Extract (doc_comment → func_body) pairs from a Go source file."""
    pairs = []
    text = go_file.read_text(errors="replace")
    # Match exported functions/methods with doc comments.
    # Pattern: optional multiline comment, then func signature + body (up to first blank+}).
    pattern = re.compile(
        r'(/(?:/[^\n]*\n)+)'          # // doc comment lines
        r'func\s+(\w[\w*]*\s+)?\(.*?\)\s*(?:\(.*?\)\s*)?{',
        re.MULTILINE,
    )
    for m in pattern.finditer(text):
        doc = m.group(1).strip()
        if len(doc) < 20:  # skip trivial one-word comments
            continue
        # Grab up to 30 lines after the match as the snippet
        start = m.start()
        lines = text[start:start + 2000].splitlines()[:30]
        snippet = "\n".join(lines)
        # Clean doc into a natural-language query
        query = " ".join(
            line.lstrip("/ ").strip()
            for line in doc.splitlines()
            if line.strip().startswith("//")
        )
        if len(query) > 10 and len(snippet) > 30:
            pairs.append({"query": query, "positive": snippet, "source": str(go_file)})
    return pairs


def extract_yaml_pairs(yaml_file: Path) -> list[dict]:
    """Extract (field_name query → yaml_block) pairs from neo.yaml-style config."""
    pairs = []
    text = yaml_file.read_text(errors="replace")
    # Sections delimited by top-level keys
    section_pat = re.compile(r'^(\w+):.*\n((?:[ \t]+.*\n)*)', re.MULTILINE)
    for m in section_pat.finditer(text):
        section_name = m.group(1)
        section_body = m.group(2)
        if len(section_body) < 20:
            continue
        query = f"configuration for {section_name} in neoanvil neo.yaml"
        snippet = f"{section_name}:\n{section_body[:500]}"
        pairs.append({"query": query, "positive": snippet, "source": str(yaml_file)})
    return pairs


def extract_markdown_pairs(md_file: Path) -> list[dict]:
    """Extract (heading → section_body) pairs from markdown docs."""
    pairs = []
    text = md_file.read_text(errors="replace")
    heading_pat = re.compile(r'^(#{1,3})\s+(.+)$', re.MULTILINE)
    headings = list(heading_pat.finditer(text))
    for i, h in enumerate(headings):
        heading_text = h.group(2).strip()
        start = h.end()
        end = headings[i + 1].start() if i + 1 < len(headings) else len(text)
        body = text[start:end].strip()[:600]
        if len(body) < 30:
            continue
        query = f"how does {heading_text} work in neoanvil"
        pairs.append({"query": query, "positive": body, "source": str(md_file)})
    return pairs


# ---------------------------------------------------------------------------
# Corpus walk
# ---------------------------------------------------------------------------

SKIP_DIRS = {"vendor", "node_modules", ".git", ".neo", "web", "testdata"}


def collect_pairs(workspace: Path, target: int) -> list[dict]:
    pairs = []
    for root, dirs, files in os.walk(workspace):
        dirs[:] = [d for d in dirs if d not in SKIP_DIRS]
        for fname in files:
            fp = Path(root) / fname
            try:
                if fname.endswith(".go") and not fname.endswith("_test.go"):
                    pairs.extend(extract_go_pairs(fp))
                elif fname == "neo.yaml":
                    pairs.extend(extract_yaml_pairs(fp))
                elif fname.endswith(".md") and fp.parent.name in ("docs", ".claude", ""):
                    pairs.extend(extract_markdown_pairs(fp))
            except Exception as exc:
                print(f"[WARN] {fp}: {exc}", file=sys.stderr)
            if len(pairs) >= target * 3:  # over-collect, dedupe later
                break
        if len(pairs) >= target * 3:
            break

    # Deduplicate on query prefix
    seen: set[str] = set()
    deduped = []
    for p in pairs:
        key = p["query"][:60]
        if key not in seen:
            seen.add(key)
            deduped.append(p)

    return deduped[:target]


# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------

def main():
    ap = argparse.ArgumentParser(description="Build embedding fine-tune dataset")
    ap.add_argument("--workspace", default=".", help="Root of the neoanvil workspace")
    ap.add_argument("--out", default=".neo/finetune/dataset.jsonl", help="Output JSONL")
    ap.add_argument("--target", type=int, default=500, help="Target number of pairs")
    args = ap.parse_args()

    workspace = Path(args.workspace).resolve()
    out_path = Path(args.out)
    out_path.parent.mkdir(parents=True, exist_ok=True)

    print(f"[INFO] Walking {workspace} for {args.target} pairs…")
    pairs = collect_pairs(workspace, args.target)
    print(f"[INFO] Collected {len(pairs)} pairs")

    with out_path.open("w") as f:
        for p in pairs:
            f.write(json.dumps(p, ensure_ascii=False) + "\n")

    print(f"[OK]  Dataset written to {out_path}")
    print()
    print("Next steps:")
    print("  1. Review/curate .neo/finetune/dataset.jsonl")
    print("  2. pip install sentence-transformers datasets")
    print("  3. python scripts/finetune_embed.py --dataset .neo/finetune/dataset.jsonl")
    print("  4. (optional) llama.cpp/quantize model.bin model_q8.gguf Q8_0")
    print("  5. ollama create neo-embed -f scripts/Modelfile.finetune")


if __name__ == "__main__":
    main()
