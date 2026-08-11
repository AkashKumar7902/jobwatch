import contextlib
import io
import json
import re
import sys
import tempfile
import unittest
from collections import Counter
from pathlib import Path
from unittest import mock

from scripts.run_summary import (
    MAX_BOARDS,
    MAX_DURATION_MS,
    MAX_FILE_BYTES,
    MAX_LINES,
    MAX_NUMBER,
    MAX_STEP_SUMMARY_BYTES,
    main,
    parse_log,
    render,
)


EMPTY = ([], [], None, None)
PREFIX = "jobwatch 2026/08/11 01:02:03 "
BOARD_TOTALS = re.compile(
    r"^BOARD index=\d+ adapter=[a-z0-9_-]+ company=.* "
    r"status=(ok|recovered|capped|degraded|partial|failed) "
    r"open=(\d+) new=(\d+) matched=(\d+) deferred=(\d+)"
)


def prefixed(line: str) -> str:
    return PREFIX + line


def board(
    index: int,
    status: str = "ok",
    new: int = 0,
    company: str | None = None,
    adapter: str = "custom",
    **overrides,
) -> str:
    metrics = dict(
        open=1,
        matched=0,
        deferred=0,
        detail_failed=0,
        retries=0,
        caps=0,
        fetch_ms=1,
        process_ms=2,
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
    company = company if company is not None else f"Board {index}"
    return (
        f"BOARD index={index} adapter={adapter} company={json.dumps(company, ensure_ascii=False)} status={status} "
        f'open={metrics["open"]} new={new} matched={metrics["matched"]} deferred={metrics["deferred"]} '
        f'detail_failed={metrics["detail_failed"]} retries={metrics["retries"]} caps={metrics["caps"]} '
        f'fetch_ms={metrics["fetch_ms"]} process_ms={metrics["process_ms"]}'
    )


def fetch(index: int, status: str = "ok", open_jobs: int = 1, duration: int = 1) -> str:
    return prefixed(f"FETCH index={index} status={status} open={open_jobs} duration_ms={duration}")


def board_warning(index: int, status: str) -> str | None:
    if status == "failed":
        return prefixed(f"WARN scope=board index={index} step=fetch code=duplicate count=1")
    if status == "partial":
        return prefixed(f"WARN scope=board index={index} step=fetch code=contract count=1")
    if status == "degraded":
        return prefixed(f"WARN scope=board index={index} step=process code=match count=1")
    return None


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


def _board_aggregates(lines: list[str]) -> tuple[Counter, list[int]]:
    counts: Counter = Counter()
    totals = [0, 0, 0, 0]
    for raw in lines:
        line = raw[len(PREFIX):] if raw.startswith(PREFIX) else raw
        match = BOARD_TOTALS.match(line)
        if match is None:
            continue
        counts[match.group(1)] += 1
        for position in range(4):
            totals[position] += int(match.group(position + 2))
    return counts, [min(total, MAX_NUMBER) for total in totals]


def poll_for(lines: list[str], **overrides) -> str:
    counts, totals = _board_aggregates(lines)
    values = {
        "boards": sum(counts.values()),
        "ok": counts["ok"],
        "recovered": counts["recovered"],
        "capped": counts["capped"],
        "degraded": counts["degraded"],
        "partial": counts["partial"],
        "failed": counts["failed"],
        "open": totals[0],
        "new": totals[1],
        "matched": totals[2],
        "deferred": totals[3],
    }
    values.update(overrides)
    return prefixed(
        "POLL boards={boards} ok={ok} recovered={recovered} capped={capped} degraded={degraded} "
        "partial={partial} failed={failed} open={open} new={new} matched={matched} deferred={deferred}".format(
            **values
        )
    )


def complete(
    lines: list[str],
    status: str | None = None,
    local_state: str = "saved",
    code: str = "none",
    duration: int = 3,
    board_count: int | None = None,
    poll_overrides: dict | None = None,
    terminal_warning: bool = True,
) -> list[str]:
    records = list(lines)
    counts, _ = _board_aggregates(records)
    count = sum(counts.values()) if board_count is None else board_count
    if status is None:
        if code == "cancelled":
            status = "cancelled"
        elif code != "none":
            status = "failed"
        else:
            status = "degraded" if sum(counts[value] for value in counts if value != "ok") else "ok"
    records.append(poll_for(records, **(poll_overrides or {})))
    if code != "none" and terminal_warning:
        records.append(prefixed(f"WARN scope=run index=0 step=terminal code={code} count=1"))
    records.append(
        prefixed(
            f"RUN status={status} local_state={local_state} code={code} duration_ms={duration} boards={count}"
        )
    )
    return records


class RunSummaryTest(unittest.TestCase):
    def parse(self, lines):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "run.log"
            path.write_text("\n".join(lines), encoding="utf-8")
            return parse_log(path)

    @staticmethod
    def rendered(parsed, **overrides):
        metadata = dict(
            restore="success",
            restore_mode="restored",
            build="success",
            poll="success",
            publish="success",
            publish_changed="false",
            publish_sha="a" * 40,
        )
        metadata.update(overrides)
        return render(*parsed, **metadata)

    def test_missing_log_and_terminal(self):
        parsed = parse_log(Path("/definitely/missing/run.log"))
        text = self.rendered(
            parsed,
            restore="failure",
            restore_mode="",
            build="skipped",
            poll="skipped",
            publish="skipped",
            publish_changed="",
            publish_sha="",
        )
        self.assertEqual(parsed, EMPTY)
        self.assertIn("No valid POLL/RUN", text)
        self.assertIn("remote state not published", text)

    def test_rejects_malformed_and_does_not_copy_it(self):
        parsed = self.parse([
            prefixed('BOARD index=1 adapter=custom company="safe" status=failed open=1'),
            prefixed('MATCH company="SECRET"'),
            prefixed("POLL boards=1 SECRET"),
            prefixed("RUN status=ok local_state=saved code=none duration_ms=1 boards=1 trailing=SECRET"),
        ])
        text = self.rendered(parsed, publish="failure", publish_changed="", publish_sha="")
        self.assertEqual(parsed, EMPTY)
        self.assertNotIn("SECRET", text)
        self.assertIn("remote state unverified", text)

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
                text = self.rendered(EMPTY, publish=outcome, publish_changed=changed, publish_sha=sha)
                self.assertIn(expected, text)

    def test_parses_and_renders_complete_aggregate(self):
        parsed = self.parse(complete(board_records(1, "degraded", 2, open=2)))
        boards, warnings, poll, terminal = parsed
        self.assertEqual((len(boards), len(warnings), terminal.status), (1, 1, "degraded"))
        self.assertEqual((boards[0].fetch_status, poll.degraded, poll.open, poll.new), ("ok", 1, 2, 2))
        text = self.rendered(parsed)
        self.assertIn("Run status: **degraded**", text)
        self.assertIn("local state: **saved**", text)
        self.assertIn("code: **none**", text)
        self.assertIn("| Poll | success |", text)
        self.assertIn("Board health: **degraded**", text)
        self.assertIn("| degraded | 1 |", text)
        self.assertIn("| Deferred | 1 |", text)
        self.assertIn("| Detail failed | 0 |", text)
        self.assertIn("| Process ms | 2 |", text)
        self.assertIn("| 1 | Board 1 | custom | degraded | ok | process/match count=1 | 2 | 2 |", text)

    def test_renders_every_status_bucket_and_additive_total(self):
        lines = []
        for index, status in enumerate(("ok", "recovered", "capped", "degraded", "partial", "failed"), 1):
            lines.extend(board_records(index, status))
        parsed = self.parse(complete(lines))
        text = self.rendered(parsed)
        for status in ("ok", "recovered", "capped", "degraded", "partial", "failed"):
            self.assertIn(f"| {status} | 1 |", text)
        for aggregate in (
            "| **Total** | **6** |",
            "| Open | 5 |",
            "| Detail failed | 0 |",
            "| Retries | 1 |",
            "| Caps | 1 |",
            "| Fetch ms | 6 |",
            "| Process ms | 12 |",
        ):
            self.assertIn(aggregate, text)

    def test_parses_actionable_fetch_contract_codes(self):
        for code in ("missing_field", "mismatch", "unstable_snapshot"):
            with self.subTest(code=code):
                records = [
                    fetch(1, "failed", 0),
                    prefixed(board(1, "failed")),
                    prefixed(f"WARN scope=board index=1 step=fetch code={code} count=1"),
                ]
                boards, warnings, poll, terminal = self.parse(complete(records))
                self.assertEqual((len(boards), len(warnings), poll.failed, terminal.status), (1, 1, 1, "degraded"))

    def test_all_263_boards_have_one_index_ordered_row_without_omissions(self):
        lines = []
        for index in range(1, 264):
            status = "capped" if index % 11 == 0 else "ok"
            company = "Exact hidden board" if index == 263 else f"Board {index}"
            lines.extend(board_records(index, status, new=1, matched=1, company=company))
        parsed = self.parse(complete(lines))
        boards, _, poll, _ = parsed
        self.assertEqual((len(boards), poll.boards), (263, 263))
        text = self.rendered(parsed)
        indices = [int(value) for value in re.findall(r"^\| (\d+) \|", text, re.MULTILINE)]
        self.assertEqual(indices, list(range(1, 264)))
        self.assertEqual(text.count("Exact hidden board"), 1)
        self.assertNotIn("omitted", text.lower())
        self.assertNotIn("Operational issues", text)
        self.assertNotIn("Boards with new postings", text)
        self.assertNotIn("### Matches", text)

    def test_every_run_warning_is_rendered_without_a_cap(self):
        records = board_records(1)
        records.extend(prefixed("WARN scope=run index=0 step=report code=no_reporter count=1") for _ in range(30))
        records.extend(prefixed("WARN scope=run index=0 step=checkpoint code=save_failed count=1") for _ in range(25))
        parsed = self.parse(complete(records, code="report"))
        _, warnings, _, terminal = parsed
        self.assertEqual(terminal.code, "report")
        self.assertEqual([(warning.step, warning.code, warning.count) for warning in warnings if warning.scope == "run"], [
            ("checkpoint", "save_failed", 25),
            ("report", "no_reporter", 30),
            ("terminal", "report", 1),
        ])
        text = self.rendered(parsed)
        for row in (
            "| run | 0 | checkpoint | save_failed | 25 |",
            "| run | 0 | report | no_reporter | 30 |",
            "| run | 0 | terminal | report | 1 |",
        ):
            self.assertIn(row, text)

    def test_fetch_statuses_and_setup_not_run_are_distinct(self):
        records = [
            *board_records(1, "ok"),
            *board_records(2, "partial"),
            *board_records(3, "failed"),
            prefixed(board(4, "failed", open=0, fetch_ms=0, process_ms=0)),
            prefixed("WARN scope=board index=4 step=setup code=not_run count=1"),
        ]
        parsed = self.parse(complete(records, local_state="checkpointed", code="persistence"))
        boards, _, poll, terminal = parsed
        self.assertEqual([record.fetch_status for record in boards], ["ok", "partial", "failed", "not_run"])
        self.assertEqual((poll.ok, poll.partial, poll.failed, terminal.code), (1, 1, 2, "persistence"))
        text = self.rendered(parsed)
        for index, fetch_status in enumerate(("ok", "partial", "failed", "not_run"), 1):
            self.assertRegex(text, rf"(?m)^\| {index} \| .* \| {fetch_status} \|")

    def test_rejects_fetch_status_divergence_and_cross_check_failures(self):
        cases = [
            complete([fetch(1, "partial", 1), prefixed(board(1))]),
            complete([
                fetch(1, "ok", 1),
                prefixed(board(1, "partial")),
                prefixed("WARN scope=board index=1 step=fetch code=contract count=1"),
            ]),
            complete([
                fetch(1, "failed", 0, 0),
                prefixed(board(1, "failed", open=0, fetch_ms=0, process_ms=0)),
                prefixed("WARN scope=board index=1 step=setup code=not_run count=1"),
            ]),
            complete([fetch(1, open_jobs=2), prefixed(board(1))]),
            complete([fetch(1, duration=2), prefixed(board(1))]),
        ]
        for lines in cases:
            with self.subTest(lines=lines[:2]):
                self.assertEqual(self.parse(lines), EMPTY)

    def test_poll_is_required_unique_strict_consistent_and_ordered(self):
        records = board_records(1, "degraded", new=3, open=4, matched=1, deferred=1)
        run = prefixed("RUN status=degraded local_state=saved code=none duration_ms=1 boards=1")
        valid_poll = poll_for(records)
        cases = [
            [*records, run],
            [*records, valid_poll, valid_poll, run],
            [*records, prefixed("POLL boards=1"), run],
            [*records, valid_poll + " trailing=1", run],
            [valid_poll, *records, run],
        ]
        for field, wrong in (
            ("boards", 0),
            ("degraded", 0),
            ("ok", 1),
            ("open", 3),
            ("new", 2),
            ("matched", 0),
            ("deferred", 0),
        ):
            cases.append([*records, poll_for(records, **{field: wrong}), run])
        for lines in cases:
            with self.subTest(last=lines[-2]):
                self.assertEqual(self.parse(lines), EMPTY)

    def test_poll_phase_accepts_only_terminal_warning_then_run(self):
        empty_poll = poll_for([])
        terminal_warn = prefixed("WARN scope=run index=0 step=terminal code=report count=1")
        failed_run = prefixed("RUN status=failed local_state=saved code=report duration_ms=1 boards=0")
        _, warnings, poll, terminal = self.parse([empty_poll, terminal_warn, failed_run])
        self.assertEqual((poll.boards, warnings[0].step, terminal.code), (0, "terminal", "report"))

        late_records = [
            fetch(1),
            prefixed(board(1)),
            prefixed("WARN scope=board index=1 step=fetch code=unknown count=1"),
            prefixed("WARN scope=run index=0 step=report code=no_reporter count=1"),
        ]
        cases = [
            [terminal_warn, empty_poll, failed_run],
            [empty_poll, prefixed("WARN scope=run index=0 step=report code=no_reporter count=1"), failed_run],
        ]
        for late in late_records:
            cases.append([empty_poll, late, prefixed("RUN status=ok local_state=saved code=none duration_ms=1 boards=0")])
        for lines in cases:
            with self.subTest(late=lines[1]):
                self.assertEqual(self.parse(lines), EMPTY)

    def test_poll_additive_totals_use_producer_clamping(self):
        records = []
        for index in (1, 2):
            records.extend(board_records(index, open=MAX_NUMBER, new=MAX_NUMBER, matched=MAX_NUMBER))
        boards, _, poll, terminal = self.parse(complete(records))
        self.assertEqual((len(boards), terminal.status), (2, "ok"))
        self.assertEqual((poll.open, poll.new, poll.matched, poll.deferred), (MAX_NUMBER, MAX_NUMBER, MAX_NUMBER, 0))
        self.assertEqual(
            self.parse(complete(records, poll_overrides={"open": MAX_NUMBER - 1})),
            EMPTY,
        )

    def test_company_length_matches_go_producer_envelope(self):
        valid_companies = ["a" * 120, "b" * 120 + "…"]
        records = []
        for index, company in enumerate(valid_companies, 1):
            records.extend(board_records(index, company=company))
        parsed = self.parse(complete(records))
        self.assertEqual([record.company for record in parsed[0]], valid_companies)
        text = self.rendered(parsed)
        self.assertIn(valid_companies[1], text)

        for company in ("x" * 121, "x" * 121 + "…"):
            with self.subTest(length=len(company), ellipsis=company.endswith("…")):
                self.assertEqual(self.parse(complete(board_records(1, company=company))), EMPTY)

    def test_company_markdown_html_is_escaped_without_hiding_quotes(self):
        company = '"quoted" & <tag> | [link](target) `code` > text'
        parsed = self.parse(complete(board_records(1, company=company)))
        text = self.rendered(parsed)
        self.assertIn('"quoted"', text)
        self.assertNotIn("&quot;quoted&quot;", text)
        self.assertIn("&amp;", text)
        self.assertIn("&lt;tag&gt;", text)
        self.assertIn(r"\|", text)
        self.assertIn(r"\[link\]\(target\)", text)
        self.assertIn(r"\`code\`", text)
        self.assertNotIn("<tag>", text)

    def test_rejects_duplicate_contradictory_and_post_terminal_records(self):
        one = board_records(1)
        cases = [
            complete([fetch(1), prefixed(board(1)), prefixed(board(1))], board_count=1),
            complete([fetch(1), fetch(1), prefixed(board(1))]),
            complete([fetch(1, open_jobs=2), prefixed(board(1))]),
            complete(board_records(1, "failed"), status="ok"),
            [*complete(one), "SECRET after terminal"],
        ]
        for lines in cases:
            with self.subTest(lines=lines):
                self.assertEqual(self.parse(lines), EMPTY)

    def test_rejects_impossible_board_metrics(self):
        cases = [
            complete([fetch(1), prefixed(board(1, new=2))]),
            complete([fetch(1), prefixed(board(1, matched=2))]),
            complete([
                fetch(1),
                prefixed(board(1, "degraded", deferred=2)),
                prefixed("WARN scope=board index=1 step=process code=match count=1"),
            ]),
            complete([
                fetch(1),
                prefixed(board(1, "degraded", deferred=0, detail_failed=2)),
                prefixed("WARN scope=board index=1 step=process code=detail count=1"),
            ]),
            complete([
                fetch(1, open_jobs=2),
                prefixed(board(1, "degraded", open=2, matched=1, deferred=1, detail_failed=1)),
                prefixed("WARN scope=board index=1 step=process code=detail_and_match count=1"),
            ]),
            complete([fetch(1), prefixed(board(1, "capped", caps=2))]),
        ]
        for lines in cases:
            with self.subTest(board=lines[1]):
                self.assertEqual(self.parse(lines), EMPTY)

    def test_duration_bounds_roundtrip_and_reject_overflow(self):
        valid = complete(board_records(1, fetch_ms=MAX_DURATION_MS, process_ms=MAX_DURATION_MS), duration=MAX_DURATION_MS)
        boards, _, _, terminal = self.parse(valid)
        self.assertEqual(
            (boards[0].fetch_ms, boards[0].process_ms, terminal.duration_ms),
            (MAX_DURATION_MS, MAX_DURATION_MS, MAX_DURATION_MS),
        )

        too_large = MAX_DURATION_MS + 1
        invalid = [
            complete([fetch(1, duration=too_large), prefixed(board(1, fetch_ms=too_large))]),
            complete([fetch(1), prefixed(board(1, process_ms=too_large))]),
            complete(board_records(1), duration=too_large),
        ]
        for lines in invalid:
            with self.subTest(lines=lines):
                self.assertEqual(self.parse(lines), EMPTY)

    def test_board_and_terminal_warnings_are_single_events(self):
        board_base = [fetch(1, "failed", 0), prefixed(board(1, "failed"))]
        cases = [
            complete([*board_base, prefixed("WARN scope=board index=1 step=fetch code=duplicate count=2")]),
            complete([
                *board_base,
                prefixed("WARN scope=board index=1 step=fetch code=duplicate count=1"),
                prefixed("WARN scope=board index=1 step=fetch code=duplicate count=1"),
            ]),
            [
                poll_for([]),
                prefixed("WARN scope=run index=0 step=terminal code=cancelled count=2"),
                prefixed("RUN status=cancelled local_state=saved code=cancelled duration_ms=1 boards=0"),
            ],
            [
                poll_for([]),
                prefixed("WARN scope=run index=0 step=terminal code=cancelled count=1"),
                prefixed("WARN scope=run index=0 step=terminal code=cancelled count=1"),
                prefixed("RUN status=cancelled local_state=saved code=cancelled duration_ms=1 boards=0"),
            ],
        ]
        for lines in cases:
            with self.subTest(lines=lines):
                self.assertEqual(self.parse(lines), EMPTY)

    def test_cancelled_partially_processed_board_roundtrips_all_fetch_outcomes(self):
        for fetch_status, open_jobs in (("ok", 2), ("partial", 2), ("failed", 0)):
            matched = 1 if open_jobs else 0
            new = 1 if open_jobs else 0
            records = [
                fetch(1, fetch_status, open_jobs),
                prefixed(board(1, "failed", new, open=open_jobs, matched=matched)),
                prefixed("WARN scope=board index=1 step=process code=cancelled count=1"),
            ]
            with self.subTest(fetch_status=fetch_status):
                boards, _, _, terminal = self.parse(complete(records, code="cancelled"))
                self.assertEqual((boards[0].fetch_status, terminal.status, terminal.code), (fetch_status, "cancelled", "cancelled"))

        not_run = [
            fetch(1, "partial", 2),
            prefixed(board(1, "failed", open=2)),
            prefixed("WARN scope=board index=1 step=process code=not_run count=1"),
        ]
        boards, _, _, terminal = self.parse(complete(not_run, local_state="checkpointed", code="persistence"))
        self.assertEqual((boards[0].fetch_status, terminal.status, terminal.code), ("partial", "failed", "persistence"))

    def test_cancelled_terminal_requires_cancelled_code_and_warning(self):
        empty_poll = poll_for([])
        cases = [
            [
                empty_poll,
                prefixed("WARN scope=run index=0 step=terminal code=unknown count=1"),
                prefixed("RUN status=cancelled local_state=saved code=unknown duration_ms=1 boards=0"),
            ],
            [empty_poll, prefixed("RUN status=cancelled local_state=saved code=cancelled duration_ms=1 boards=0")],
            [
                empty_poll,
                prefixed("WARN scope=run index=0 step=terminal code=unknown count=1"),
                prefixed("RUN status=cancelled local_state=saved code=cancelled duration_ms=1 boards=0"),
            ],
            [
                empty_poll,
                prefixed("WARN scope=run index=0 step=terminal code=cancelled count=1"),
                prefixed("RUN status=failed local_state=saved code=cancelled duration_ms=1 boards=0"),
            ],
        ]
        for lines in cases:
            with self.subTest(lines=lines):
                self.assertEqual(self.parse(lines), EMPTY)

    def test_terminal_codes_and_successful_local_state_match_producer(self):
        producer_codes = {"persistence", "notify", "report", "match", "seed", "fetch", "unknown"}
        for code in producer_codes:
            with self.subTest(code=code):
                _, _, _, terminal = self.parse(complete([], code=code))
                self.assertEqual((terminal.status, terminal.code), ("failed", code))

        for code in {
            "timeout",
            "transport",
            "forbidden",
            "unauthorized",
            "not_found",
            "rate_limited",
            "server",
            "duplicate",
            "contract",
            "invalid_response",
        }:
            lines = [
                poll_for([]),
                prefixed(f"WARN scope=run index=0 step=terminal code={code} count=1"),
                prefixed(f"RUN status=failed local_state=saved code={code} duration_ms=1 boards=0"),
            ]
            with self.subTest(impossible_code=code):
                self.assertEqual(self.parse(lines), EMPTY)

        for status in ("ok", "degraded"):
            records = board_records(1, "capped" if status == "degraded" else "ok")
            for local_state in ("saved", "not_applicable"):
                with self.subTest(status=status, local_state=local_state):
                    _, _, _, terminal = self.parse(complete(records, status=status, local_state=local_state))
                    self.assertIsNotNone(terminal)
            for local_state in ("checkpointed", "not_saved"):
                with self.subTest(status=status, impossible_local_state=local_state):
                    self.assertEqual(self.parse(complete(records, status=status, local_state=local_state)), EMPTY)

    def test_obsolete_match_records_fail_closed(self):
        for record in (
            'MATCH company="Acme" title="Engineer" location="Pune"',
            'MATCH company="recovery code: ABCD-EFGH" title="OTP: 123456" location=""',
            "MATCH index=1",
        ):
            lines = [*board_records(1, matched=1), prefixed(record)]
            with self.subTest(record=record):
                self.assertEqual(self.parse(complete(lines)), EMPTY)

    def test_valid_records_without_poll_and_run_are_never_rendered(self):
        parsed = self.parse([
            *board_records(1, "failed"),
            prefixed('MATCH company="Injected" title="Secret" location=""'),
        ])
        text = self.rendered(parsed, poll="failure", publish="skipped", publish_changed="", publish_sha="")
        self.assertEqual(parsed, EMPTY)
        self.assertNotIn("Injected", text)
        self.assertNotIn("Board 1", text)

    def test_rejects_huge_numbers_files_lines_and_malformed_protocol(self):
        cases = [
            [
                fetch(1),
                prefixed(board(1)).replace("open=1", "open=" + "9" * 5_000),
                poll_for([], boards=1, ok=1),
                prefixed("RUN status=ok local_state=saved code=none duration_ms=1 boards=1"),
            ],
            [prefixed("BOARD malformed"), poll_for([]), prefixed("RUN status=ok local_state=saved code=none duration_ms=1 boards=0")],
            [prefixed("FETCH malformed"), poll_for([]), prefixed("RUN status=ok local_state=saved code=none duration_ms=1 boards=0")],
            complete([*board_records(1), prefixed("WARN scope=run index=0 step=made_up code=made_up count=1")]),
            complete([
                *board_records(1),
                prefixed("WARN scope=run index=0 step=report code=no_reporter count=600000000"),
                prefixed("WARN scope=run index=0 step=report code=no_reporter count=600000000"),
            ]),
            complete([fetch(1), prefixed(board(1)).replace("open=1", "open=1000000001")]),
            complete([fetch(1), prefixed(board(1, "ok", deferred=1))]),
            [prefixed("POLL malformed"), prefixed("RUN status=ok local_state=saved code=none duration_ms=1 boards=0")],
        ]
        for lines in cases:
            with self.subTest(first=lines[0][:80]):
                self.assertEqual(self.parse(lines), EMPTY)

        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "run.log"
            path.write_bytes(b"x" * (MAX_FILE_BYTES + 1))
            self.assertEqual(parse_log(path), EMPTY)
            path.write_text("\n".join("noise" for _ in range(MAX_LINES + 1)), encoding="utf-8")
            self.assertEqual(parse_log(path), EMPTY)
            path.write_bytes(b"\xff" + prefixed("RUN status=ok local_state=saved code=none duration_ms=1 boards=0").encode())
            self.assertEqual(parse_log(path), EMPTY)

    def test_large_match_count_stays_aggregate_only(self):
        parsed = self.parse(complete(board_records(1, open=1001, matched=1001)))
        boards, _, poll, terminal = parsed
        self.assertEqual((boards[0].matched, poll.matched, terminal.status), (1001, 1001, "ok"))
        text = self.rendered(parsed)
        self.assertRegex(text, r"(?m)^\| 1 \| Board 1 \| custom \| ok \| ok \| none \| 1001 \| 0 \| 1001 \|")
        self.assertIn("per-job title, location, URL, and body data stay out", text)
        self.assertNotIn("Role ", text)

    def test_adversarial_1000_board_summary_is_complete_and_below_one_mib(self):
        lines = []
        company = "&" * 120 + "…"
        adapter = "a" * 48
        for index in range(1, MAX_BOARDS + 1):
            lines.extend([
                fetch(index, "ok", MAX_NUMBER, MAX_DURATION_MS),
                prefixed(board(
                    index,
                    "degraded",
                    MAX_NUMBER,
                    company=company,
                    adapter=adapter,
                    open=MAX_NUMBER,
                    matched=333_333_333,
                    deferred=333_333_333,
                    detail_failed=333_333_334,
                    retries=MAX_NUMBER,
                    caps=1,
                    fetch_ms=MAX_DURATION_MS,
                    process_ms=MAX_DURATION_MS,
                )),
                prefixed(f"WARN scope=board index={index} step=process code=detail_and_match count=1"),
            ])
        parsed = self.parse(complete(lines))
        self.assertEqual((len(parsed[0]), parsed[2].boards), (MAX_BOARDS, MAX_BOARDS))
        text = self.rendered(parsed)
        indices = [int(value) for value in re.findall(r"^\| (\d+) \|", text, re.MULTILINE)]
        self.assertEqual(indices, list(range(1, MAX_BOARDS + 1)))
        self.assertNotIn("omitted", text.lower())
        self.assertLessEqual(len((text + "\n").encode("utf-8")), MAX_STEP_SUMMARY_BYTES)

    def test_main_fails_loudly_instead_of_truncating_oversize_output(self):
        argv = [
            "run_summary.py",
            "--log",
            "unused.log",
            "--restore",
            "success",
            "--build",
            "success",
            "--poll",
            "success",
            "--publish",
            "skipped",
        ]
        stdout = io.StringIO()
        with (
            mock.patch.object(sys, "argv", argv),
            mock.patch("scripts.run_summary.parse_log", return_value=EMPTY),
            mock.patch("scripts.run_summary.render", return_value="x" * MAX_STEP_SUMMARY_BYTES),
            contextlib.redirect_stdout(stdout),
        ):
            with self.assertRaisesRegex(SystemExit, "refusing to truncate"):
                main()
        self.assertEqual(stdout.getvalue(), "")

        stdout = io.StringIO()
        with (
            mock.patch.object(sys, "argv", argv),
            mock.patch("scripts.run_summary.parse_log", return_value=EMPTY),
            mock.patch("scripts.run_summary.render", return_value="x" * (MAX_STEP_SUMMARY_BYTES - 1)),
            contextlib.redirect_stdout(stdout),
        ):
            main()
        self.assertEqual(len(stdout.getvalue().encode("utf-8")), MAX_STEP_SUMMARY_BYTES)

    def test_workflow_captures_only_logger_stderr_and_summarizes_after_publish(self):
        workflow = Path(".github/workflows/jobwatch.yml").read_text(encoding="utf-8")
        self.assertEqual(workflow.count("2>&1 > /dev/null | tee run.log"), 2)
        self.assertNotIn("2>&1 | tee run.log", workflow)
        self.assertLess(workflow.index("- name: Publish state"), workflow.index("- name: Publish run summary"))


if __name__ == "__main__":
    unittest.main()
