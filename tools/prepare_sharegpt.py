"""Build assets/sharegpt.jsonl from ShareGPT (download, or local JSON path).

Usage: python prepare_sharegpt.py [local_sharegpt.json|-] [out.jsonl]
"""
import json
import os
import random
import re
import sys
import tempfile
import urllib.request

URL = ("https://huggingface.co/datasets/anon8231489123/ShareGPT_Vicuna_unfiltered/"
       "resolve/main/ShareGPT_V3_unfiltered_cleaned_split.json")


# Real user conversations contain leaked credentials; drop those rows (GitHub push protection rejects them).
SECRET = re.compile(
    r"ya29\.[\w-]{20,}|AIza[\w-]{35}|AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{36}|github_pat_\w{20,}"
    r"|sk-[A-Za-z0-9_-]{20,}|sk_live_\w{16,}|xox[abprs]-[\w-]{10,}|-----BEGIN [A-Z ]*PRIVATE KEY-----"
    r"|[MN][A-Za-z\d]{23,}\.[\w-]{6}\.[\w-]{27,}|eyJ[\w-]{10,}\.eyJ[\w-]{10,}\.[\w-]{10,}"
    r"|https://hooks\.slack\.com/\S+|AC[0-9a-f]{32}|SG\.[\w-]{22}\.[\w-]{43}")


def download(url):
    fd, path = tempfile.mkstemp(suffix=".json")
    with os.fdopen(fd, "wb") as f, urllib.request.urlopen(url) as r:
        while chunk := r.read(1 << 20):
            f.write(chunk)
    return path


def build(src, dst, n=3000):
    with open(src, encoding="utf-8") as f:
        data = json.load(f)
    rows = []
    for item in data:
        c = item.get("conversations") or []
        if len(c) < 2 or c[0].get("from") != "human" or c[1].get("from") != "gpt":
            continue
        prompt = c[0].get("value") or ""
        out_words = len((c[1].get("value") or "").split())
        if 4 <= len(prompt.split()) <= 1500 and 4 <= out_words <= 1500 and not SECRET.search(prompt):
            rows.append({"prompt": prompt, "output_words": out_words})
    random.Random(0).shuffle(rows)
    os.makedirs(os.path.dirname(os.path.abspath(dst)), exist_ok=True)
    with open(dst, "w", encoding="utf-8") as f:
        for r in rows[:n]:
            f.write(json.dumps(r, ensure_ascii=False) + "\n")
    return min(n, len(rows))


if __name__ == "__main__":
    src = sys.argv[1] if len(sys.argv) > 1 and sys.argv[1] != "-" else None
    dst = sys.argv[2] if len(sys.argv) > 2 else "assets/sharegpt.jsonl"
    tmp = None if src else download(URL)
    try:
        print(f"wrote {build(src or tmp, dst)} prompts to {dst}")
    finally:
        if tmp:
            os.remove(tmp)
