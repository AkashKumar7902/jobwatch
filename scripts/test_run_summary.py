import tempfile
import unittest
from pathlib import Path

from scripts.run_summary import MAX_DURATION_MS, MAX_FILE_BYTES, MAX_ISSUES, MAX_LINES, parse_log, render


def board(index: int, status: str = "ok", new: int = 0, **overrides) -> str:
    metrics = dict(
        open=1, matched=0, deferred=0, detail_failed=0, retries=0, caps=0,
        fetch_ms=1, process_ms=2,
    )
    if status == "recovered":
        metrics["retries"] = 1
    elif status == "capped":
        metrics["caps"] = 1
    elif status == "degraded":
        metrics["deferred"] = 1
    elif status == "failed":
        metrics["open"] = 0
    metrics.update(overrides)
    return (
        f'BOARD index={index} adapter=custom company="Board {index}" status={status} '
        f'open={metrics["open"]} new={new} matched={metrics["matched"]} deferred={metrics["deferred"]} '
        f'detail_failed={metrics["detail_failed"]} retries={metrics["retries"]} caps={metrics["caps"]} '
        f'fetch_ms={metrics["fetch_ms"]} process_ms={metrics["process_ms"]}'
    )


def prefixed(line: str) -> str:
    return "jobwatch 2026/08/11 01:02:03 " + line


def fetch(index: int, status: str = "ok", open_jobs: int = 1, duration: int = 1) -> str:
    return prefixed(f"FETCH index={index} status={status} open={open_jobs} duration_ms={duration}")


def board_records(index: int, status: str = "ok", new: int = 0, **overrides) -> list[str]:
    board_line = board(index, status, new, **overrides)
    if status == "failed":
        fetch_line = fetch(index, "failed", 0, overrides.get("fetch_ms", 1))
    elif status == "partial":
        fetch_line = fetch(index, "partial", overrides.get("open", 1), overrides.get("fetch_ms", 1))
    else:
        fetch_line = fetch(index, "ok", overrides.get("open", 1), overrides.get("fetch_ms", 1))
    records = [fetch_line, prefixed(board_line)]
    warning = board_warning(index, status)
    if warning:
        records.append(warning)
    return records


def board_warning(index: int, status: str) -> str | None:
    if status == "failed":
        return prefixed(f"WARN scope=board index={index} step=fetch code=duplicate count=1")
    if status == "partial":
        return prefixed(f"WARN scope=board index={index} step=fetch code=contract count=1")
    if status == "degraded":
        return prefixed(f"WARN scope=board index={index} step=process code=match count=1")
    return None


class RunSummaryTest(unittest.TestCase):
    def parse(self, lines):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "run.log"
            path.write_text("\n".join(lines), encoding="utf-8")
            return parse_log(path)

    def test_missing_log_and_terminal(self):
        boards, warnings, matches, terminal = parse_log(Path("/definitely/missing/run.log"))
        text = render(boards, warnings, matches, terminal, "failure", "", "skipped", "skipped", "skipped", "", "")
        self.assertIn("No valid RUN terminal", text)
        self.assertIn("remote state not published", text)

    def test_rejects_malformed_and_does_not_copy_it(self):
        boards, warnings, matches, terminal = self.parse([
            'BOARD index=1 adapter=custom company="safe" status=failed open=1',
            'MATCH company="SECRET"',
            'RUN status=ok local_state=saved code=none duration_ms=1 boards=1 trailing=SECRET',
        ])
        text = render(boards, warnings, matches, terminal, "success", "restored", "success", "success", "failure", "", "")
        self.assertNotIn("SECRET", text)
        self.assertIn("remote state unverified", text)

    def test_issue_limits_zero_one_twenty_four_twenty_five(self):
        for count in (0, 1, MAX_ISSUES, MAX_ISSUES + 1):
            lines = []
            statuses = ("failed", "partial", "degraded", "capped", "recovered")
            for index in range(1, count + 1):
                lines.extend(board_records(index, statuses[(index - 1) % len(statuses)]))
            run_status = "degraded" if count else "ok"
            lines.append(prefixed(f"RUN status={run_status} local_state=saved code=none duration_ms=9 boards={count}"))
            boards, warnings, matches, terminal = self.parse(lines)
            text = render(boards, warnings, matches, terminal, "success", "restored", "success", "success", "success", "true", "a" * 40)
            rendered = sum(text.count(f"- {status}:") for status in statuses)
            self.assertEqual(rendered, min(count, MAX_ISSUES))
            self.assertEqual("more issue(s) omitted" in text, count > MAX_ISSUES)

    def test_mixed_priority_and_duplicate_warnings_do_not_hide_failed_board(self):
        lines = board_records(1, "failed")
        for index in range(2, 27):
            lines.extend(board_records(index, "capped"))
        lines.extend(prefixed("WARN scope=run index=0 step=report code=no_reporter count=1") for _ in range(30))
        lines.append(prefixed("RUN status=degraded local_state=saved code=none duration_ms=9 boards=26"))
        boards, warnings, matches, terminal = self.parse(lines)
        text = render(boards, warnings, matches, terminal, "success", "restored", "success", "success", "success", "false", "d" * 40)
        self.assertIn("- failed: Board 1", text)
        self.assertEqual(text.count("run: report/no_reporter"), 1)
        self.assertIn("count=30", text)
        self.assertEqual(text.count("\n- "), MAX_ISSUES + 1)  # issue rows plus one omitted row

    def test_publish_outcomes_require_verified_sha(self):
        cases = [
            ("success", "true", "a" * 40, "verified `"),
            ("success", "", "", "remote state unverified"),
            ("failure", "true", "a" * 40, "remote state unverified"),
            ("cancelled", "true", "a" * 40, "remote state unverified"),
            ("skipped", "", "", "remote state not published"),
        ]
        for outcome, changed, sha, expected in cases:
            with self.subTest(outcome=outcome, sha=bool(sha)):
                text = render([], [], [], None, "success", "restored", "success", "success", outcome, changed, sha)
                self.assertIn(expected, text)

    def test_parses_prefixed_safe_board_and_warning(self):
        boards, warnings, matches, terminal = self.parse([
            fetch(1, open_jobs=2), prefixed(board(1, "degraded", 2, open=2)),
            'jobwatch 2026/08/11 01:02:03 WARN scope=board index=1 step=process code=match count=1',
            'jobwatch 2026/08/11 01:02:03 RUN status=degraded local_state=saved code=none duration_ms=3 boards=1',
        ])
        self.assertEqual((len(boards), len(warnings), terminal.status), (1, 1, "degraded"))
        text = render(boards, warnings, matches, terminal, "success", "restored", "success", "success", "success", "false", "b" * 40)
        self.assertIn("process/match", text)
        self.assertIn("Board 1 (custom): 2", text)

    def test_match_summary_uses_only_board_aggregates(self):
        boards, warnings, matches, terminal = self.parse([
            *board_records(1, "ok", open=3, matched=2),
            prefixed('RUN status=ok local_state=saved code=none duration_ms=3 boards=1'),
        ])
        text = render(boards, warnings, matches, terminal, "success", "restored", "success", "success", "success", "false", "c" * 40)
        self.assertEqual([(record.company, record.adapter, record.count) for record in matches], [("Board 1", "custom", 2)])
        self.assertIn("Board 1 (custom): 2", text)
        self.assertIn("titles and locations are intentionally omitted", text)

    def test_match_summary_escapes_board_name_and_bounds_rows(self):
        lines = []
        for index in range(1, 27):
            records = board_records(index, "ok", matched=1)
            records[1] = records[1].replace(f'company="Board {index}"', f'company="&lt;[*] {index}"')
            lines.extend(records)
        lines.append(prefixed('RUN status=ok local_state=saved code=none duration_ms=3 boards=1'))
        lines[-1] = prefixed('RUN status=ok local_state=saved code=none duration_ms=3 boards=26')
        boards, warnings, matches, terminal = self.parse(lines)
        text = render(boards, warnings, matches, terminal, "success", "restored", "success", "success", "success", "false", "c" * 40)
        self.assertEqual(len(matches), 26)
        self.assertIn("2 more board(s) with matches omitted", text)
        self.assertNotIn("<[*]>", text)

    def test_rejects_duplicate_contradictory_and_post_terminal_records(self):
        cases = [
            [fetch(1), prefixed(board(1)), prefixed(board(1)), prefixed('RUN status=ok local_state=saved code=none duration_ms=1 boards=1')],
            [fetch(1), fetch(1), prefixed(board(1)), prefixed('RUN status=ok local_state=saved code=none duration_ms=1 boards=1')],
            [fetch(1, open_jobs=2), prefixed(board(1)), prefixed('RUN status=ok local_state=saved code=none duration_ms=1 boards=1')],
            [*board_records(1, "failed"), prefixed('RUN status=ok local_state=saved code=none duration_ms=1 boards=1')],
            [*board_records(1), prefixed('RUN status=ok local_state=saved code=none duration_ms=1 boards=1'), 'SECRET after terminal'],
        ]
        for lines in cases:
            with self.subTest(lines=lines):
                boards, warnings, matches, terminal = self.parse(lines)
                self.assertEqual((boards, warnings, matches, terminal), ([], [], [], None))

    def test_rejects_impossible_board_metrics(self):
        cases = [
            [fetch(1), prefixed(board(1, new=2)), prefixed('RUN status=ok local_state=saved code=none duration_ms=1 boards=1')],
            [
                fetch(1), prefixed(board(1, matched=2)),
                prefixed('RUN status=ok local_state=saved code=none duration_ms=1 boards=1'),
            ],
            [
                fetch(1), prefixed(board(1, "degraded", deferred=2)),
                prefixed('WARN scope=board index=1 step=process code=match count=1'),
                prefixed('RUN status=degraded local_state=saved code=none duration_ms=1 boards=1'),
            ],
            [
                fetch(1), prefixed(board(1, "degraded", deferred=0, detail_failed=2)),
                prefixed('WARN scope=board index=1 step=process code=detail count=1'),
                prefixed('RUN status=degraded local_state=saved code=none duration_ms=1 boards=1'),
            ],
            [
                fetch(1, open_jobs=2),
                prefixed(board(1, "degraded", open=2, matched=1, deferred=1, detail_failed=1)),
                prefixed('WARN scope=board index=1 step=process code=detail_and_match count=1'),
                prefixed('RUN status=degraded local_state=saved code=none duration_ms=1 boards=1'),
            ],
            [
                fetch(1), prefixed(board(1, "capped", caps=2)),
                prefixed('RUN status=degraded local_state=saved code=none duration_ms=1 boards=1'),
            ],
        ]
        for lines in cases:
            with self.subTest(board=lines[1]):
                self.assertEqual(self.parse(lines), ([], [], [], None))

    def test_duration_bounds_roundtrip_and_reject_overflow(self):
        valid = [
            *board_records(1, fetch_ms=MAX_DURATION_MS, process_ms=MAX_DURATION_MS),
            prefixed(
                f'RUN status=ok local_state=saved code=none duration_ms={MAX_DURATION_MS} boards=1'
            ),
        ]
        boards, warnings, matches, terminal = self.parse(valid)
        self.assertEqual(
            (boards[0].fetch_ms, boards[0].process_ms, terminal.duration_ms),
            (MAX_DURATION_MS, MAX_DURATION_MS, MAX_DURATION_MS),
        )

        too_large = MAX_DURATION_MS + 1
        invalid = [
            [
                fetch(1, duration=too_large), prefixed(board(1, fetch_ms=too_large)),
                prefixed('RUN status=ok local_state=saved code=none duration_ms=1 boards=1'),
            ],
            [
                fetch(1), prefixed(board(1, process_ms=too_large)),
                prefixed('RUN status=ok local_state=saved code=none duration_ms=1 boards=1'),
            ],
            [
                *board_records(1),
                prefixed(f'RUN status=ok local_state=saved code=none duration_ms={too_large} boards=1'),
            ],
        ]
        for lines in invalid:
            with self.subTest(lines=lines):
                self.assertEqual(self.parse(lines), ([], [], [], None))

    def test_board_and_terminal_warnings_are_single_events(self):
        cases = [
            [
                fetch(1, "failed", 0), prefixed(board(1, "failed")),
                prefixed('WARN scope=board index=1 step=fetch code=duplicate count=2'),
                prefixed('RUN status=degraded local_state=saved code=none duration_ms=1 boards=1'),
            ],
            [
                fetch(1, "failed", 0), prefixed(board(1, "failed")),
                prefixed('WARN scope=board index=1 step=fetch code=duplicate count=1'),
                prefixed('WARN scope=board index=1 step=fetch code=duplicate count=1'),
                prefixed('RUN status=degraded local_state=saved code=none duration_ms=1 boards=1'),
            ],
            [
                prefixed('WARN scope=run index=0 step=terminal code=cancelled count=2'),
                prefixed('RUN status=cancelled local_state=saved code=cancelled duration_ms=1 boards=0'),
            ],
            [
                prefixed('WARN scope=run index=0 step=terminal code=cancelled count=1'),
                prefixed('WARN scope=run index=0 step=terminal code=cancelled count=1'),
                prefixed('RUN status=cancelled local_state=saved code=cancelled duration_ms=1 boards=0'),
            ],
        ]
        for lines in cases:
            with self.subTest(lines=lines):
                self.assertEqual(self.parse(lines), ([], [], [], None))

    def test_cancelled_partially_processed_board_roundtrips_all_fetch_outcomes(self):
        for fetch_status, open_jobs in (("ok", 2), ("partial", 2), ("failed", 0)):
            matched = 1 if open_jobs else 0
            new = 1 if open_jobs else 0
            lines = [fetch(1, fetch_status, open_jobs)]
            lines.extend([
                prefixed(board(1, "failed", new, open=open_jobs, matched=matched)),
                prefixed('WARN scope=board index=1 step=process code=cancelled count=1'),
                prefixed('WARN scope=run index=0 step=terminal code=cancelled count=1'),
                prefixed('RUN status=cancelled local_state=saved code=cancelled duration_ms=3 boards=1'),
            ])
            with self.subTest(fetch_status=fetch_status):
                boards, warnings, matches, terminal = self.parse(lines)
                self.assertEqual((len(boards), len(matches), terminal.status, terminal.code), (1, matched, "cancelled", "cancelled"))
                self.assertEqual(boards[0].status, "failed")

        not_run = [
            fetch(1, "partial", 2),
            prefixed(board(1, "failed", open=2)),
            prefixed('WARN scope=board index=1 step=process code=not_run count=1'),
            prefixed('WARN scope=run index=0 step=terminal code=persistence count=1'),
            prefixed('RUN status=failed local_state=checkpointed code=persistence duration_ms=3 boards=1'),
        ]
        boards, warnings, matches, terminal = self.parse(not_run)
        self.assertEqual((boards[0].status, terminal.status, terminal.code), ("failed", "failed", "persistence"))

    def test_cancelled_terminal_requires_cancelled_code_and_warning(self):
        cases = [
            [
                prefixed('WARN scope=run index=0 step=terminal code=timeout count=1'),
                prefixed('RUN status=cancelled local_state=saved code=timeout duration_ms=1 boards=0'),
            ],
            [prefixed('RUN status=cancelled local_state=saved code=cancelled duration_ms=1 boards=0')],
            [
                prefixed('WARN scope=run index=0 step=terminal code=timeout count=1'),
                prefixed('RUN status=cancelled local_state=saved code=cancelled duration_ms=1 boards=0'),
            ],
            [
                prefixed('WARN scope=run index=0 step=terminal code=cancelled count=1'),
                prefixed('RUN status=failed local_state=saved code=cancelled duration_ms=1 boards=0'),
            ],
        ]
        for lines in cases:
            with self.subTest(lines=lines):
                self.assertEqual(self.parse(lines), ([], [], [], None))

    def test_terminal_codes_and_successful_local_state_match_producer(self):
        producer_codes = {"persistence", "notify", "report", "match", "seed", "fetch", "unknown"}
        for code in producer_codes:
            lines = [
                prefixed(f'WARN scope=run index=0 step=terminal code={code} count=1'),
                prefixed(f'RUN status=failed local_state=saved code={code} duration_ms=1 boards=0'),
            ]
            with self.subTest(code=code):
                _, _, _, terminal = self.parse(lines)
                self.assertIsNotNone(terminal)
                self.assertEqual((terminal.status, terminal.code), ("failed", code))

        for code in {
            "timeout", "transport", "forbidden", "unauthorized", "not_found", "rate_limited",
            "server", "duplicate", "contract", "invalid_response",
        }:
            lines = [
                prefixed(f'WARN scope=run index=0 step=terminal code={code} count=1'),
                prefixed(f'RUN status=failed local_state=saved code={code} duration_ms=1 boards=0'),
            ]
            with self.subTest(impossible_code=code):
                self.assertEqual(self.parse(lines), ([], [], [], None))

        for status in ("ok", "degraded"):
            affected = status == "degraded"
            for local_state in ("saved", "not_applicable"):
                lines = board_records(1, "capped" if affected else "ok")
                lines.append(prefixed(
                    f'RUN status={status} local_state={local_state} code=none duration_ms=1 boards=1'
                ))
                with self.subTest(status=status, local_state=local_state):
                    _, _, _, terminal = self.parse(lines)
                    self.assertIsNotNone(terminal)
            for local_state in ("checkpointed", "not_saved"):
                lines = board_records(1, "capped" if affected else "ok")
                lines.append(prefixed(
                    f'RUN status={status} local_state={local_state} code=none duration_ms=1 boards=1'
                ))
                with self.subTest(status=status, impossible_local_state=local_state):
                    self.assertEqual(self.parse(lines), ([], [], [], None))

    def test_obsolete_match_records_fail_closed(self):
        for record in (
            'MATCH company="Acme" title="Engineer" location="Pune"',
            'MATCH company="recovery code: ABCD-EFGH" title="OTP: 123456" location=""',
            'MATCH index=1',
        ):
            lines = [
                *board_records(1, matched=1), prefixed(record),
                prefixed('RUN status=ok local_state=saved code=none duration_ms=1 boards=1'),
            ]
            with self.subTest(record=record):
                self.assertEqual(self.parse(lines), ([], [], [], None))

    def test_terminal_code_remains_visible_when_issue_list_is_saturated(self):
        lines = []
        for index in range(1, MAX_ISSUES + 2):
            lines.extend(board_records(index, "failed"))
        lines.extend([
            prefixed('WARN scope=run index=0 step=terminal code=report count=1'),
            prefixed(
                f'RUN status=failed local_state=saved code=report duration_ms=1 boards={MAX_ISSUES + 1}'
            ),
        ])
        boards, warnings, matches, terminal = self.parse(lines)
        text = render(
            boards, warnings, matches, terminal,
            "success", "restored", "success", "failure", "success", "false", "a" * 40,
        )
        self.assertIn("code: **report**", text)
        self.assertIn("more issue(s) omitted", text)

    def test_valid_records_without_run_are_never_rendered(self):
        boards, warnings, matches, terminal = self.parse([
            *board_records(1, "failed"),
            prefixed('MATCH company="Injected" title="Secret" location=""'),
        ])
        text = render(boards, warnings, matches, terminal, "success", "restored", "success", "failure", "skipped", "", "")
        self.assertEqual((boards, warnings, matches, terminal), ([], [], [], None))
        self.assertNotIn("Injected", text)
        self.assertNotIn("Board 1", text)

    def test_rejects_huge_numbers_files_lines_and_malformed_protocol(self):
        cases = [
            [fetch(1), prefixed(board(1)).replace("open=1", "open=" + "9" * 5_000), prefixed('RUN status=ok local_state=saved code=none duration_ms=1 boards=1')],
            [prefixed("BOARD malformed"), prefixed('RUN status=ok local_state=saved code=none duration_ms=1 boards=0')],
            [prefixed("FETCH malformed"), prefixed('RUN status=ok local_state=saved code=none duration_ms=1 boards=0')],
            [*board_records(1), prefixed("WARN scope=run index=0 step=made_up code=made_up count=1"), prefixed('RUN status=ok local_state=saved code=none duration_ms=1 boards=1')],
            [*board_records(1), prefixed("WARN scope=run index=0 step=report code=no_reporter count=600000000"), prefixed("WARN scope=run index=0 step=report code=no_reporter count=600000000"), prefixed('RUN status=ok local_state=saved code=none duration_ms=1 boards=1')],
            [fetch(1), prefixed(board(1)).replace("open=1", "open=1000000001"), prefixed('RUN status=ok local_state=saved code=none duration_ms=1 boards=1')],
            [fetch(1), prefixed(board(1, "ok", deferred=1)), prefixed('RUN status=ok local_state=saved code=none duration_ms=1 boards=1')],
        ]
        for lines in cases:
            with self.subTest(first=lines[0][:80]):
                self.assertEqual(self.parse(lines), ([], [], [], None))

        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "run.log"
            path.write_bytes(b"x" * (MAX_FILE_BYTES + 1))
            self.assertEqual(parse_log(path), ([], [], [], None))
            path.write_text("\n".join("noise" for _ in range(MAX_LINES + 1)), encoding="utf-8")
            self.assertEqual(parse_log(path), ([], [], [], None))
            path.write_bytes(b"\xff" + prefixed('RUN status=ok local_state=saved code=none duration_ms=1 boards=0').encode())
            self.assertEqual(parse_log(path), ([], [], [], None))

    def test_summarizes_large_match_count_without_per_job_records(self):
        lines = board_records(1, open=1001, matched=1001)
        lines.append(prefixed('RUN status=ok local_state=saved code=none duration_ms=1 boards=1'))
        boards, warnings, matches, terminal = self.parse(lines)
        self.assertEqual((len(boards), len(warnings), len(matches), terminal.status), (1, 0, 1, "ok"))
        self.assertEqual(matches[0].count, 1001)
        text = render(boards, warnings, matches, terminal, "success", "restored", "success", "success", "success", "false", "a" * 40)
        self.assertIn("Board 1 (custom): 1001", text)
        self.assertNotIn("Role ", text)

    def test_workflow_captures_only_logger_stderr_and_summarizes_after_publish(self):
        workflow = Path(".github/workflows/jobwatch.yml").read_text(encoding="utf-8")
        self.assertEqual(workflow.count("2>&1 > /dev/null | tee run.log"), 2)
        self.assertNotIn("2>&1 | tee run.log", workflow)
        self.assertLess(workflow.index("- name: Publish state"), workflow.index("- name: Publish run summary"))


if __name__ == "__main__":
    unittest.main()
