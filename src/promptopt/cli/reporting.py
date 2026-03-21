"""Structured output helpers for CLI commands."""

from __future__ import annotations

import html
import json
from dataclasses import asdict, is_dataclass
from enum import Enum
from pathlib import Path


def to_jsonable(value: object) -> object:
    """Convert common Python objects into JSON-safe values."""
    if is_dataclass(value) and not isinstance(value, type):
        return to_jsonable(asdict(value))
    if isinstance(value, Enum):
        return value.value
    if isinstance(value, Path):
        return str(value)
    if isinstance(value, dict):
        return {str(key): to_jsonable(item) for key, item in value.items()}
    if isinstance(value, list):
        return [to_jsonable(item) for item in value]
    if isinstance(value, tuple):
        return [to_jsonable(item) for item in value]
    return value


def to_json_text(payload: dict[str, object]) -> str:
    """Serialize a payload as pretty JSON."""
    return json.dumps(to_jsonable(payload), ensure_ascii=False, indent=2, sort_keys=True)


def write_report_file(
    *,
    title: str,
    payload: dict[str, object],
    destination: Path,
    report_format: str,
) -> None:
    """Write a markdown or HTML report file."""
    destination.parent.mkdir(parents=True, exist_ok=True)
    if report_format == "markdown":
        destination.write_text(_to_markdown(title, payload), encoding="utf-8")
        return
    if report_format == "html":
        destination.write_text(_to_html(title, payload), encoding="utf-8")
        return
    raise ValueError(f"Unsupported report format: {report_format}")


def _to_markdown(title: str, payload: dict[str, object]) -> str:
    lines = [f"# {title}", ""]
    for key, value in payload.items():
        lines.extend(_markdown_block(key, value, level=2))
    return "\n".join(lines).rstrip() + "\n"


def _markdown_block(name: str, value: object, *, level: int) -> list[str]:
    heading = "#" * level
    lines = [f"{heading} {name}", ""]
    lines.extend(_markdown_value(value, level=level))
    lines.append("")
    return lines


def _markdown_value(value: object, *, level: int) -> list[str]:
    if isinstance(value, dict):
        lines: list[str] = []
        for key, item in value.items():
            if isinstance(item, (dict, list)):
                lines.extend(_markdown_block(str(key), item, level=min(level + 1, 6)))
            else:
                lines.append(f"- **{key}**: {item}")
        return lines or ["- 无"]
    if isinstance(value, list):
        if not value:
            return ["- 无"]
        if all(not isinstance(item, (dict, list)) for item in value):
            return [f"- {item}" for item in value]
        lines = []
        for index, item in enumerate(value, start=1):
            if isinstance(item, (dict, list)):
                lines.extend(_markdown_block(f"Item {index}", item, level=min(level + 1, 6)))
            else:
                lines.append(f"- {item}")
        return lines
    return [f"- {value}"]


def _to_html(title: str, payload: dict[str, object]) -> str:
    body = [f"<h1>{html.escape(title)}</h1>"]
    for key, value in payload.items():
        body.extend(_html_block(key, value, level=2))
    return (
        "<!DOCTYPE html><html><head><meta charset='utf-8'>"
        f"<title>{html.escape(title)}</title>"
        "<style>body{font-family:Arial,sans-serif;max-width:1000px;margin:40px auto;padding:0 16px;}"
        "table{border-collapse:collapse;width:100%;margin:12px 0;}th,td{border:1px solid #ddd;padding:8px;text-align:left;}"
        "th{background:#f5f5f5;}code,pre{background:#f7f7f7;padding:8px;border-radius:6px;display:block;white-space:pre-wrap;}"
        "ul{padding-left:20px;}h1,h2,h3,h4,h5,h6{margin-top:28px;}</style></head>"
        f"<body>{''.join(body)}</body></html>"
    )


def _html_block(name: str, value: object, *, level: int) -> list[str]:
    heading_level = min(level, 6)
    lines = [f"<h{heading_level}>{html.escape(name)}</h{heading_level}>"]
    lines.extend(_html_value(value, level=level))
    return lines


def _html_value(value: object, *, level: int) -> list[str]:
    if isinstance(value, dict):
        if not value:
            return ["<p>无</p>"]
        rows = []
        complex_blocks: list[str] = []
        for key, item in value.items():
            if isinstance(item, (dict, list)):
                complex_blocks.extend(_html_block(str(key), item, level=level + 1))
            else:
                rows.append(
                    f"<tr><th>{html.escape(str(key))}</th><td>{html.escape(str(item))}</td></tr>"
                )
        result = []
        if rows:
            result.append(f"<table>{''.join(rows)}</table>")
        result.extend(complex_blocks)
        return result
    if isinstance(value, list):
        if not value:
            return ["<p>无</p>"]
        if all(not isinstance(item, (dict, list)) for item in value):
            items = "".join(f"<li>{html.escape(str(item))}</li>" for item in value)
            return [f"<ul>{items}</ul>"]
        blocks: list[str] = []
        for index, item in enumerate(value, start=1):
            blocks.extend(_html_block(f"Item {index}", item, level=level + 1))
        return blocks
    return [f"<p>{html.escape(str(value))}</p>"]
