"""Command-level checks of advisory decisions; no guests or downloads."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[2] / "scripts/check-dependency-vulnerabilities.py"
ADVISORY_ID = "GO-2026-1234"
MODULE = "example.com/dependency"


class DependencyVulnerabilityTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.repository = self.root / "repository"
        (self.repository / "scripts").mkdir(parents=True)
        shutil.copyfile(SCRIPT, self.repository / "scripts" / SCRIPT.name)
        (self.repository / "go.mod").write_text(
            "module example.com/application\n\ngo 1.26.0\n\nreplace (\n"
            "example.com/dependency => ./third_party/example.com/dependency\n)\n")
        (self.repository / ".git").mkdir()
        self.record(MODULE, "None.")
        self.commands = self.root / "commands"
        self.commands.mkdir()
        # The Go stand-in checks the upstream copy, then installs the scanner
        # stand-in. Decisions still run through the script's normal CLI.
        fake_go = self.commands / "go"
        fake_go.write_text(f"#!{sys.executable}\n" + '''
import os
from pathlib import Path
import sys
if sys.argv[1:] == ['env', 'GOVERSION']:
    print('go1.26.9')
    sys.exit(0)
assert not Path('.git').exists()
assert not Path('third_party').exists()
assert 'replace' not in Path('go.mod').read_text()
assert os.environ['GOTOOLCHAIN'] == 'go1.26.9'
Path(os.environ['COPY_LOCATION']).write_text(str(Path.cwd()))
if sys.argv[1:] == ['mod', 'tidy']:
    sys.exit(int(os.environ.get('TIDY_STATUS', '0')))
assert sys.argv[1:] == ['install', 'golang.org/x/vuln/cmd/govulncheck@v1.9.0']
scanner = Path(os.environ['GOBIN']) / 'govulncheck'
scanner.parent.mkdir()
scanner.write_text("#!" + sys.executable + "\\n" + """
import os
from pathlib import Path
import sys
assert sys.argv[1:] == ['-format', 'json', './...']
print(Path(os.environ['FINDINGS']).read_text())
sys.exit(int(os.environ.get('SCANNER_STATUS', '0')))
""")
scanner.chmod(0o755)
''')
        fake_go.chmod(0o755)

    def record(self, module, applied_fixes):
        record = self.repository / "third_party" / module / "PRUNED_COPY_RECORD.md"
        record.parent.mkdir(parents=True, exist_ok=True)
        record.write_text(f"# Copy\n\n- Module: `{module}`, version `v1.0.0`.\n\n"
                          f"## Applied fixes\n\n{applied_fixes}\n\n"
                          "## Other notes\n\nGO-2026-9999 is not an applied fix.\n")

    def stream(self, module=MODULE, level="symbol", advisory_id=ADVISORY_ID):
        # Shape follows golang.org/x/vuln@v1.9.0/internal/govulncheck.
        frame = {"module": module, "version": "v1.0.0"}
        if level != "module":
            frame["package"] = module + "/parser"
        trace = [frame]
        if level == "symbol":
            frame.update(function="Parse", receiver="Decoder")
            trace.append({"module": "example.com/application", "package": "example.com/application",
                          "function": "main", "position": {"filename": "main.go", "line": 12}})
        messages = [
            {"config": {"protocol_version": "v1.0.0", "scan_level": "symbol"}},
            {"progress": {"message": "Scanning"}},
            {"finding": {"osv": advisory_id, "fixed_version": "v1.0.1", "trace": trace}},
            # OSV may arrive after a finding; messages have no ordering guarantee.
            {"osv": {"id": advisory_id, "summary": "Untrusted input crashes the parser"}},
        ]
        return "\n".join(json.dumps(message, indent=2) for message in messages)

    def check(self, output, **extra_env):
        findings = self.root / "findings.json"
        findings.write_text(output)
        location = self.root / "copy-location"
        result = subprocess.run(
            [sys.executable, str(self.repository / "scripts" / SCRIPT.name)],
            cwd=self.root, text=True, capture_output=True,
            env=dict(os.environ, PATH=str(self.commands) + os.pathsep + os.environ["PATH"],
                     FINDINGS=str(findings), COPY_LOCATION=str(location), **extra_env))
        if location.exists():
            self.assertFalse(Path(location.read_text()).exists(), "The upstream copy was not cleaned up")
        return result

    def test_reached_unhandled_advisory_fails_with_call_path(self):
        result = self.check(self.stream())
        self.assertEqual(result.returncode, 1, result.stderr)
        for expected in (ADVISORY_ID, "Untrusted input crashes the parser", MODULE,
                         "v1.0.0", "v1.0.1", "main.go:12", "Decoder.Parse"):
            self.assertIn(expected, result.stdout)
        self.assertLess(result.stdout.index("main.go:12"), result.stdout.index("Decoder.Parse"))

    def test_fix_in_own_module_passes_and_is_reported(self):
        self.record(MODULE, f"- {ADVISORY_ID}: Apply the upstream parser fix.")
        result = self.check(self.stream())
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(f"Reached advisory {ADVISORY_ID} in {MODULE} is listed as fixed", result.stdout)

    def test_fix_in_another_module_does_not_suppress(self):
        self.record("example.com/other", f"- {ADVISORY_ID}: Fix.")
        self.assertEqual(self.check(self.stream()).returncode, 1)

    def test_id_outside_applied_fixes_does_not_suppress(self):
        self.assertEqual(self.check(self.stream(advisory_id="GO-2026-9999")).returncode, 1)

    def test_package_and_module_findings_pass(self):
        for level in ("package", "module"):
            with self.subTest(level=level):
                result = self.check(self.stream(level=level))
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn("No reached", result.stdout)
                self.assertNotIn(ADVISORY_ID, result.stdout)

    def test_reached_standard_library_advisory_fails(self):
        self.assertEqual(self.check(self.stream(module="stdlib")).returncode, 1)

    def test_repeated_paths_report_advisory_once(self):
        result = self.check(self.stream() + "\n" + self.stream())
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertEqual(result.stdout.count("is not handled"), 1)

    def test_malformed_output_is_check_failure(self):
        for output in ("not json", self.stream() + "\n{", "", '{"finding": {}}',
                       '{"config": {}}\n{"unexpected": {}}',
                       '{"config": {}}\n{"finding": {"trace": [null]}}'):
            with self.subTest(output=output):
                result = self.check(output)
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertIn("could not run", result.stderr)

    def test_go_failure_is_check_failure(self):
        result = self.check(self.stream(), TIDY_STATUS="1")
        self.assertEqual(result.returncode, 2)
        self.assertIn("go mod tidy failed", result.stderr)

    def test_scanner_failure_is_check_failure(self):
        result = self.check(self.stream(), SCANNER_STATUS="1")
        self.assertEqual(result.returncode, 2)
        self.assertIn("govulncheck -format json ./... failed", result.stderr)


if __name__ == "__main__":
    unittest.main()
