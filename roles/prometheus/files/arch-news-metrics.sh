#!/usr/bin/env python3
"""Reports the newest item on the Arch Linux news feed.

Arch occasionally requires a manual step before an upgrade -- a keyring
refresh, a package replaced by another, a config that must be moved -- and
skipping it can leave a system that does not boot. The upgrade playbook says to
read the news first, which depends on remembering to.

The metric this writes is compared against the last full upgrade, so the alert
means "something was published that you have not upgraded past" and clears
itself once you have.
"""

import email.utils
import html
import os
import re
import sys
import tempfile
import time
import urllib.request
import xml.etree.ElementTree as ET

FEED = "https://archlinux.org/feeds/news/"
TEXTFILE_DIR = "/var/lib/node_exporter/textfile_collector"
METRICS_FILE = os.path.join(TEXTFILE_DIR, "arch_news.prom")


def label_escape(s):
    return s.replace("\\", "\\\\").replace('"', '\\"').replace("\n", " ")


def main():
    try:
        with urllib.request.urlopen(FEED, timeout=30) as r:
            root = ET.fromstring(r.read())
    except Exception as e:
        # The previous file is left in place: a feed that is unreachable today
        # says nothing about whether news exists, and blanking the metric would
        # read as "no news" rather than "did not look".
        print(f"fetching {FEED}: {e}", file=sys.stderr)
        return 1

    item = root.find("./channel/item")
    if item is None:
        print("feed carried no items", file=sys.stderr)
        return 1

    title = (item.findtext("title") or "").strip()
    title = html.unescape(re.sub(r"<[^>]+>", "", title))
    published = item.findtext("pubDate") or ""
    try:
        stamp = int(email.utils.parsedate_to_datetime(published).timestamp())
    except Exception as e:
        print(f"unparseable pubDate {published!r}: {e}", file=sys.stderr)
        return 1

    body = (
        "# HELP arch_news_latest_timestamp_seconds Publication time of the newest item on the Arch Linux news feed.\n"
        "# TYPE arch_news_latest_timestamp_seconds gauge\n"
        f'arch_news_latest_timestamp_seconds{{title="{label_escape(title)}"}} {stamp}\n'
        "# HELP arch_news_check_timestamp_seconds Unix time of the last successful read of the news feed.\n"
        "# TYPE arch_news_check_timestamp_seconds gauge\n"
        f"arch_news_check_timestamp_seconds {int(time.time())}\n"
    )

    # Atomic write, for the reason the other collectors give: node_exporter must
    # never scrape a half-written file, and a temporary name ending in .prom
    # would itself be scraped.
    os.makedirs(TEXTFILE_DIR, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=TEXTFILE_DIR, suffix=".tmp")
    try:
        with os.fdopen(fd, "w") as f:
            f.write(body)
        os.chmod(tmp, 0o644)
        os.replace(tmp, METRICS_FILE)
    except Exception:
        os.unlink(tmp)
        raise
    return 0


if __name__ == "__main__":
    sys.exit(main())
