import importlib.machinery
import importlib.util
import json
import subprocess
import tempfile
import unittest
from pathlib import Path

loader = importlib.machinery.SourceFileLoader("factory_run", str(Path(__file__).with_name("run")))
spec = importlib.util.spec_from_loader(loader.name, loader)
factory = importlib.util.module_from_spec(spec)
loader.exec_module(factory)

TOOL_CONFIG = {
    "changelog": {"kind": "markdown-h2", "url": "https://example.test/CHANGELOG.md"},
    "keywords": ["hook"],
    "surface": [{"name": "hook events", "risk": "status", "fingerprint": [r"^hook_event\s"]}],
}

CHANGELOG = """# Changelog

## [Unreleased]

- Unreleased hook change

## [1.2.0] - 2026-10-21

### Added

- Added a PostToolBatch hook event
  that spans two lines
- Faster startup

### Fixed

- Fixed a crash

## [1.1.0] - 2026-10-10

- Old entry below the floor
"""


class DataDirTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.saved = (factory.DATA, factory.fetch, dict(factory.FINGERPRINTERS), factory.run)
        factory.DATA = Path(self.tmp.name)

    def tearDown(self):
        factory.DATA, factory.fetch, fingerprinters, factory.run = self.saved
        factory.FINGERPRINTERS.clear()
        factory.FINGERPRINTERS.update(fingerprinters)
        self.tmp.cleanup()


class MarkdownBulletTests(unittest.TestCase):
    def test_should_join_continuation_lines_and_track_sections_when_bullets_wrap(self):
        bullets = list(factory.markdown_bullets("### Added\n\n- one\n  two\n- three\n\n## Fixed\n- four\n"))
        self.assertEqual(bullets, [("Added", "one two"), ("Added", "three"), ("Fixed", "four")])

    def test_should_ignore_prose_when_it_is_not_a_bullet(self):
        self.assertEqual(list(factory.markdown_bullets("Release 0.165.0-alpha.1\n\n")), [])


class ChangelogTests(DataDirTest):
    def test_should_skip_unreleased_and_parse_bracketed_versions_when_reading_h2_changelog(self):
        factory.fetch = lambda url: CHANGELOG
        versions = [v for v, _ in factory.changelog_markdown_h2(TOOL_CONFIG["changelog"])]
        self.assertEqual(versions, ["1.2.0", "1.1.0"])

    def test_should_record_only_versions_at_or_above_floor_when_sweeping(self):
        factory.fetch = lambda url: CHANGELOG
        records = factory.sweep_changelog("pi", TOOL_CONFIG, "1.2.0", "1.1.0", set())
        self.assertEqual({r["version"] for r in records}, {"1.2.0"})
        self.assertEqual(len(records), 3)
        self.assertTrue(all(r["ahead"] for r in records))

    def test_should_flag_watch_when_a_keyword_appears_in_any_case(self):
        factory.fetch = lambda url: CHANGELOG.replace("hook", "HOOK")
        records = factory.sweep_changelog("pi", TOOL_CONFIG, "1.2.0", "1.2.0", set())
        watched = [r["text"] for r in records if r["watch"]]
        self.assertEqual(watched, ["Added a PostToolBatch HOOK event that spans two lines"])
        self.assertFalse(any(r["ahead"] for r in records))

    def test_should_add_nothing_when_the_same_changelog_is_swept_twice(self):
        factory.fetch = lambda url: CHANGELOG
        known = set()
        first = factory.sweep_changelog("pi", TOOL_CONFIG, "1.0.0", "1.2.0", known)
        second = factory.sweep_changelog("pi", TOOL_CONFIG, "1.0.0", "1.2.0", known)
        self.assertEqual(len(first), 4)
        self.assertEqual(second, [])

    def test_should_skip_prereleases_and_stop_paging_when_releases_pass_the_floor(self):
        pages = {
            1: [{"tag_name": "rust-v0.165.0-alpha.1", "prerelease": True, "body": "- alpha"},
                {"tag_name": "rust-v0.164.0", "prerelease": False, "body": "## New Features\n- feature"}],
            2: [{"tag_name": "rust-v0.162.0", "prerelease": False, "body": "- older"}],
            3: [{"tag_name": "rust-v0.100.0", "prerelease": False, "body": "- never fetched"}],
        }
        requested = []

        def fake_fetch(url):
            page = int(url.rsplit("page=", 1)[1])
            requested.append(page)
            return json.dumps(pages.get(page, []))

        factory.fetch = fake_fetch
        changelog = {"url": "https://api.example.test/releases", "tag_prefix": "rust-v"}
        entries = factory.changelog_github_releases(changelog, factory.version_key("0.163.0"))
        self.assertEqual([v for v, _ in entries], ["0.164.0", "0.162.0"])
        self.assertEqual(requested, [1, 2])


class FingerprintTests(DataDirTest):
    def fake(self, version, lines):
        factory.FINGERPRINTERS["pi"] = lambda: (version, lines)

    def test_should_record_nothing_when_there_is_no_previous_fingerprint(self):
        self.fake("1.0.0", ["hook_event Stop", "cli_flag --x"])
        version, records = factory.sweep_fingerprint("pi", TOOL_CONFIG, set())
        self.assertEqual((version, records), ("1.0.0", []))

    def test_should_record_only_watched_lines_with_their_sign_when_the_fingerprint_changes(self):
        self.fake("1.0.0", ["hook_event Stop", "hook_event Gone", "cli_flag --x"])
        factory.sweep_fingerprint("pi", TOOL_CONFIG, set())
        self.fake("1.1.0", ["hook_event Stop", "hook_event New", "cli_flag --y"])
        _, records = factory.sweep_fingerprint("pi", TOOL_CONFIG, set())
        self.assertEqual(sorted(r["text"] for r in records), ["+ hook_event New", "- hook_event Gone"])
        self.assertEqual(records[0]["section"], "1.0.0->1.1.0")
        self.assertEqual(records[0]["surfaces"], ["hook events"])

    def test_should_fail_loudly_when_the_tool_reports_no_version(self):
        self.fake(None, [])
        with self.assertRaises(RuntimeError):
            factory.sweep_fingerprint("pi", TOOL_CONFIG, set())


class ClassifyTests(DataDirTest):
    def records(self):
        return [factory.new_record(f"pi:cl:1.0.0:{n}", "pi", "changelog", "1.0.0", "", f"text {n}", False)
                for n in ("a", "b", "c")]

    def answer(self, items):
        factory.run = lambda *argv, **kwargs: json.dumps({"is_error": False, "structured_output": {"items": items}})

    def test_should_close_depends_and_ignore_but_keep_threats_open_when_classified(self):
        records = self.records()
        self.answer([{"id": "pi:cl:1.0.0:a", "class": "threat", "note": "n"},
                     {"id": "pi:cl:1.0.0:b", "class": "ignore", "note": ""},
                     {"id": "pi:cl:1.0.0:c", "class": "depends", "note": ""}])
        self.assertEqual(factory.classify(records), 3)
        self.assertEqual([(r["class"], r["status"]) for r in records],
                         [("threat", "open"), ("ignore", "done"), ("depends", "done")])

    def test_should_leave_records_unclassified_when_the_model_returns_unknown_ids_or_classes(self):
        records = self.records()
        self.answer([{"id": "pi:cl:1.0.0:zzz", "class": "threat", "note": ""},
                     {"id": "pi:cl:1.0.0:a", "class": "urgent", "note": ""}])
        self.assertEqual(factory.classify(records), 0)
        self.assertTrue(all(r["class"] is None for r in records))

    def test_should_not_reclassify_a_record_a_human_already_marked(self):
        records = self.records()
        records[0].update({"class": "ignore", "by": "human"})
        self.answer([{"id": "pi:cl:1.0.0:a", "class": "threat", "note": ""}])
        factory.classify(records)
        self.assertEqual(records[0]["class"], "ignore")

    def test_should_raise_when_claude_returns_no_json(self):
        factory.run = lambda *argv, **kwargs: ""
        with self.assertRaisesRegex(RuntimeError, "no JSON"):
            factory.classify(self.records())


class SweepTests(DataDirTest):
    def setUp(self):
        super().setUp()
        self.saved_surface = factory.SURFACE
        surface = Path(self.tmp.name) / "surface.toml"
        surface.write_text('[tools.pi]\nchangelog = { kind = "markdown-h2", url = "https://example.test" }\n'
                           'keywords = ["hook"]\n[[tools.pi.surface]]\nname = "hook events"\nrisk = "status"\n'
                           "fingerprint = ['^hook_event\\s']\n")
        factory.SURFACE = surface
        factory.DATA = Path(self.tmp.name) / "data"
        factory.fetch = lambda url: CHANGELOG
        factory.FINGERPRINTERS["pi"] = lambda: ("1.1.0", ["hook_event Stop"])

    def tearDown(self):
        factory.SURFACE = self.saved_surface
        super().tearDown()

    def log(self):
        result = subprocess.run(["git", "-C", str(factory.DATA), "log", "--format=%s"], capture_output=True, text=True)
        return result.stdout.splitlines()

    def test_should_commit_once_and_then_stay_clean_when_swept_twice_with_no_upstream_change(self):
        self.assertEqual(factory.cmd_sweep(["--no-classify"]), 0)
        self.assertEqual(factory.cmd_sweep(["--no-classify"]), 0)
        log = self.log()
        self.assertEqual(len(log), 1)
        self.assertIn("pi 1.1.0 | +4", log[0])

    def test_should_catch_up_every_missed_version_in_one_sweep_when_days_were_skipped(self):
        factory.cmd_sweep(["--no-classify"])
        factory.fetch = lambda url: CHANGELOG.replace("## [Unreleased]", "## [1.4.0] - x\n\n- four\n\n## [1.3.0] - x\n\n- three")
        factory.FINGERPRINTERS["pi"] = lambda: ("1.4.0", ["hook_event Stop", "hook_event Added"])
        factory.cmd_sweep(["--no-classify"])
        records = factory.load_records()
        self.assertEqual({r["version"] for r in records}, {"1.1.0", "1.2.0", "1.3.0", "1.4.0"})
        self.assertIn("pi 1.1.0->1.4.0", self.log()[0])

    def test_should_keep_other_tools_and_report_failure_when_one_fetch_fails(self):
        def broken(url):
            raise OSError("504")
        factory.fetch = broken
        self.assertEqual(factory.cmd_sweep(["--no-classify"]), 1)
        state = factory.load_state()
        self.assertEqual(state["tools"]["pi"]["installed"], "1.1.0")
        self.assertIn("pi changelog: 504", state["errors"])

    def test_should_refuse_an_ambiguous_prefix_when_marking(self):
        factory.cmd_sweep(["--no-classify"])
        with self.assertRaises(SystemExit):
            factory.cmd_mark(["pi:cl:", "done"])


if __name__ == "__main__":
    unittest.main()
