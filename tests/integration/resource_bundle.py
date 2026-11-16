"""History expiry, dryrun candidates, and package absence through the public CLI."""
import json
from datetime import datetime, timedelta, timezone


def test(workspace, run, project, remote, mark):
    base = "/var/tmp/mmx-integration"
    path = base + "/preview-config"
    remote(f"printf live > {path}; chmod 640 {path}")
    before = remote(f"stat -c '%i %a %U %G %Y' {path}; cat {path}")
    hashes = remote("find /run/mmx/services -type f -exec sha256sum {} + | sort")
    file = {"id": "candidate", "file": {"path": path, "content": "proposed", "permissions_mode": "0600",
            "check_new_file": {"shell": {"script": f"touch {base}/apply-validator-ran",
                "dryrun": "test \"$(cat \"$NEW_FILE\")\" = proposed",
                "env": {"NEW_FILE": "{{ .new_file.path }}"}}}}}

    def result(record):
        return record["entries"][0]["response"]["results"][0]

    project([file], "candidate_preview")
    r = result(run("dryrun"))
    assert r["change"] == "would_change" and r["check_new_file"]["status"] == "succeeded"
    assert remote(f"stat -c '%i %a %U %G %Y' {path}; cat {path}") == before
    remote(f"test ! -e {base}/apply-validator-ran")
    mark("dryrun validates candidate bytes without changing live content or metadata")

    file["file"]["check_new_file"]["shell"]["dryrun"] = "echo rejected >&2; exit 1"
    project([file], "candidate_preview")
    r = result(run("dryrun", 1))
    assert r["change"] == "would_change" and r["check_new_file"]["status"] == "failed"
    assert "rejected" in r["check_new_file"]["stderr"]
    assert remote(f"stat -c '%i %a %U %G %Y' {path}; cat {path}") == before
    mark("dryrun reports rejected validation separately from the proposed change")

    for inspection in ["", "mmx-validator-not-installed"]:
        file["file"]["check_new_file"]["shell"]["dryrun"] = inspection
        project([file], "candidate_preview")
        r = result(run("dryrun"))
        assert r["check_new_file"]["status"] == "skipped" and "not checked" in r["check_new_file"]["reason"]
    file["file"]["path"] = base + "/missing-directory/config"
    project([file], "candidate_preview")
    r = result(run("dryrun"))
    assert r["status"] == "not_checked" and "the parent directory " + base + "/missing-directory does not exist" in r["reason"], r
    remote(f"test ! -d {base}/missing-directory; test ! -e {base}/apply-validator-ran")
    assert remote(f"find {base} -name '.mmx-candidate-*'") == ""
    assert remote("find /run/mmx/services -type f -exec sha256sum {} + | sort") == hashes
    mark("missing dryrun prerequisites are not checked; candidates are removed and service hashes stay unchanged")

    metadata_path = base + "/permissions-only"
    remote(f"printf preserved > {metadata_path}; chmod 640 {metadata_path}")
    identity = remote(f"stat -c '%i %s %Y' {metadata_path}; sha256sum {metadata_path}")
    project([{"file": {"path": metadata_path, "owner": "nobody", "group": "users", "permissions_mode": "0600"}}], "file_metadata")
    assert result(run("dryrun"))["change"] == "would_change"
    assert remote(f"stat -c '%a %U %G' {metadata_path}").strip() == "640 root root"
    assert result(run("run"))["change"] == "changed"
    assert remote(f"stat -c '%a %U %G' {metadata_path}").strip() == "600 nobody users"
    assert remote(f"stat -c '%i %s %Y' {metadata_path}; sha256sum {metadata_path}") == identity
    assert result(run("run"))["change"] == "unchanged"
    mark("permissions-only apply changes ownership and mode without rewriting content; preview and rerun leave it alone")

    # The removal of a package with apt; server_resources.py covers dnf.
    if remote("command -v dpkg-deb || true"):
        # A real, isolated package with a conffile. '+' exercises apt's action-suffix syntax.
        package = "mmx-removal-fixture+"
        remote(f"""set -eu
fixture={base}/package-fixture
mkdir -p "$fixture/DEBIAN" "$fixture/etc"
cat > "$fixture/DEBIAN/control" <<'CONTROL'
Package: {package}
Version: 1.0
Architecture: all
Maintainer: MMX tests <tests@example.invalid>
Description: Disposable MMX removal fixture
CONTROL
printf '/etc/mmx-removal-fixture.conf\\n' > "$fixture/DEBIAN/conffiles"
printf 'keep this configuration\\n' > "$fixture/etc/mmx-removal-fixture.conf"
dpkg-deb --build "$fixture" {base}/fixture.deb
# Another apt or dpkg, such as the distribution's daily update right after a
# guest starts, may hold dpkg's lock; dpkg itself does not wait for it.
timeout 180 sh -c 'until dpkg -i {base}/fixture.deb; do sleep 2; done'
""")
        project([{"apt_package": {'packages': [{'name': package}], 'state': "absent"}}], "package_removal")
        assert result(run("dryrun"))["change"] == "would_change"
        assert remote(f"dpkg-query -W -f='${{db:Status-Status}}' '{package}'") == "installed"
        assert result(run("run"))["change"] == "changed"
        assert remote(f"dpkg-query -W -f='${{db:Status-Status}}' '{package}'") == "config-files"
        assert remote("cat /etc/mmx-removal-fixture.conf") == "keep this configuration\n"
        assert result(run("run"))["change"] == "unchanged"
        assert result(run("dryrun"))["change"] == "unchanged"
        remote(f"dpkg --purge '{package}'")  # Fixture teardown, not MMX removal behavior.
        mark("real package removal previews, preserves conffiles, and becomes a no-op")

    project([{"shell": {"script": "exit 1"}}], "run_history_retention")
    failed = run("run", 1)
    project([{"show": "ready"}], "run_history_retention")
    successful = run("run")
    dry = run("dryrun")
    runs = workspace / ".mmx/runs"
    old = (datetime.now(timezone.utc) - timedelta(days=2)).isoformat().replace("+00:00", "Z")
    for record in [failed, successful, dry]:
        status = runs / record["id"] / "status.json"
        saved = json.loads(status.read_text())
        assert saved["finished"]
        saved["finished"] = old
        status.write_text(json.dumps(saved))
    project_file = workspace / "mmx_project_settings.yml"
    definition = json.loads(project_file.read_text())
    for retention in ["forever", "7d"]:
        definition["run_history_retention"] = retention
        project_file.write_text(json.dumps(definition))
        assert run("results", 0, failed["id"])["status"] == "failed"
        assert all((runs / record["id"]).exists() for record in [failed, successful, dry])
    mark("current run_history_retention setting controls existing records, including forever")

    del definition["run_history_retention"]
    project_file.write_text(json.dumps(definition))
    runners = workspace / ".mmx/runners"
    unused = runners / ("f" * 64)
    unused.write_text("unreferenced fixture runner")
    run("results")
    assert not (runs / failed["id"]).exists() and not (runs / dry["id"]).exists()
    assert (runs / successful["id"]).exists() and not unused.exists()
    assert (runners / successful["entries"][0]["runner_hash"]).exists()
    assert run("resume")["status"] == "nothing_to_resume"
    assert datetime.fromisoformat(run("results", 0, successful["id"])["finished"]) == datetime.fromisoformat(old)
    mark("default 24h cleanup removes expired history and unused cache while preserving latest normal resume selection")
