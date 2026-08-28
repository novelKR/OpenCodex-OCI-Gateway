#!/usr/bin/env python3

import json
import os
import pathlib
import shutil
import subprocess
import tempfile
import textwrap
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
EXPORTER = ROOT / "tools" / "export-public-core.sh"
PREPARER = ROOT / "tools" / "prepare-public-core.sh"
VALIDATOR = ROOT / "tools" / "validate-public-core.sh"
VERIFIER = ROOT / "tools" / "verify-public-core-remote.sh"
RELEASE_REF_VERIFIER = ROOT / "tools" / "verify-release-ref.sh"


def run(*arguments, cwd=None, env=None):
    return subprocess.run(
        [str(argument) for argument in arguments],
        cwd=cwd,
        env=env,
        check=False,
        capture_output=True,
        text=True,
    )


class PublicCorePublicationTests(unittest.TestCase):
    def commit(self, repository, message):
        result = run("git", "-C", repository, "add", "-A")
        self.assertEqual(result.returncode, 0, result.stderr)
        result = run(
            "git",
            "-C",
            repository,
            "-c",
            "user.name=Private Fixture",
            "-c",
            "user.email=private-fixture@example.invalid",
            "commit",
            "--quiet",
            "-m",
            message,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        return run("git", "-C", repository, "rev-parse", "HEAD").stdout.strip()

    def create_source(self, base, validator_status=0):
        source = base / "source"
        (source / "config").mkdir(parents=True)
        (source / "tools").mkdir()
        (source / "README.md").write_text("# Synthetic public Core\n", encoding="utf-8")
        for name in (
            "export-public-core.sh",
            "prepare-public-core.sh",
            "public-hygiene.sh",
            "public_hygiene.py",
        ):
            shutil.copy2(ROOT / "tools" / name, source / "tools" / name)
        validator = source / "tools" / "validate-public-core.sh"
        validator.write_text(
            "#!/usr/bin/env bash\n"
            "set -euo pipefail\n"
            f"exit {validator_status}\n",
            encoding="utf-8",
        )
        validator.chmod(0o755)
        (source / "config" / "public-export-allowlist.txt").write_text(
            "README.md\n"
            "config/public-export-allowlist.txt\n"
            "tools/\n",
            encoding="utf-8",
        )
        result = run("git", "init", "--quiet", "-b", "main", source)
        self.assertEqual(result.returncode, 0, result.stderr)
        return source, self.commit(source, "private source fixture")

    def prepare(self, source, commit, destination, environment=None):
        return run(
            source / "tools" / "prepare-public-core.sh",
            "--source-commit",
            commit,
            "--version",
            "v0.3.0",
            "--destination",
            destination,
            "--author-name",
            "Public Fixture",
            "--author-email",
            "public-fixture@example.invalid",
            env=environment,
        )

    def test_workflows_are_public_only_and_manually_dispatchable(self):
        ci = (ROOT / ".github" / "workflows" / "ci.yml").read_text(encoding="utf-8")
        codeql = (ROOT / ".github" / "workflows" / "codeql.yml").read_text(
            encoding="utf-8"
        )
        container = (
            ROOT / ".github" / "workflows" / "container-release.yml"
        ).read_text(encoding="utf-8")

        self.assertIn("workflow_dispatch:", ci)
        self.assertEqual(ci.count("if: github.event.repository.private == false"), 2)
        self.assertIn("workflow_dispatch:", codeql)
        self.assertEqual(
            codeql.count("if: github.event.repository.private == false"), 1
        )
        self.assertIn(
            "if: github.repository == 'novelKR/OpenCodex-OCI-Gateway' "
            "&& github.event.repository.private == false",
            container,
        )
        self.assertIn('tools/verify-release-ref.sh "$version"', container)

    def test_release_ref_verifier_binds_tag_workflow_and_checkout_commits(self):
        with tempfile.TemporaryDirectory() as directory:
            repository = pathlib.Path(directory) / "repository"
            repository.mkdir()
            result = run("git", "init", "--quiet", "-b", "main", repository)
            self.assertEqual(result.returncode, 0, result.stderr)
            (repository / "README.md").write_text("release fixture\n", encoding="utf-8")
            release_commit = self.commit(repository, "release fixture")
            result = run("git", "-C", repository, "tag", "v0.3.0")
            self.assertEqual(result.returncode, 0, result.stderr)
            environment = {
                **os.environ,
                "GITHUB_REF_TYPE": "tag",
                "GITHUB_REF_NAME": "v0.3.0",
                "GITHUB_SHA": release_commit,
            }

            result = run(
                RELEASE_REF_VERIFIER,
                "v0.3.0",
                cwd=repository,
                env=environment,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("release_ref=ok", result.stdout)

            for name, overrides, message in (
                (
                    "branch ref",
                    {"GITHUB_REF_TYPE": "branch"},
                    "must run from an exact tag ref",
                ),
                (
                    "tag name mismatch",
                    {"GITHUB_REF_NAME": "v0.3.1"},
                    "does not match the workflow tag ref",
                ),
                (
                    "workflow commit mismatch",
                    {"GITHUB_SHA": "0" * 40},
                    "workflow commit does not match",
                ),
            ):
                with self.subTest(name=name):
                    result = run(
                        RELEASE_REF_VERIFIER,
                        "v0.3.0",
                        cwd=repository,
                        env={**environment, **overrides},
                    )
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn(message, result.stderr)

            (repository / "README.md").write_text(
                "release fixture changed\n", encoding="utf-8"
            )
            newer_commit = self.commit(repository, "post-tag fixture")
            result = run(
                RELEASE_REF_VERIFIER,
                "v0.3.0",
                cwd=repository,
                env=environment,
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("checked out commit does not match", result.stderr)

            result = run("git", "-C", repository, "tag", "-d", "v0.3.0")
            self.assertEqual(result.returncode, 0, result.stderr)
            result = run(
                "git",
                "-C",
                repository,
                "-c",
                "user.name=Private Fixture",
                "-c",
                "user.email=private-fixture@example.invalid",
                "tag",
                "-a",
                "v0.3.0",
                newer_commit,
                "-m",
                "annotated fixture",
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            result = run(
                RELEASE_REF_VERIFIER,
                "v0.3.0",
                cwd=repository,
                env={**environment, "GITHUB_SHA": newer_commit},
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("must exist locally and be lightweight", result.stderr)

    def test_exporter_rejects_dirty_mismatched_and_inside_destinations(self):
        with tempfile.TemporaryDirectory() as directory:
            base = pathlib.Path(directory)
            source, commit = self.create_source(base)
            (source / "README.md").write_text("dirty\n", encoding="utf-8")
            destination = base / "dirty-export"
            result = run(source / "tools" / "export-public-core.sh", destination, commit)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("source worktree must be clean", result.stderr)
            self.assertFalse(destination.exists())

        with tempfile.TemporaryDirectory() as directory:
            base = pathlib.Path(directory)
            source, old_commit = self.create_source(base)
            (source / "README.md").write_text("# second source commit\n", encoding="utf-8")
            self.commit(source, "second source fixture")
            destination = base / "mismatched-export"
            result = run(
                source / "tools" / "export-public-core.sh",
                destination,
                old_commit,
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("current HEAD", result.stderr)
            self.assertFalse(destination.exists())

        with tempfile.TemporaryDirectory() as directory:
            base = pathlib.Path(directory)
            source, commit = self.create_source(base)
            destination = source / "nested-export"
            result = run(source / "tools" / "export-public-core.sh", destination, commit)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("outside the source checkout", result.stderr)
            self.assertFalse(destination.exists())

    def test_exporter_cleans_failed_hygiene_output(self):
        with tempfile.TemporaryDirectory() as directory:
            base = pathlib.Path(directory)
            source, _ = self.create_source(base)
            global_address = "8" + ".8.8.8"
            (source / "README.md").write_text(
                f"# unsafe fixture {global_address}\n", encoding="utf-8"
            )
            commit = self.commit(source, "unsafe source fixture")
            destination = base / "failed-export"

            result = run(source / "tools" / "export-public-core.sh", destination, commit)

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("global IPv4 address", result.stderr)
            self.assertFalse(destination.exists())

    def test_preparer_creates_one_parentless_commit_tag_and_exact_evidence(self):
        with tempfile.TemporaryDirectory() as directory:
            base = pathlib.Path(directory)
            source, commit = self.create_source(base)
            destination = base / "public-staging"

            result = self.prepare(source, commit, destination)

            self.assertEqual(result.returncode, 0, result.stderr)
            evidence_path = pathlib.Path(str(destination) + ".publication.json")
            evidence = json.loads(evidence_path.read_text(encoding="utf-8"))
            self.assertEqual(
                set(evidence),
                {
                    "schema_version",
                    "source_commit",
                    "source_tree",
                    "allowlist_blob",
                    "public_commit",
                    "public_tree",
                    "tag",
                    "exported_files",
                },
            )
            self.assertEqual(evidence["schema_version"], 1)
            self.assertEqual(evidence["source_commit"], commit)
            self.assertEqual(evidence["tag"], "v0.3.0")
            self.assertEqual(
                run("git", "-C", destination, "rev-list", "--count", "--all")
                .stdout.strip(),
                "1",
            )
            parent_line = run(
                "git", "-C", destination, "rev-list", "--parents", "-n", "1", "HEAD"
            ).stdout.split()
            self.assertEqual(len(parent_line), 1)
            self.assertEqual(
                run("git", "-C", destination, "cat-file", "-t", "refs/tags/v0.3.0")
                .stdout.strip(),
                "commit",
            )
            self.assertEqual(
                run("git", "-C", destination, "log", "-1", "--format=%s")
                .stdout.strip(),
                "Initial public Core v0.3.0",
            )
            self.assertEqual(
                run("git", "-C", destination, "log", "-1", "--format=%an <%ae>")
                .stdout.strip(),
                "Public Fixture <public-fixture@example.invalid>",
            )
            self.assertEqual(
                evidence["source_tree"],
                run("git", "-C", source, "rev-parse", f"{commit}^{{tree}}")
                .stdout.strip(),
            )
            self.assertEqual(
                evidence["allowlist_blob"],
                run(
                    "git",
                    "-C",
                    source,
                    "rev-parse",
                    f"{commit}:config/public-export-allowlist.txt",
                ).stdout.strip(),
            )
            self.assertEqual(
                evidence["public_commit"],
                run("git", "-C", destination, "rev-parse", "HEAD").stdout.strip(),
            )
            self.assertEqual(
                evidence["public_tree"],
                run("git", "-C", destination, "rev-parse", "HEAD^{tree}")
                .stdout.strip(),
            )
            files = run("git", "-C", destination, "ls-files").stdout.splitlines()
            self.assertEqual(evidence["exported_files"], len(files))
            self.assertEqual(
                run("git", "-C", destination, "status", "--porcelain").stdout, ""
            )

    def test_preparer_ignores_global_hooks_and_preserves_canonical_commit(self):
        with tempfile.TemporaryDirectory() as directory:
            base = pathlib.Path(directory)
            source, commit = self.create_source(base)
            hooks = base / "global-hooks"
            hooks.mkdir()
            marker = base / "hook-ran"
            pre_commit = hooks / "pre-commit"
            pre_commit.write_text(
                "#!/bin/sh\n"
                "printf 'mutated by global hook\\n' > README.md\n"
                "git add README.md\n"
                "printf 'ran\\n' > \"$HOOK_MARKER\"\n",
                encoding="utf-8",
            )
            pre_commit.chmod(0o755)
            commit_message = hooks / "commit-msg"
            commit_message.write_text(
                "#!/bin/sh\n"
                "printf 'Changed by global hook\\n' > \"$1\"\n",
                encoding="utf-8",
            )
            commit_message.chmod(0o755)
            global_config = base / "global.gitconfig"
            global_config.write_text(
                f"[core]\n\thooksPath = {hooks}\n", encoding="utf-8"
            )
            environment = os.environ.copy()
            environment["GIT_CONFIG_GLOBAL"] = str(global_config)
            environment["GIT_CONFIG_NOSYSTEM"] = "1"
            environment["HOOK_MARKER"] = str(marker)
            destination = base / "public-staging"

            result = self.prepare(
                source, commit, destination, environment=environment
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertFalse(marker.exists())
            self.assertEqual(
                run("git", "-C", destination, "log", "-1", "--format=%B")
                .stdout.strip(),
                "Initial public Core v0.3.0",
            )
            self.assertEqual(
                (destination / "README.md").read_text(encoding="utf-8"),
                "# Synthetic public Core\n",
            )

    def test_preparer_force_tracks_allowlisted_ignored_file(self):
        with tempfile.TemporaryDirectory() as directory:
            base = pathlib.Path(directory)
            source, _ = self.create_source(base)
            (source / ".gitignore").write_text("ignored.txt\n", encoding="utf-8")
            (source / "ignored.txt").write_text(
                "allowlisted and ignored\n", encoding="utf-8"
            )
            allowlist = source / "config" / "public-export-allowlist.txt"
            allowlist.write_text(
                allowlist.read_text(encoding="utf-8")
                + ".gitignore\n"
                + "ignored.txt\n",
                encoding="utf-8",
            )
            result = run("git", "-C", source, "add", ".gitignore", allowlist)
            self.assertEqual(result.returncode, 0, result.stderr)
            result = run("git", "-C", source, "add", "-f", "ignored.txt")
            self.assertEqual(result.returncode, 0, result.stderr)
            commit = self.commit(source, "add allowlisted ignored file")
            destination = base / "public-staging"

            result = self.prepare(source, commit, destination)

            self.assertEqual(result.returncode, 0, result.stderr)
            tracked = run("git", "-C", destination, "ls-files").stdout.splitlines()
            self.assertIn("ignored.txt", tracked)
            evidence = json.loads(
                pathlib.Path(str(destination) + ".publication.json").read_text(
                    encoding="utf-8"
                )
            )
            self.assertEqual(evidence["exported_files"], len(tracked))
            self.assertEqual(
                run(
                    "git",
                    "-C",
                    destination,
                    "status",
                    "--porcelain",
                    "--ignored=matching",
                ).stdout,
                "",
            )

    def test_preparer_rejects_invalid_metadata_and_cleans_validation_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            base = pathlib.Path(directory)
            source, commit = self.create_source(base)
            destination = base / "invalid-staging"
            result = run(
                source / "tools" / "prepare-public-core.sh",
                "--source-commit",
                commit,
                "--version",
                "0.3",
                "--destination",
                destination,
                "--author-name",
                "Public Fixture",
                "--author-email",
                "public-fixture@example.invalid",
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("--version must be vSEMVER", result.stderr)
            self.assertFalse(destination.exists())

        with tempfile.TemporaryDirectory() as directory:
            base = pathlib.Path(directory)
            source, commit = self.create_source(base, validator_status=23)
            destination = base / "failed-staging"

            result = self.prepare(source, commit, destination)

            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(destination.exists())
            self.assertFalse(pathlib.Path(str(destination) + ".publication.json").exists())
            self.assertEqual(list(base.glob(".public-core-prepare.*")), [])

    def test_validator_builds_only_in_disposable_copy(self):
        with tempfile.TemporaryDirectory() as directory:
            base = pathlib.Path(directory)
            public_root = base / "public-root"
            fake_bin = base / "fake-bin"
            public_root.mkdir()
            fake_bin.mkdir()
            shutil.copy2(VALIDATOR, public_root / "validate-public-core.sh")
            tools = public_root / "tools"
            tools.mkdir()
            shutil.move(
                str(public_root / "validate-public-core.sh"),
                str(tools / "validate-public-core.sh"),
            )
            hygiene = tools / "public-hygiene.sh"
            hygiene.write_text("#!/usr/bin/env bash\nexit 0\n", encoding="utf-8")
            hygiene.chmod(0o755)
            for relative in (
                "pilot/scripts/check.sh",
                "pilot/libexec/check",
                "ops/oci/check.sh",
                "client/relay/scripts/check.sh",
            ):
                path = public_root / relative
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("#!/usr/bin/env bash\nexit 0\n", encoding="utf-8")
            tests = public_root / "pilot" / "tests"
            tests.mkdir()
            (tests / "test_sample.py").write_text(
                "import unittest\n"
                "class Sample(unittest.TestCase):\n"
                "    def test_ok(self): self.assertTrue(True)\n",
                encoding="utf-8",
            )
            swift_root = (
                public_root / "client" / "relay" / "macos" / "OpenCodexRelay"
            )
            swift_root.mkdir(parents=True)
            log = base / "commands.log"
            for command in ("go", "swift", "jq"):
                executable = fake_bin / command
                executable.write_text(
                    "#!/usr/bin/env bash\n"
                    "set -euo pipefail\n"
                    f"printf '%s:%s\\n' '{command}' \"$PWD\" >> \"$VALIDATION_TEST_LOG\"\n"
                    f"touch .{command}-build-artifact\n",
                    encoding="utf-8",
                )
                executable.chmod(0o755)
            environment = os.environ.copy()
            environment["PATH"] = str(fake_bin) + os.pathsep + environment["PATH"]
            environment["VALIDATION_TEST_LOG"] = str(log)

            result = run(
                tools / "validate-public-core.sh",
                public_root,
                env=environment,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("public_validation=ok", result.stdout)
            self.assertEqual(len(log.read_text(encoding="utf-8").splitlines()), 5)
            self.assertEqual(list(public_root.rglob("*-build-artifact")), [])

            (public_root / ".gitignore").write_text(
                "ignored.txt\n", encoding="utf-8"
            )
            (public_root / "ignored.txt").write_text(
                "trailing whitespace \n", encoding="utf-8"
            )
            ignored_result = run(
                tools / "validate-public-core.sh",
                public_root,
                env=environment,
            )
            self.assertNotEqual(ignored_result.returncode, 0)
            self.assertIn(
                "trailing whitespace", ignored_result.stdout + ignored_result.stderr
            )
            self.assertEqual(len(log.read_text(encoding="utf-8").splitlines()), 5)

    def create_remote_fixture(self, base):
        source, commit = self.create_source(base)
        destination = base / "public-staging"
        result = self.prepare(source, commit, destination)
        self.assertEqual(result.returncode, 0, result.stderr)
        evidence = pathlib.Path(str(destination) + ".publication.json")
        remote = base / "remote.git"
        result = run("git", "clone", "--quiet", "--bare", destination, remote)
        self.assertEqual(result.returncode, 0, result.stderr)
        fake_bin = base / "fake-bin"
        fake_bin.mkdir()
        gh = fake_bin / "gh"
        gh.write_text(
            textwrap.dedent(
                """\
                #!/usr/bin/env bash
                set -euo pipefail
                case "$1:$2" in
                  repo:view)
                    printf '%s\\t%s\\t%s\\tfalse\\tfalse\\tmain\\n' \
                      "${FAKE_REPOSITORY_ID:-R_fixture}" "$3" "${FAKE_PRIVATE:-true}"
                    ;;
                  repo:clone)
                    exec git clone --mirror --quiet "$FAKE_REMOTE" "$4"
                    ;;
                  run:list)
                    case " $* " in
                      *" --all "*) ;;
                      *) exit 93 ;;
                    esac
                    if [[ "${FAKE_REQUIRE_WORKFLOW_DISPATCH:-false}" == true ]]; then
                      case " $* " in
                        *" --event workflow_dispatch "*) ;;
                        *) exit 94 ;;
                      esac
                    fi
                    limit=20
                    previous=""
                    for argument in "$@"; do
                      if [[ "$previous" == "--limit" ]]; then
                        limit="$argument"
                      fi
                      previous="$argument"
                    done
                    case "${FAKE_RUN_MODE:-empty}" in
                      empty) printf '%s\\n' '[]' ;;
                      skipped)
                        printf '%s\\n' '[{"databaseId":101,"status":"completed","conclusion":"skipped","workflowName":"fixture","event":"push"}]'
                        ;;
                      success)
                        printf '%s\\n' '[{"databaseId":101,"status":"completed","conclusion":"success","workflowName":"fixture","event":"workflow_dispatch"}]'
                        ;;
                      failure)
                        printf '%s\\n' '[{"databaseId":101,"status":"completed","conclusion":"failure","workflowName":"fixture","event":"workflow_dispatch"}]'
                        ;;
                      pending)
                        printf '%s\\n' '[{"databaseId":101,"status":"in_progress","conclusion":null,"workflowName":"fixture","event":"workflow_dispatch"}]'
                        ;;
                      failure_pending)
                        printf '%s\\n' '[{"databaseId":101,"status":"in_progress","conclusion":null,"workflowName":"fixture","event":"workflow_dispatch"},{"databaseId":102,"status":"completed","conclusion":"failure","workflowName":"fixture","event":"workflow_dispatch"}]'
                        ;;
                      many)
                        python3 -c 'import json, sys; limit = int(sys.argv[1]); print(json.dumps([{"databaseId": 1000 + index, "status": "completed", "conclusion": "skipped" if index < 24 else "success", "workflowName": "fixture", "event": "workflow_dispatch"} for index in range(25)][:limit]))' "$limit"
                        ;;
                      capped)
                        python3 -c 'import json, sys; limit = int(sys.argv[1]); print(json.dumps([{"databaseId": 2000 + index, "status": "completed", "conclusion": "skipped", "workflowName": "fixture", "event": "push"} for index in range(limit)]))' "$limit"
                        ;;
                      *) exit 91 ;;
                    esac
                    ;;
                  run:view)
                    case "${FAKE_RUN_MODE:-empty}" in
                      skipped)
                        printf '%s\\n' '{"jobs":[{"status":"completed","conclusion":"skipped"}]}'
                        ;;
                      success)
                        printf '%s\\n' '{"jobs":[{"status":"completed","conclusion":"success"}]}'
                        ;;
                      many)
                        if [[ "$3" == "1024" ]]; then
                          printf '%s\\n' '{"jobs":[{"status":"completed","conclusion":"success"}]}'
                        else
                          printf '%s\\n' '{"jobs":[{"status":"completed","conclusion":"skipped"}]}'
                        fi
                        ;;
                      *) exit 92 ;;
                    esac
                    ;;
                  *) exit 90 ;;
                esac
                """
            ),
            encoding="utf-8",
        )
        gh.chmod(0o755)
        sleep = fake_bin / "sleep"
        sleep.write_text("#!/bin/sh\nexec /bin/sleep 2\n", encoding="utf-8")
        sleep.chmod(0o755)
        environment = os.environ.copy()
        environment["PATH"] = str(fake_bin) + os.pathsep + environment["PATH"]
        environment["FAKE_REMOTE"] = str(remote)
        return evidence, remote, environment

    def verify_remote(self, evidence, environment, phase, repository_id="R_fixture"):
        return run(
            VERIFIER,
            "--phase",
            phase,
            "--repo",
            "example/PublicCore",
            "--repository-id",
            repository_id,
            "--evidence",
            evidence,
            "--timeout-seconds",
            "1",
            env=environment,
        )

    def test_remote_verifier_accepts_private_skips_and_public_success(self):
        with tempfile.TemporaryDirectory() as directory:
            evidence, _, environment = self.create_remote_fixture(
                pathlib.Path(directory)
            )
            for mode in ("empty", "skipped"):
                with self.subTest(private_mode=mode):
                    environment["FAKE_PRIVATE"] = "true"
                    environment["FAKE_RUN_MODE"] = mode
                    result = self.verify_remote(
                        evidence, environment, "private-staging"
                    )
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertIn("actions=skipped", result.stdout)

            environment["FAKE_PRIVATE"] = "false"
            environment["FAKE_RUN_MODE"] = "success"
            environment["FAKE_REQUIRE_WORKFLOW_DISPATCH"] = "true"
            result = self.verify_remote(evidence, environment, "public-ci")
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("ci=success codeql=success", result.stdout)

    def test_remote_verifier_distinguishes_public_failure_and_timeout(self):
        with tempfile.TemporaryDirectory() as directory:
            evidence, _, environment = self.create_remote_fixture(
                pathlib.Path(directory)
            )
            environment["FAKE_PRIVATE"] = "false"
            environment["FAKE_REQUIRE_WORKFLOW_DISPATCH"] = "true"
            environment["FAKE_RUN_MODE"] = "failure"

            result = self.verify_remote(evidence, environment, "public-ci")

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("completed without success", result.stderr)

            environment["FAKE_RUN_MODE"] = "failure_pending"
            result = self.verify_remote(evidence, environment, "public-ci")

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("completed without success", result.stderr)

            environment["FAKE_RUN_MODE"] = "empty"
            result = self.verify_remote(evidence, environment, "public-ci")

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("timed out", result.stderr)

    def test_remote_verifier_rejects_non_skipped_private_job_and_wrong_identity(self):
        with tempfile.TemporaryDirectory() as directory:
            evidence, _, environment = self.create_remote_fixture(
                pathlib.Path(directory)
            )
            environment["FAKE_PRIVATE"] = "true"
            environment["FAKE_RUN_MODE"] = "success"

            result = self.verify_remote(evidence, environment, "private-staging")

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("non-skipped job", result.stderr)

            result = self.verify_remote(
                evidence,
                environment,
                "private-staging",
                repository_id="R_wrong",
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("repository ID", result.stderr)

    def test_remote_verifier_checks_more_than_twenty_runs_and_rejects_cap(self):
        with tempfile.TemporaryDirectory() as directory:
            evidence, _, environment = self.create_remote_fixture(
                pathlib.Path(directory)
            )
            environment["FAKE_PRIVATE"] = "true"
            environment["FAKE_RUN_MODE"] = "many"

            result = self.verify_remote(evidence, environment, "private-staging")

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("non-skipped job", result.stderr)

            environment["FAKE_RUN_MODE"] = "capped"
            result = self.verify_remote(evidence, environment, "private-staging")

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("safety limit", result.stderr)

    def test_remote_verifier_rejects_annotated_publication_tag(self):
        with tempfile.TemporaryDirectory() as directory:
            base = pathlib.Path(directory)
            evidence, remote, environment = self.create_remote_fixture(base)
            commit = run(
                "git", "--git-dir", remote, "rev-parse", "refs/heads/main"
            ).stdout.strip()
            result = run(
                "git", "--git-dir", remote, "tag", "-d", "v0.3.0"
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            result = run(
                "git",
                "--git-dir",
                remote,
                "-c",
                "user.name=Fixture",
                "-c",
                "user.email=fixture@example.invalid",
                "tag",
                "-a",
                "v0.3.0",
                commit,
                "-m",
                "annotated replacement",
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            environment["FAKE_PRIVATE"] = "true"
            environment["FAKE_RUN_MODE"] = "empty"

            result = self.verify_remote(evidence, environment, "private-staging")

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("tag is not lightweight", result.stderr)

    def test_remote_verifier_rejects_additional_ref_and_tree_mismatch(self):
        with tempfile.TemporaryDirectory() as directory:
            base = pathlib.Path(directory)
            evidence, remote, environment = self.create_remote_fixture(base)
            environment["FAKE_PRIVATE"] = "true"
            environment["FAKE_RUN_MODE"] = "empty"
            commit = run("git", "--git-dir", remote, "rev-parse", "refs/heads/main").stdout.strip()
            result = run(
                "git",
                "--git-dir",
                remote,
                "update-ref",
                "refs/tags/unexpected",
                commit,
            )
            self.assertEqual(result.returncode, 0, result.stderr)

            result = self.verify_remote(evidence, environment, "private-staging")

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("unexpected", result.stderr)

        with tempfile.TemporaryDirectory() as directory:
            base = pathlib.Path(directory)
            evidence, _, environment = self.create_remote_fixture(base)
            document = json.loads(evidence.read_text(encoding="utf-8"))
            document["public_tree"] = "0" * 40
            evidence.write_text(
                json.dumps(document, indent=2, sort_keys=True) + "\n",
                encoding="utf-8",
            )
            environment["FAKE_PRIVATE"] = "true"
            environment["FAKE_RUN_MODE"] = "empty"

            result = self.verify_remote(evidence, environment, "private-staging")

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("tree does not match", result.stderr)


if __name__ == "__main__":
    unittest.main()
