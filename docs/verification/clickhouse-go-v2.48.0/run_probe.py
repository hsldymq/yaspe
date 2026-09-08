#!/usr/bin/env python3
"""Run white-box batch probes against a checksummed, unmodified driver copy."""

import json
import shutil
import stat
import subprocess
import sys
import tempfile
from pathlib import Path

MODULE = "github.com/ClickHouse/clickhouse-go/v2"
VERSION = "v2.48.0"
MODULE_SUM = "h1:auzd4VkapQYhQF8F2Gog7s3x78Bi1JZmByxGbrw3C+4="


def main():
    probe = Path(__file__).resolve().with_name("yaspe_probe_test.go.txt")
    with tempfile.TemporaryDirectory(prefix="yaspe-clickhouse-probe-") as tmp:
        result = subprocess.run(
            ["go", "mod", "download", "-json", f"{MODULE}@{VERSION}"],
            cwd=tmp, capture_output=True, text=True,
        )
        if result.returncode:
            print(result.stdout, end="")
            print(result.stderr, end="", file=sys.stderr)
            return result.returncode
        module = json.loads(result.stdout)
        if module.get("Version") != VERSION or module.get("Sum") != MODULE_SUM:
            raise RuntimeError("downloaded module does not match the verified baseline")
        target = Path(tmp) / "driver"
        # Upstream tests are excluded so no server, Docker, or unrelated suite runs.
        shutil.copytree(
            module["Dir"], target,
            ignore=shutil.ignore_patterns(
                "*_test.go", "tests", "examples", ".github", ".agents", "benchmark", "benchmarks"
            ),
        )
        target.chmod(target.stat().st_mode | stat.S_IWUSR)
        shutil.copyfile(probe, target / "yaspe_probe_test.go")
        print(f"Baseline: {MODULE}@{VERSION}", flush=True)
        print(f"Module checksum: {MODULE_SUM}", flush=True)
        subprocess.run(["go", "version"], check=True)
        return subprocess.run(
            ["go", "test", "-mod=readonly", "-race", "-v", "-count=1",
             "-timeout", "45s", "-run", "^TestProbe", "."],
            cwd=target,
        ).returncode


if __name__ == "__main__":
    sys.exit(main())
