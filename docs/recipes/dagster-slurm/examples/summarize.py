"""Cluster payload: read every simulation result and report a data check."""
import json
import os
from pathlib import Path

from dagster_pipes import open_dagster_pipes

with open_dagster_pipes() as context:
    expected = context.extras["expected"]
    sweep_dir = Path(os.environ["SWEEP_DIR"])

    results = {}
    for name in expected:
        path = sweep_dir / ("%s.json" % name)
        if path.exists():
            results[name] = json.loads(path.read_text())
    missing = sorted(set(expected) - set(results))
    context.log.info("found %d of %d results" % (len(results), len(expected)))

    # The check shows up in Dagster next to the asset, passed or failed.
    context.report_asset_check(
        check_name="all_runs_present",
        passed=not missing,
        metadata={"missing": ",".join(missing)},
    )

    # With no result at all there is nothing to compare; the failed check
    # above is the whole story.
    summary = {"runs": len(results)}
    if results:
        farthest = max(results.values(), key=lambda r: r["distance"])
        summary["farthest"] = farthest["name"]
        summary["farthest_distance"] = farthest["distance"]
        summary["hosts"] = ",".join(sorted({r["host"] for r in results.values()}))
    context.report_asset_materialization(metadata=summary)
