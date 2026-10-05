#!/usr/bin/env python3
"""Print every Grafana dashboard query as a recording rule, so promtool can
check that each one parses (make observability-check)."""
import json
import sys

dashboard = json.load(open(sys.argv[1]))
print("groups:\n  - name: dashboard-check\n    rules:")
n = 0
for panel in dashboard["panels"]:
    for target in panel.get("targets", []):
        print(f"      - record: dashboard:check_{n}")
        print("        expr: " + json.dumps(target["expr"]))
        n += 1
