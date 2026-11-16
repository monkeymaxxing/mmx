#!/usr/bin/env python3
"""Check upstream dependencies, accounting for fixes in the pruned copies."""

import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tempfile


def recorded_fixes(repository):
    fixes = {}
    for record in sorted((repository / "third_party").rglob("PRUNED_COPY_RECORD.md")):
        text = record.read_text()
        module = re.search(r"(?m)^- Module: `([^`]+)`", text)
        section = re.search(r"(?ms)^## Applied fixes\s*\n(.*?)(?=^## |\Z)", text)
        if not module or not section:
            raise ValueError(f"Cannot read the module or Applied fixes section in {record}.")
        fixes[module[1]] = set(re.findall(r"\bGO-\d{4}-\d{4,}\b", section[1]))
    return fixes


def report_findings(output, fixes):
    """Read govulncheck's JSON stream and return the check's exit status."""
    advisories = {}
    reached = {}
    decoder = json.JSONDecoder()
    remaining = output.strip()
    saw_config = False
    while remaining:
        message, end = decoder.raw_decode(remaining)
        remaining = remaining[end:].lstrip()
        if not isinstance(message, dict):
            raise ValueError("govulncheck output contains a message that is not an object.")
        if not message or any(key not in ("config", "progress", "osv", "finding", "SBOM")
                              or not isinstance(value, dict) for key, value in message.items()):
            raise ValueError("govulncheck output contains an invalid message.")
        if "config" in message:
            saw_config = True
        if "osv" in message:
            advisory = message["osv"]
            advisories[advisory["id"]] = advisory
        if "finding" in message:
            finding = message["finding"]
            trace = finding["trace"]
            if not isinstance(trace, list) or not trace or any(not isinstance(frame, dict) for frame in trace):
                raise ValueError("govulncheck output contains an invalid finding trace.")
            vulnerable = trace[0]
            advisory_id = finding["osv"]
            module = vulnerable["module"]
            if not isinstance(advisory_id, str) or not isinstance(module, str):
                raise ValueError("govulncheck output contains an invalid advisory ID or module path.")
            if not isinstance(vulnerable.get("function", ""), str):
                raise ValueError("govulncheck output contains an invalid function name.")
            if vulnerable.get("function"):
                reached.setdefault((advisory_id, module), finding)
    if not saw_config:
        raise ValueError("govulncheck output has no config message.")

    unhandled = False
    for (advisory_id, module), finding in sorted(reached.items()):
        if module not in ("stdlib", "toolchain") and advisory_id in fixes.get(module, set()):
            print(f"Reached advisory {advisory_id} in {module} is listed as fixed in its pruned copy's record.")
            continue
        unhandled = True
        summary = advisories[advisory_id]["summary"]
        vulnerable = finding["trace"][0]
        fixed_version = finding.get("fixed_version") or "no fixed version is available"
        print(f"Reached advisory {advisory_id} is not handled: {summary}")
        print(f"The vulnerable module is {module} at {vulnerable['version']}; fixed version: {fixed_version}.")
        call_path = []
        for frame in reversed(finding["trace"]):
            symbol = ".".join(filter(None, (frame.get("package"), frame.get("receiver"), frame.get("function"))))
            position = frame.get("position")
            if position:
                symbol += f" ({position['filename']}:{position['line']})"
            call_path.append(symbol)
        print("The call path is " + " -> ".join(call_path) + ".")
    if not unhandled:
        print("No reached dependency or Go toolchain vulnerabilities remain unhandled.")
    return int(unhandled)


def run(command, directory, env):
    result = subprocess.run(command, cwd=directory, env=env, text=True, capture_output=True)
    if result.returncode:
        raise RuntimeError(f"{' '.join(map(str, command))} failed with exit status {result.returncode}: "
                           + (result.stderr.strip() or result.stdout.strip()))
    return result.stdout


def main():
    repository = Path(__file__).resolve().parent.parent
    try:
        fixes = recorded_fixes(repository)
        with tempfile.TemporaryDirectory(prefix="go-dependency-vulnerability-check-") as temporary:
            temporary = Path(temporary)
            repository_copy = temporary / "repository"
            shutil.copytree(repository, repository_copy, ignore=shutil.ignore_patterns(".git", "third_party"))
            module_file = repository_copy / "go.mod"
            module_file.write_text(re.sub(r"(?ms)^replace \(\n.*?^\)\s*\n", "", module_file.read_text()))
            env = dict(os.environ, GOBIN=str(temporary / "bin"), GOWORK="off")
            # A versioned go install otherwise selects its own module's
            # toolchain, which may be newer than this repository's toolchain.
            env["GOTOOLCHAIN"] = run(["go", "env", "GOVERSION"], repository, env).strip()
            run(["go", "mod", "tidy"], repository_copy, env)
            run(["go", "install", "golang.org/x/vuln/cmd/govulncheck@v1.9.0"], repository_copy, env)
            output = run([str(temporary / "bin" / "govulncheck"), "-format", "json", "./..."], repository_copy, env)
            return report_findings(output, fixes)
    except (OSError, RuntimeError, ValueError, KeyError, TypeError, IndexError) as error:
        print(f"The dependency vulnerability check could not run: {error}", file=sys.stderr)
        return 2


def interrupted(signum, frame):
    # Unwind the temporary-directory context on the usual termination signals.
    raise KeyboardInterrupt


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGHUP, interrupted)
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        print("The dependency vulnerability check could not run: interrupted.", file=sys.stderr)
        sys.exit(2)
