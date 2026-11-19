#!/usr/bin/env python3
"""Post a GitHub release's notes to Slack, with only the Python standard library.

Called as the last step of a release job, after the GitHub Release exists. (A release created with the job's own
token does not start other workflows, so an `on: release` workflow would never run.)

  python3 slack_release.py <tag> --name NAME [--headline TEXT] [--brief-if REGEX] [--per-section N]

  <tag>          the release tag, e.g. v1.2.0
  --name         what was released, shown in the message (e.g. "korai-mvp")
  --headline     the first line instead of "NAME TAG released" (e.g. "is live on app.korai.tech")
  --brief-if     post one line instead of the notes when every change in them matches this regular expression
  --per-section  changes listed per section before "and N more" (default 8)

The notes are read from the release itself: GitHub's generated notes ("### Features", "* title by @user in URL"),
or any Markdown with headings and "-" / "*" bullets. The message keeps the sections, the first changes of each and
a link to the full release.

Environment: SLACK_WEBHOOK (an incoming webhook; unset skips the post), GH_TOKEN, GITHUB_REPOSITORY, GITHUB_API_URL.
A failed post is a warning, never a failed release.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import sys
import urllib.parse
import urllib.request

SECTION_LIMIT = 2900  # Slack's limit per section block is 3000 characters
SKIP_SECTIONS = {"new contributors"}
GENERATED = re.compile(r"^(?P<title>.+?) by @(?P<user>[\w.\-\[\]]+) in (?P<url>https://\S+/pull/(?P<num>\d+))$")


def escape(text: str) -> str:
    return text.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")


def inline(text: str) -> str:
    """Markdown inline formatting to Slack mrkdwn."""
    text = escape(text)
    text = re.sub(r"\[([^\]]+)\]\((https?://[^)\s]+)\)", r"<\2|\1>", text)
    text = re.sub(r"\*\*(.+?)\*\*", r"*\1*", text)
    return text


def change(item: str) -> str:
    m = GENERATED.match(item)
    if m:
        return f"<{m['url']}|#{m['num']}> {inline(m['title'])} ({escape(m['user'])})"
    return inline(item)


def parse(body: str) -> list[tuple[str, list[str]]]:
    """[(section title, [raw change])]. Continuation lines join their bullet; prose outside bullets is dropped."""
    sections: list[tuple[str, list[str]]] = [("", [])]
    skip = False
    for line in (body or "").replace("\r\n", "\n").split("\n"):
        stripped = line.strip()
        if not stripped or stripped.startswith("<!--") or stripped.startswith("**Full Changelog**"):
            continue
        heading = re.match(r"^#{1,6}\s+(.*)$", stripped)
        if heading:
            title = heading.group(1).strip()
            skip = title.lower() in SKIP_SECTIONS
            if title.lower() not in {"what's changed", "changes"} and not skip:
                sections.append((title, []))
            continue
        if skip:
            continue
        bullet = re.match(r"^[-*]\s+(.*)$", stripped)
        if bullet and not line.startswith("  "):
            sections[-1][1].append(bullet.group(1).strip())
        elif sections[-1][1] and line.startswith(" "):
            sections[-1][1][-1] += " " + stripped.lstrip("-* ")
    return [(t, items) for t, items in sections if items]


def section_text(title: str, items: list[str], per_section: int) -> str:
    head = f"*{inline(title)}* ({len(items)})\n" if title else ""
    lines = [f"• {change(i)}" for i in items[:per_section]]
    rest = len(items) - len(lines)
    while lines and len(head) + sum(len(x) + 1 for x in lines) + 20 > SECTION_LIMIT:
        lines.pop()
        rest += 1
    tail = [f"_and {rest} more_"] if rest > 0 else []
    return head + "\n".join(lines + tail)


def message(name: str, tag: str, url: str, body: str, headline: str | None = None,
            brief_if: str | None = None, per_section: int = 8) -> dict:
    sections = parse(body)
    first = f"*{escape(name)} {escape(tag)}* {escape(headline or 'released')}"
    link = f"<{url}|Release notes>"
    items = [i for _, its in sections for i in its]
    if brief_if and items and all(re.search(brief_if, i) for i in items):
        summary = "; ".join(re.sub(r"\s*\([0-9a-f]{7,}\)$", "", i) for i in items)
        text = f":package: {first}: {inline(summary)} · {link}"
        return {"text": text, "blocks": [{"type": "section", "text": {"type": "mrkdwn", "text": text}}]}
    blocks = [{"type": "section", "text": {"type": "mrkdwn", "text": f":rocket: {first}"}}]
    for title, its in sections[:45]:
        blocks.append({"type": "section", "text": {"type": "mrkdwn", "text": section_text(title, its, per_section)}})
    if not sections:
        blocks.append({"type": "section", "text": {"type": "mrkdwn", "text": "_No notes._"}})
    total = f"{len(items)} change{'s' if len(items) != 1 else ''} · " if items else ""
    blocks.append({"type": "context", "elements": [{"type": "mrkdwn", "text": f"{total}{link}"}]})
    return {"text": f"{name} {tag} {headline or 'released'}", "blocks": blocks, "unfurl_links": False}


def release(tag: str) -> dict:
    api = os.environ.get("GITHUB_API_URL", "https://api.github.com").rstrip("/")
    url = f"{api}/repos/{os.environ['GITHUB_REPOSITORY']}/releases/tags/{urllib.parse.quote(tag)}"
    headers = {"Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28"}
    if os.environ.get("GH_TOKEN"):
        headers["Authorization"] = f"Bearer {os.environ['GH_TOKEN']}"
    req = urllib.request.Request(url, headers=headers)
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.loads(r.read())


def post(webhook: str, payload: dict) -> None:
    req = urllib.request.Request(webhook, data=json.dumps(payload).encode(), method="POST",
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=30) as r:
        r.read()


def main(argv: list[str]) -> int:
    p = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    p.add_argument("tag")
    p.add_argument("--name", required=True)
    p.add_argument("--headline")
    p.add_argument("--brief-if")
    p.add_argument("--per-section", type=int, default=8)
    p.add_argument("--dry-run", action="store_true", help="print the message instead of posting it")
    a = p.parse_args(argv)
    webhook = os.environ.get("SLACK_WEBHOOK", "")
    if not webhook and not a.dry_run:
        print("::warning::SLACK_WEBHOOK is not set: the release was not posted to Slack.")
        return 0
    try:
        rel = release(a.tag)
        payload = message(a.name, a.tag, rel["html_url"], rel.get("body") or "", a.headline, a.brief_if,
                          a.per_section)
        if a.dry_run:
            print(json.dumps(payload, indent=2, ensure_ascii=False))
        else:
            post(webhook, payload)
            print(f"Posted {a.name} {a.tag} to Slack.")
    except Exception as e:  # never fail the release over the announcement
        print(f"::warning::Could not post {a.name} {a.tag} to Slack: {type(e).__name__}: {e}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
