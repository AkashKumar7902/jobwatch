#!/usr/bin/env python3
"""Render a complete bounded GitHub summary from jobwatch's authenticated log grammar."""

from __future__ import annotations

import argparse
import html
import json
import re
from dataclasses import dataclass, replace
from pathlib import Path


MAX_FILE_BYTES = 8 * 1024 * 1024
MAX_LINES = 10_000
MAX_LINE_BYTES = 4_096
MAX_NUMBER = 1_000_000_000
MAX_DURATION_MS = 86_400_000
MAX_BOARDS = 1_000
MAX_WARNINGS = 2_000
MAX_STEP_SUMMARY_BYTES = 1024 * 1024

PREFIX = re.compile(r"^jobwatch \d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} ")
KNOWN_RECORD = re.compile(r"^(?:FETCH|BOARD|WARN|POLL|MATCH|RUN)(?:\s|$)")
TOKEN = r"[a-z0-9_-]{1,48}"
NUMBER = r"[0-9]{1,10}"
QUOTED = r'"(?:\\.|[^"\\])*"'
BOARD_RE = re.compile(
    rf"^BOARD index=({NUMBER}) adapter=({TOKEN}) company=({QUOTED}) "
    rf"status=(ok|recovered|capped|degraded|partial|failed) open=({NUMBER}) new=({NUMBER}) "
    rf"matched=({NUMBER}) deferred=({NUMBER}) detail_failed=({NUMBER}) retries=({NUMBER}) "
    rf"caps=({NUMBER}) fetch_ms=({NUMBER}) process_ms=({NUMBER})$"
)
FETCH_RE = re.compile(
    rf"^FETCH index=({NUMBER}) status=(ok|partial|failed) open=({NUMBER}) duration_ms=({NUMBER})$"
)
WARN_RE = re.compile(
    rf"^WARN scope=(run|board) index=({NUMBER}) step=({TOKEN}) code=({TOKEN}) count=({NUMBER})$"
)
POLL_RE = re.compile(
    rf"^POLL boards=({NUMBER}) ok=({NUMBER}) recovered=({NUMBER}) capped=({NUMBER}) "
    rf"degraded=({NUMBER}) partial=({NUMBER}) failed=({NUMBER}) open=({NUMBER}) "
    rf"new=({NUMBER}) matched=({NUMBER}) deferred=({NUMBER})$"
)
RUN_RE = re.compile(
    rf"^RUN status=(ok|degraded|failed|cancelled) "
    rf"local_state=(saved|checkpointed|not_saved|not_applicable) code=({TOKEN}) "
    rf"duration_ms=({NUMBER}) boards=({NUMBER})$"
)
HEX_SHA = re.compile(r"^[0-9a-f]{40}$")
OUTCOMES = {"success", "failure", "cancelled", "skipped"}
MODES = {"restored", "bootstrap"}
STATUS_ORDER = ("ok", "recovered", "capped", "degraded", "partial", "failed")

BOARD_WARN = {
    "setup": {"not_run"},
    "fetch": {
        "cancelled", "timeout", "transport", "forbidden", "unauthorized", "not_found",
        "rate_limited", "server", "duplicate", "contract", "missing_field", "mismatch",
        "unstable_snapshot", "invalid_response", "unknown",
    },
    "process": {"cancelled", "not_run", "detail", "match", "detail_and_match"},
}
RUN_WARN = {
    "report": {"no_reporter"},
    "checkpoint": {"save_failed"},
    "terminal": {
        "persistence", "notify", "report", "match", "seed", "fetch", "cancelled", "unknown",
    },
}


@dataclass(frozen=True)
class Board:
    index: int
    adapter: str
    company: str
    status: str
    fetch_status: str
    open: int
    new: int
    matched: int
    deferred: int
    detail_failed: int
    retries: int
    caps: int
    fetch_ms: int
    process_ms: int


@dataclass(frozen=True)
class Warning:
    scope: str
    step: str
    code: str
    index: int
    count: int


@dataclass(frozen=True)
class Terminal:
    status: str
    local_state: str
    code: str
    duration_ms: int
    boards: int


@dataclass(frozen=True)
class Poll:
    boards: int
    ok: int
    recovered: int
    capped: int
    degraded: int
    partial: int
    failed: int
    open: int
    new: int
    matched: int
    deferred: int


def _unquote(value: str) -> str | None:
    try:
        decoded = json.loads(value)
    except (ValueError, TypeError):
        return None
    return decoded if isinstance(decoded, str) else None


def _number(value: str) -> int | None:
    if not value or len(value) > 10:
        return None
    try:
        number = int(value)
    except (ValueError, OverflowError):
        return None
    return number if 0 <= number <= MAX_NUMBER else None


def _read_lines(path: Path) -> list[str] | None:
    try:
        if path.stat().st_size > MAX_FILE_BYTES:
            return None
        with path.open("rb") as handle:
            data = handle.read(MAX_FILE_BYTES + 1)
    except OSError:
        return []
    if len(data) > MAX_FILE_BYTES:
        return None
    try:
        text = data.decode("utf-8")
    except UnicodeDecodeError:
        return None
    lines = text.splitlines()
    if len(lines) > MAX_LINES:
        return None
    if any(len(line.encode("utf-8")) > MAX_LINE_BYTES for line in lines):
        return None
    return lines


def _canonical_status(board: Board, warnings: list[Warning]) -> str | None:
    setup = any(warning.step == "setup" for warning in warnings)
    fetch = any(warning.step == "fetch" for warning in warnings)
    process = any(warning.step == "process" for warning in warnings)
    if setup:
        return "failed"
    if fetch:
        return "partial" if board.open > 0 else "failed"
    if process and any(warning.code in {"cancelled", "not_run"} for warning in warnings):
        return "failed"
    if board.deferred > 0 or board.detail_failed > 0 or process:
        return "degraded"
    if board.caps > 0:
        return "capped"
    if board.retries > 0:
        return "recovered"
    return "ok"


def parse_log(path: Path) -> tuple[list[Board], list[Warning], Poll | None, Terminal | None]:
    lines = _read_lines(path)
    if lines is None:
        return [], [], None, None

    boards: dict[int, Board] = {}
    fetches: dict[int, tuple[str, int, int]] = {}
    warning_counts: dict[tuple[str, int, str, str], int] = {}
    pending_board_warning: int | None = None
    poll: Poll | None = None
    terminal: Terminal | None = None
    invalid = False

    for raw in lines:
        if terminal is not None:
            if raw.strip():
                invalid = True
            continue
        prefix = PREFIX.match(raw)
        if prefix is None:
            continue
        line = raw[prefix.end():]

        if pending_board_warning is not None:
            immediate = WARN_RE.fullmatch(line)
            expected_index = pending_board_warning
            pending_board_warning = None
            if (
                immediate is None or immediate.group(1) != "board" or
                _number(immediate.group(2)) != expected_index
            ):
                invalid = True

        match = FETCH_RE.fullmatch(line)
        if match:
            index, open_jobs, duration = (_number(match.group(position)) for position in (1, 3, 4))
            status = match.group(2)
            if (
                poll is not None or boards or index is None or open_jobs is None or duration is None or
                duration > MAX_DURATION_MS or index == 0 or
                index > MAX_BOARDS or index in fetches or len(fetches) >= MAX_BOARDS or
                (status == "partial" and open_jobs == 0) or (status == "failed" and open_jobs != 0)
            ):
                invalid = True
            else:
                fetches[index] = (status, open_jobs, duration)
            continue

        match = BOARD_RE.fullmatch(line)
        if match:
            numbers = [_number(match.group(index)) for index in range(1, 2)]
            numbers.extend(_number(match.group(index)) for index in range(5, 14))
            company = _unquote(match.group(3))
            if company is None or any(number is None for number in numbers):
                invalid = True
                continue
            board = Board(numbers[0], match.group(2), company, match.group(4), "", *numbers[1:])
            if (
                poll is not None or board.index == 0 or board.index > MAX_BOARDS or
                board.index != len(boards) + 1 or board.index in boards or len(boards) >= MAX_BOARDS or
                not (len(board.company) <= 120 or (len(board.company) == 121 and board.company.endswith("…"))) or
                board.new > board.open or board.matched > board.open or board.deferred > board.open or
                board.detail_failed > board.open or board.matched + board.deferred + board.detail_failed > board.open or
                board.caps not in {0, 1} or board.fetch_ms > MAX_DURATION_MS or board.process_ms > MAX_DURATION_MS
            ):
                invalid = True
            else:
                boards[board.index] = board
                if board.status in {"failed", "partial", "degraded"}:
                    pending_board_warning = board.index
            continue

        match = WARN_RE.fullmatch(line)
        if match:
            index, count = _number(match.group(2)), _number(match.group(5))
            scope, step, code = match.group(1), match.group(3), match.group(4)
            valid_combo = code in (RUN_WARN.get(step, set()) if scope == "run" else BOARD_WARN.get(step, set()))
            terminal_warning = scope == "run" and step == "terminal"
            wrong_phase = (terminal_warning and poll is None) or (not terminal_warning and poll is not None)
            board_warning_out_of_order = scope == "board" and index is not None and index != len(boards)
            if (
                index is None or count is None or count == 0 or not valid_combo or
                (scope == "run") != (index == 0) or wrong_phase or board_warning_out_of_order
            ):
                invalid = True
                continue
            key = (scope, index, step, code)
            if (scope == "board" or step == "terminal") and (count != 1 or key in warning_counts):
                invalid = True
                continue
            if key not in warning_counts and len(warning_counts) >= MAX_WARNINGS:
                invalid = True
                continue
            combined = warning_counts.get(key, 0) + count
            if combined > MAX_NUMBER:
                invalid = True
                continue
            warning_counts[key] = combined
            continue

        match = POLL_RE.fullmatch(line)
        if match:
            values = [_number(match.group(index)) for index in range(1, 12)]
            if poll is not None or any(value is None for value in values) or values[0] > MAX_BOARDS:
                invalid = True
            else:
                poll = Poll(*values)
            continue

        match = RUN_RE.fullmatch(line)
        if match:
            duration, board_count = _number(match.group(4)), _number(match.group(5))
            if (
                poll is None or duration is None or duration > MAX_DURATION_MS or
                board_count is None or board_count > MAX_BOARDS
            ):
                invalid = True
            else:
                terminal = Terminal(match.group(1), match.group(2), match.group(3), duration, board_count)
            continue

        if KNOWN_RECORD.match(line):
            invalid = True

    if invalid or pending_board_warning is not None or poll is None or terminal is None:
        return [], [], None, None

    ordered = sorted(boards.values(), key=lambda board: board.index)
    if (
        terminal.boards != len(ordered) or poll.boards != terminal.boards or
        [board.index for board in ordered] != list(range(1, len(ordered) + 1))
    ):
        return [], [], None, None

    status_counts = {status: 0 for status in STATUS_ORDER}
    for board in ordered:
        status_counts[board.status] += 1
    expected_statuses = tuple(status_counts[status] for status in STATUS_ORDER)
    poll_statuses = (poll.ok, poll.recovered, poll.capped, poll.degraded, poll.partial, poll.failed)
    expected_totals = tuple(
        min(sum(getattr(board, field) for board in ordered), MAX_NUMBER)
        for field in ("open", "new", "matched", "deferred")
    )
    if poll_statuses != expected_statuses or (poll.open, poll.new, poll.matched, poll.deferred) != expected_totals:
        return [], [], None, None

    warnings = [Warning(scope, step, code, index, count) for (scope, index, step, code), count in warning_counts.items()]
    warnings.sort(key=lambda warning: (warning.scope, warning.index, warning.step, warning.code))
    board_warnings: dict[int, list[Warning]] = {}
    for warning in warnings:
        if warning.scope == "board":
            if warning.index not in boards:
                return [], [], None, None
            board_warnings.setdefault(warning.index, []).append(warning)

    validated: list[Board] = []
    for board in ordered:
        current_warnings = board_warnings.get(board.index, [])
        if len(current_warnings) > 1:
            return [], [], None, None
        if _canonical_status(board, current_warnings) != board.status:
            return [], [], None, None
        setup = any(warning.step == "setup" for warning in current_warnings)
        fetch_warning = any(warning.step == "fetch" for warning in current_warnings)
        fetch = fetches.get(board.index)
        if setup:
            setup_metrics = (
                board.open, board.new, board.matched, board.deferred, board.detail_failed,
                board.retries, board.caps, board.fetch_ms, board.process_ms,
            )
            if fetch is not None or any(setup_metrics):
                return [], [], None, None
            validated.append(replace(board, fetch_status="not_run"))
            continue
        if fetch is None:
            return [], [], None, None
        if fetch[1:] != (board.open, board.fetch_ms):
            return [], [], None, None
        warning = current_warnings[0] if current_warnings else None
        unprocessed = warning is not None and warning.step == "process" and warning.code in {"cancelled", "not_run"}
        if fetch_warning:
            expected_fetch_status = "partial" if board.open > 0 else "failed"
            if fetch[0] != expected_fetch_status:
                return [], [], None, None
        elif not unprocessed and fetch[0] != "ok":
            return [], [], None, None
        if current_warnings:
            if warning.step == "process" and (
                (warning.code == "detail" and not (board.detail_failed > 0 and board.deferred == 0)) or
                (warning.code == "match" and not (board.deferred > 0 and board.detail_failed == 0)) or
                (warning.code == "detail_and_match" and not (board.deferred > 0 and board.detail_failed > 0))
            ):
                return [], [], None, None
        validated.append(replace(board, fetch_status=fetch[0]))

    if any(index not in boards for index in fetches):
        return [], [], None, None
    affected = any(board.status != "ok" for board in validated)
    terminal_warnings = [warning for warning in warnings if warning.scope == "run" and warning.step == "terminal"]
    if terminal.status == "ok":
        valid_terminal = not affected and terminal.code == "none" and not terminal_warnings
    elif terminal.status == "degraded":
        valid_terminal = affected and terminal.code == "none" and not terminal_warnings
    elif terminal.status == "cancelled":
        valid_terminal = (
            terminal.code == "cancelled" and len(terminal_warnings) == 1 and
            terminal_warnings[0].code == "cancelled" and terminal_warnings[0].count == 1
        )
    else:
        valid_terminal = (
            terminal.code not in {"none", "cancelled"} and len(terminal_warnings) == 1 and
            terminal_warnings[0].code == terminal.code and terminal_warnings[0].count == 1
        )
    if not valid_terminal:
        return [], [], None, None
    if terminal.status in {"ok", "degraded"} and terminal.local_state not in {"saved", "not_applicable"}:
        return [], [], None, None
    return validated, warnings, poll, terminal


def safe_text(value: str) -> str:
    value = "".join(ch if ch.isprintable() and ch not in "\r\n" else "�" for ch in value)
    value = html.escape(value, quote=False)
    return re.sub(r"([\\`*_{}\[\]()#+.!|>-])", r"\\\1", value)


def safe_outcome(value: str) -> str:
    return value if value in OUTCOMES else "unknown"


def render(
    boards: list[Board], warnings: list[Warning], poll_record: Poll | None, terminal: Terminal | None,
    restore: str, restore_mode: str, build: str, poll: str, publish: str, publish_changed: str, publish_sha: str,
) -> str:
    restore, build, poll, publish = map(safe_outcome, (restore, build, poll, publish))
    mode = restore_mode if restore_mode in MODES else "unknown"
    rows = [
        "### jobwatch run", "", "| Phase | Outcome |", "|---|---|",
        f"| Restore | {restore} ({mode}) |", f"| Build | {build} |", f"| Poll | {poll} |",
    ]
    if publish == "success" and publish_changed in {"true", "false"} and HEX_SHA.fullmatch(publish_sha):
        change = "updated" if publish_changed == "true" else "no changes"
        publish_result = f"success — {change}, verified `{publish_sha}`"
    elif publish == "skipped":
        publish_result = "skipped — remote state not published"
    else:
        publish_result = f"{publish} — remote state unverified"
    rows.extend([f"| Publish | {publish_result} |", ""])

    if poll_record is None or terminal is None:
        rows.extend(["_No valid POLL/RUN record pair was found; poll log details were not trusted._", ""])
        return "\n".join(rows)

    rows.extend([
        f"Run status: **{terminal.status}** · local state: **{terminal.local_state}** · "
        f"code: **{terminal.code}** · "
        f"boards: {terminal.boards} · duration: {terminal.duration_ms} ms", "",
        f"Board health: **{'degraded' if any(board.status != 'ok' for board in boards) else 'ok'}**", "",
        "| Board status | Count |", "|---|---:|",
    ])
    for status in STATUS_ORDER:
        rows.append(f"| {status} | {getattr(poll_record, status)} |")
    rows.extend([
        f"| **Total** | **{poll_record.boards}** |", "",
        "| Activity | Total |", "|---|---:|",
        f"| Open | {poll_record.open} |", f"| New | {poll_record.new} |",
        f"| Matched | {poll_record.matched} |", f"| Deferred | {poll_record.deferred} |",
        f"| Detail failed | {sum(board.detail_failed for board in boards)} |",
        f"| Retries | {sum(board.retries for board in boards)} |",
        f"| Caps | {sum(board.caps for board in boards)} |",
        f"| Fetch ms | {sum(board.fetch_ms for board in boards)} |",
        f"| Process ms | {sum(board.process_ms for board in boards)} |", "",
    ])

    warning_by_index: dict[int, list[Warning]] = {}
    run_warnings: list[Warning] = []
    for warning in warnings:
        if warning.scope == "board":
            warning_by_index.setdefault(warning.index, []).append(warning)
        else:
            run_warnings.append(warning)

    rows.extend(["### Run warnings", ""])
    if not run_warnings:
        rows.extend(["_None._", ""])
    else:
        rows.extend(["| Scope | Index | Step | Code | Count |", "|---|---:|---|---|---:|"])
        rows.extend(
            f"| {warning.scope} | {warning.index} | {warning.step} | {warning.code} | {warning.count} |"
            for warning in run_warnings
        )
        rows.append("")

    rows.extend([
        "### Boards", "",
        "| # | Company | Adapter | Status | Fetch | Warning | Open | New | Matched | Deferred | "
        "Detail failed | Retries | Caps | Fetch ms | Process ms |",
        "|---:|---|---|---|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|",
    ])
    for board in boards:
        board_warnings = warning_by_index.get(board.index, [])
        warning = (
            f"{board_warnings[0].step}/{board_warnings[0].code} count={board_warnings[0].count}"
            if board_warnings else "none"
        )
        rows.append(
            f"| {board.index} | {safe_text(board.company)} | {board.adapter} | {board.status} | "
            f"{board.fetch_status} | {warning} | {board.open} | {board.new} | {board.matched} | "
            f"{board.deferred} | {board.detail_failed} | {board.retries} | {board.caps} | "
            f"{board.fetch_ms} | {board.process_ms} |"
        )
    rows.extend([
        "",
        "_Privacy: this summary contains the complete validated FETCH/BOARD/WARN/POLL/RUN aggregates. "
        "Raw errors and per-job title, location, URL, and body data stay out of public Actions logs._",
        "",
    ])
    return "\n".join(rows)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--log", type=Path, required=True)
    parser.add_argument("--restore", required=True)
    parser.add_argument("--restore-mode", default="")
    parser.add_argument("--build", required=True)
    parser.add_argument("--poll", required=True)
    parser.add_argument("--publish", required=True)
    parser.add_argument("--publish-changed", default="")
    parser.add_argument("--publish-sha", default="")
    args = parser.parse_args()
    boards, warnings, poll_record, terminal = parse_log(args.log)
    summary = render(
        boards, warnings, poll_record, terminal, args.restore, args.restore_mode, args.build, args.poll,
        args.publish, args.publish_changed, args.publish_sha,
    )
    encoded_bytes = len((summary + "\n").encode("utf-8"))
    if encoded_bytes > MAX_STEP_SUMMARY_BYTES:
        raise SystemExit(
            f"summary is {encoded_bytes} bytes, exceeding the {MAX_STEP_SUMMARY_BYTES}-byte step-summary limit; "
            "refusing to truncate"
        )
    print(summary)


if __name__ == "__main__":
    main()
