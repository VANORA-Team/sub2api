"""Exercise upstream sync with real Git histories and an isolated registry stub."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


SCRIPTS = Path(__file__).resolve().parent
BASH = os.environ.get("BASH_EXECUTABLE") or shutil.which("bash")


class UpstreamSyncTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="upstream-sync-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.repo = self.root / "repo"
        self.repo.mkdir()
        self.output = self.root / "output"
        self.summary = self.root / "summary"
        self.report = self.root / "report"
        self.env = dict(os.environ)
        self.env.update(
            UPSTREAM_URL=self.repo.as_posix(),
            UPSTREAM_BRANCH="production",
            SYNC_REPORT_DIR=self.report.as_posix(),
            GITHUB_OUTPUT=self.output.as_posix(),
            GITHUB_STEP_SUMMARY=self.summary.as_posix(),
            HAS_UPSTREAM_SYNC_TOKEN="false",
            FORCE="false",
            GIT_TERMINAL_PROMPT="0",
            PYTHON=Path(sys.executable).as_posix(),
        )
        self.git("init", "-q", "-b", "main")
        self.git("config", "user.name", "Sync test")
        self.git("config", "user.email", "sync-test@example.invalid")
        self.git("config", "commit.gpgsign", "false")
        self.git("config", "core.autocrlf", "false")
        self.write("shared.txt", "base\n")
        self.commit("base")
        self.git("branch", "production")
        self.before = self.git("rev-parse", "HEAD")

    def git(self, *args):
        return subprocess.run(
            ["git", *args], cwd=self.repo, env=self.env,
            check=True, text=True, capture_output=True,
        ).stdout.strip()

    def write(self, name, text):
        path = self.repo / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")

    def commit(self, message):
        self.git("add", ".")
        self.git("commit", "-qm", message)

    def upstream_change(self, name="upstream.txt", text="upstream\n"):
        self.git("checkout", "-q", "production")
        self.write(name, text)
        self.commit("upstream")
        upstream = self.git("rev-parse", "HEAD")
        self.git("checkout", "-q", "main")
        return upstream

    def script(self, name, success=True, **env):
        result = subprocess.run(
            [BASH, (SCRIPTS / name).as_posix()],
            cwd=self.repo, env={**self.env, **env}, text=True, capture_output=True,
        )
        if success:
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        return result

    def outputs(self):
        return dict(line.split("=", 1) for line in self.output.read_text().splitlines())

    def test_merge_retains_both_histories_and_is_idempotent(self):
        upstream = self.upstream_change()
        self.write("fork.txt", "fork feature\n")
        self.commit("fork")
        before = self.git("rev-parse", "HEAD")
        self.script("sync-upstream.sh")
        merged = self.git("rev-parse", "HEAD")
        self.assertEqual(self.outputs(), {"commit": merged, "build": "true"})
        self.assertEqual(self.git("rev-parse", "HEAD^1"), before)
        self.assertEqual(self.git("rev-parse", "HEAD^2"), upstream)
        self.assertEqual((self.repo / "fork.txt").read_text(), "fork feature\n")
        self.assertEqual((self.repo / "upstream.txt").read_text(), "upstream\n")
        self.output.unlink()
        self.script("sync-upstream.sh")
        self.assertEqual(self.outputs(), {"commit": merged, "build": "false"})
        self.assertEqual(self.git("status", "--porcelain"), "")

    def test_fast_forward_candidate_still_checks_workflow_permission(self):
        self.upstream_change(".github/workflows/upstream.yml", "name: upstream\n")
        result = self.script("sync-upstream.sh", success=False)
        self.assertIn("UPSTREAM_SYNC_TOKEN", result.stdout)
        self.assertEqual(self.git("rev-parse", "HEAD"), self.before)
        self.assertFalse(self.output.exists())

    def test_authorized_workflow_update_keeps_upstream_history(self):
        upstream = self.upstream_change(".github/workflows/upstream.yml", "name: upstream\n")
        self.script("sync-upstream.sh", HAS_UPSTREAM_SYNC_TOKEN="true")
        self.assertEqual(self.git("rev-parse", "HEAD^2"), upstream)
        self.assertEqual(self.outputs()["build"], "true")
        self.assertIn("name: upstream", (self.repo / ".github/workflows/upstream.yml").read_text())

    def test_source_conflict_leaves_head_unchanged_and_saves_patch(self):
        self.upstream_change("shared.txt", "upstream behavior\n")
        self.write("shared.txt", "fork behavior\n")
        self.commit("fork")
        before = self.git("rev-parse", "HEAD")
        self.script("sync-upstream.sh", success=False)
        self.assertEqual(self.git("rev-parse", "HEAD"), before)
        self.assertEqual((self.report / "conflicts.txt").read_text().strip(), "shared.txt")
        patch = (self.report / "conflicts.patch").read_text()
        self.assertIn("upstream behavior", patch)
        self.assertIn("fork behavior", patch)
        self.assertIn("shared.txt", self.summary.read_text())
        self.assertFalse(self.output.exists())

    def test_no_updates_outputs_current_commit_without_build(self):
        self.script("sync-upstream.sh")
        self.assertEqual(self.outputs(), {"commit": self.before, "build": "false"})

    def test_force_revalidates_current_commit(self):
        self.script("sync-upstream.sh", FORCE="true")
        self.assertEqual(self.outputs(), {"commit": self.before, "build": "true"})
        self.assertEqual(self.git("rev-parse", "HEAD"), self.before)

    def test_dirty_checkout_is_rejected(self):
        self.write("shared.txt", "local work\n")
        self.script("sync-upstream.sh", success=False)
        self.assertEqual((self.repo / "shared.txt").read_text(), "local work\n")
        self.assertFalse(self.output.exists())

    def registry(self, platforms=(), error=""):
        bin_dir = self.root / "bin"
        bin_dir.mkdir()
        docker = bin_dir / "docker"
        docker.write_text(
            '#!/usr/bin/env bash\n'
            'if [[ -n "$REGISTRY_ERROR" ]]; then\n'
            '  echo "$REGISTRY_ERROR" >&2\n'
            '  exit 1\n'
            'fi\n'
            'cat "$REGISTRY_MANIFEST"\n',
            encoding="utf-8",
        )
        docker.chmod(0o755)
        manifest = self.root / "manifest.json"
        manifest.write_text(json.dumps({
            "manifests": [
                {"platform": {"os": "linux", "architecture": arch}}
                for arch in platforms
            ],
        }), encoding="utf-8")
        self.env.update(
            PATH=str(bin_dir) + os.pathsep + self.env["PATH"],
            IMAGE="ghcr.io/example/sub2api:sha-test",
            REGISTRY_ERROR=error,
            REGISTRY_MANIFEST=manifest.as_posix(),
        )

    def test_existing_multiarch_image_is_reused(self):
        self.registry(("amd64", "arm64", "unknown"))
        self.script("check-fork-image.sh")
        self.assertEqual(self.outputs()["build"], "false")

    def test_missing_image_is_retried_without_new_upstream_commits(self):
        self.script("sync-upstream.sh")
        self.assertEqual(self.outputs()["build"], "false")
        self.registry(error="ERROR: manifest unknown")
        self.script("check-fork-image.sh")
        self.assertEqual(self.outputs()["build"], "true")

    def test_incomplete_image_is_rebuilt(self):
        self.registry(("amd64",))
        self.script("check-fork-image.sh")
        self.assertEqual(self.outputs()["build"], "true")

    def test_registry_access_failure_is_reported(self):
        self.registry(error="ERROR: unauthorized: access denied")
        self.script("check-fork-image.sh", success=False)
        self.assertFalse(self.output.exists())

    def test_force_rebuilds_existing_image(self):
        self.registry(("amd64", "arm64"))
        self.script("check-fork-image.sh", FORCE="true")
        self.assertEqual(self.outputs()["build"], "true")


if __name__ == "__main__":
    unittest.main(verbosity=2)
