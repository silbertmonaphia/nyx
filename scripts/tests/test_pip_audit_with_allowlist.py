import importlib.util
import shlex
from datetime import date, timedelta
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parents[1] / "ci" / "pip_audit_with_allowlist.py"

_spec = importlib.util.spec_from_file_location("pip_audit_with_allowlist", SCRIPT)
paw = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(paw)

CLEAN_JSON = (
    '{"dependencies": [{"name": "fastapi", "version": "0.115.0", "vulns": []}]}'
)
VULN_JSON = (
    '{"dependencies": ['
    '{"name": "starlette", "version": "0.40.0", "vulns": ['
    '{"id": "PYSEC-2026-99", "fix_versions": ["0.41.0"], '
    '"aliases": ["CVE-2026-1234"]}]},'
    '{"name": "fastapi", "version": "0.115.0", "vulns": []}]}'
)
SKIP_JSON = (
    '{"dependencies": ['
    '{"name": "nyx-agent", "skip_reason": "Dependency not found on PyPI and '
    'could not be audited: nyx-agent (0.1.0)"},'
    '{"name": "fastapi", "version": "0.115.0", "vulns": []}]}'
)
SKIP_OTHER_JSON = (
    '{"dependencies": ['
    '{"name": "vendored-wheel", "skip_reason": "could not be audited"},'
    '{"name": "fastapi", "version": "0.115.0", "vulns": []}]}'
)
DUPLICATE_ADVISORY_JSON = (
    '{"dependencies": ['
    '{"name": "starlette", "version": "0.40.0", "vulns": ['
    '{"id": "PYSEC-2026-99", "fix_versions": ["0.41.0"], "aliases": []}]},'
    '{"name": "starlette", "version": "0.40.0", "vulns": ['
    '{"id": "PYSEC-2026-99", "fix_versions": ["0.41.0", "0.42.0"], "aliases": []}]}]}'
)


def _write_allowlist(root: Path, body: str, name: str | None = None) -> Path:
    path = root / name if name else root / "allowlist.toml"
    path.write_text(body)
    return path


def _write_fake_bin(root: Path, payload: str, code: int) -> Path:
    data_path = root / "audit.json"
    data_path.write_text(payload)
    bin_path = root / "fake-pip-audit"
    bin_path.write_text(
        "#!/bin/sh\n"
        f"cat {shlex.quote(str(data_path))}\n"
        f"exit {code}\n"
    )
    bin_path.chmod(0o755)
    return bin_path


def _entry(advisory_id: str, entry_date: str, rationale: str = "deferred, no fix upstream") -> str:
    return (
        "[[advisories]]\n"
        f'id = "{advisory_id}"\n'
        f'date = "{entry_date}"\n'
        f'rationale = "{rationale}"\n'
    )


def test_load_allowlist_roundtrip(tmp_path):
    path = _write_allowlist(tmp_path, _entry("PYSEC-2026-99", "2026-10-01"))
    allowlist = paw.load_allowlist(path)
    assert set(allowlist) == {"PYSEC-2026-99"}
    assert allowlist["PYSEC-2026-99"]["date"] == date(2026, 10, 1)
    assert "no fix upstream" in allowlist["PYSEC-2026-99"]["rationale"]


def test_load_allowlist_accepts_native_toml_dates(tmp_path):
    path = _write_allowlist(
        tmp_path,
        "[[advisories]]\nid = \"GHSA-1\"\ndate = 2026-09-15\nrationale = \"x\"\n",
    )
    assert paw.load_allowlist(path)["GHSA-1"]["date"] == date(2026, 9, 15)


def test_load_allowlist_empty_file_yields_no_entries(tmp_path):
    path = _write_allowlist(tmp_path, "")
    assert paw.load_allowlist(path) == {}


@pytest.mark.parametrize(
    "body",
    [
        "[[advisories]]\nid = \"GHSA-1\"\nrationale = \"x\"\n",
        "[[advisories]]\nid = \"GHSA-1\"\ndate = \"2026-10-01\"\n",
        "[[advisories]]\nid = \"\"\ndate = \"2026-10-01\"\nrationale = \"x\"\n",
        "[[advisories]]\nid = \"GHSA-1\"\ndate = \"01-10-2026\"\nrationale = \"x\"\n",
        "[[advisories]]\nid = \"GHSA-1\"\ndate = \"2026-10-01\"\nrationale = \"  \"\n",
        "advisories = \"nope\"\n",
    ],
)
def test_load_allowlist_rejects_malformed_entries(tmp_path, body):
    path = _write_allowlist(tmp_path, body)
    with pytest.raises(ValueError):
        paw.load_allowlist(path)


def test_load_allowlist_rejects_duplicate_ids(tmp_path):
    path = _write_allowlist(
        tmp_path, _entry("GHSA-1", "2026-10-01") + _entry("GHSA-1", "2026-10-02")
    )
    with pytest.raises(ValueError, match="duplicate"):
        paw.load_allowlist(path)


def test_parse_audit_json_flattens_to_advisory_level():
    findings, skipped = paw.parse_audit_json(VULN_JSON)
    assert [f["id"] for f in findings] == ["PYSEC-2026-99"]
    assert findings[0]["package"] == "starlette"
    assert findings[0]["version"] == "0.40.0"
    assert findings[0]["fix_versions"] == ["0.41.0"]
    assert findings[0]["aliases"] == ["CVE-2026-1234"]
    assert skipped == []


def test_parse_audit_json_clean_report_has_no_findings():
    assert paw.parse_audit_json(CLEAN_JSON) == ([], [])


def test_parse_audit_json_separates_skipped_dependencies():
    findings, skipped = paw.parse_audit_json(SKIP_JSON)
    assert findings == []
    assert skipped == [
        {
            "name": "nyx-agent",
            "reason": "Dependency not found on PyPI and could not be audited: nyx-agent (0.1.0)",
        }
    ]


@pytest.mark.parametrize(
    "payload",
    [
        "[]",
        "{}",
        '{"dependencies": "one"}',
        '{"dependencies": ["nope"]}',
        '{"dependencies": [{"name": "x", "vulns": [{}]}]}',
        '{"dependencies": [{"name": "x", "vulns": "all"}]}',
    ],
)
def test_parse_audit_json_rejects_schema_violations(payload):
    with pytest.raises(ValueError):
        paw.parse_audit_json(payload)


def test_cli_clean_report_passes(tmp_path, capsys):
    bin_path = _write_fake_bin(tmp_path, CLEAN_JSON, code=0)
    allowlist = _write_allowlist(tmp_path, "")
    rc = paw.main(
        [
            "--root", str(tmp_path),
            "--allowlist", str(allowlist),
            "--pip-audit-bin", str(bin_path),
        ]
    )
    assert rc == 0
    assert "ok" in capsys.readouterr().out


def test_cli_unallowlisted_advisory_fails_even_though_pip_audit_exit_is_one(tmp_path, capsys):
    bin_path = _write_fake_bin(tmp_path, VULN_JSON, code=1)
    allowlist = _write_allowlist(tmp_path, "")
    rc = paw.main(
        [
            "--root", str(tmp_path),
            "--allowlist", str(allowlist),
            "--pip-audit-bin", str(bin_path),
        ]
    )
    assert rc == 1
    out = capsys.readouterr()
    assert "FAIL PYSEC-2026-99" in out.out
    assert "not allowlisted" in out.out
    assert "failing advisory" in out.err


def test_cli_fresh_entry_passes_and_prints_id_date_rationale(tmp_path, capsys):
    bin_path = _write_fake_bin(tmp_path, VULN_JSON, code=1)
    allowlist = _write_allowlist(
        tmp_path,
        _entry("PYSEC-2026-99", date.today().isoformat(), rationale="test-only transitive dep"),
    )
    rc = paw.main(
        [
            "--root", str(tmp_path),
            "--allowlist", str(allowlist),
            "--pip-audit-bin", str(bin_path),
        ]
    )
    assert rc == 0
    out = capsys.readouterr().out
    assert "ALLOW PYSEC-2026-99" in out
    assert date.today().isoformat() in out
    assert "test-only transitive dep" in out


def test_cli_expired_entry_fails_with_notice(tmp_path, capsys):
    bin_path = _write_fake_bin(tmp_path, VULN_JSON, code=1)
    stale = (date.today() - timedelta(days=91)).isoformat()
    allowlist = _write_allowlist(tmp_path, _entry("PYSEC-2026-99", stale))
    rc = paw.main(
        [
            "--root", str(tmp_path),
            "--allowlist", str(allowlist),
            "--pip-audit-bin", str(bin_path),
        ]
    )
    assert rc == 1
    out = capsys.readouterr().out
    assert "expired" in out
    assert stale in out


def test_cli_entry_exactly_ninety_days_old_is_still_fresh(tmp_path, capsys):
    bin_path = _write_fake_bin(tmp_path, VULN_JSON, code=1)
    edge = (date.today() - timedelta(days=90)).isoformat()
    allowlist = _write_allowlist(tmp_path, _entry("PYSEC-2026-99", edge))
    rc = paw.main(
        [
            "--root", str(tmp_path),
            "--allowlist", str(allowlist),
            "--pip-audit-bin", str(bin_path),
        ]
    )
    assert rc == 0


def test_cli_missing_allowlist_file_exits_two(tmp_path, capsys):
    bin_path = _write_fake_bin(tmp_path, CLEAN_JSON, code=0)
    rc = paw.main(
        [
            "--root", str(tmp_path),
            "--allowlist", str(tmp_path / "nope.toml"),
            "--pip-audit-bin", str(bin_path),
        ]
    )
    assert rc == 2
    assert "cannot load allowlist" in capsys.readouterr().err


def test_cli_malformed_toml_exits_two(tmp_path, capsys):
    bin_path = _write_fake_bin(tmp_path, CLEAN_JSON, code=0)
    allowlist = _write_allowlist(tmp_path, "[[advisories]\nid = \"GHSA-1\"\n")
    rc = paw.main(
        [
            "--root", str(tmp_path),
            "--allowlist", str(allowlist),
            "--pip-audit-bin", str(bin_path),
        ]
    )
    assert rc == 2
    assert "cannot load allowlist" in capsys.readouterr().err


def test_cli_unparseable_output_exits_two(tmp_path, capsys):
    bin_path = _write_fake_bin(tmp_path, "Traceback (most recent call last):", code=1)
    allowlist = _write_allowlist(tmp_path, "")
    rc = paw.main(
        [
            "--root", str(tmp_path),
            "--allowlist", str(allowlist),
            "--pip-audit-bin", str(bin_path),
        ]
    )
    assert rc == 2
    assert "unparseable" in capsys.readouterr().err


def test_cli_schema_violating_output_exits_two(tmp_path, capsys):
    bin_path = _write_fake_bin(tmp_path, '{"unexpected": true}', code=0)
    allowlist = _write_allowlist(tmp_path, "")
    rc = paw.main(
        [
            "--root", str(tmp_path),
            "--allowlist", str(allowlist),
            "--pip-audit-bin", str(bin_path),
        ]
    )
    assert rc == 2
    assert "schema" in capsys.readouterr().err


def test_cli_missing_binary_exits_two(tmp_path, capsys):
    allowlist = _write_allowlist(tmp_path, "")
    rc = paw.main(
        [
            "--root", str(tmp_path),
            "--allowlist", str(allowlist),
            "--pip-audit-bin", str(tmp_path / "does-not-exist"),
        ]
    )
    assert rc == 2
    assert "not found" in capsys.readouterr().err


def test_cli_resolves_allowlist_under_root_by_default(tmp_path, capsys):
    bin_path = _write_fake_bin(tmp_path, CLEAN_JSON, code=0)
    default_path = tmp_path / "backend" / "agent_py" / ".pip-audit-allowlist.toml"
    default_path.parent.mkdir(parents=True)
    default_path.write_text("")
    rc = paw.main(["--root", str(tmp_path), "--pip-audit-bin", str(bin_path)])
    assert rc == 0
    assert "ok" in capsys.readouterr().out


def test_load_allowlist_rejects_future_dates(tmp_path):
    future = (date.today() + timedelta(days=400)).isoformat()
    path = _write_allowlist(tmp_path, _entry("GHSA-1", future))
    with pytest.raises(ValueError, match="future"):
        paw.load_allowlist(path)


def test_cli_self_package_skip_is_not_a_failure(tmp_path, capsys):
    bin_path = _write_fake_bin(tmp_path, SKIP_JSON, code=1)
    allowlist = _write_allowlist(tmp_path, "")
    rc = paw.main(
        [
            "--root", str(tmp_path),
            "--allowlist", str(allowlist),
            "--pip-audit-bin", str(bin_path),
            "--self-package", "nyx-agent",
        ]
    )
    assert rc == 0
    out = capsys.readouterr().out
    assert "SKIP nyx-agent" in out
    assert "the project's own distribution" in out
    assert "0 failing" in out


def test_cli_unknown_skip_fails_the_gate(tmp_path, capsys):
    bin_path = _write_fake_bin(tmp_path, SKIP_OTHER_JSON, code=1)
    allowlist = _write_allowlist(tmp_path, "")
    rc = paw.main(
        [
            "--root", str(tmp_path),
            "--allowlist", str(allowlist),
            "--pip-audit-bin", str(bin_path),
            "--self-package", "nyx-agent",
        ]
    )
    assert rc == 1
    captured = capsys.readouterr()
    assert "FAIL vendored-wheel" in captured.out
    assert "unauditable" in captured.err


def test_cli_self_package_defaults_to_pyproject_next_to_allowlist(tmp_path, capsys):
    bin_path = _write_fake_bin(tmp_path, SKIP_JSON, code=1)
    allowlist = _write_allowlist(tmp_path, "")
    (allowlist.parent / "pyproject.toml").write_text(
        '[project]\nname = "Nyx_Agent"\nversion = "0.1.0"\n'
    )
    rc = paw.main(
        [
            "--root", str(tmp_path),
            "--allowlist", str(allowlist),
            "--pip-audit-bin", str(bin_path),
        ]
    )
    assert rc == 0
    assert "SKIP nyx-agent" in capsys.readouterr().out


def test_cli_skips_fail_when_no_self_package_can_be_resolved(tmp_path, capsys):
    bin_path = _write_fake_bin(tmp_path, SKIP_JSON, code=1)
    allowlist = _write_allowlist(tmp_path, "")
    rc = paw.main(
        [
            "--root", str(tmp_path),
            "--allowlist", str(allowlist),
            "--pip-audit-bin", str(bin_path),
        ]
    )
    assert rc == 1
    assert "FAIL nyx-agent" in capsys.readouterr().out


def test_cli_warns_on_unmatched_allowlist_entry(tmp_path, capsys):
    bin_path = _write_fake_bin(tmp_path, CLEAN_JSON, code=0)
    allowlist = _write_allowlist(tmp_path, _entry("PYSEC-2026-404", "2026-10-01"))
    rc = paw.main(
        [
            "--root", str(tmp_path),
            "--allowlist", str(allowlist),
            "--pip-audit-bin", str(bin_path),
        ]
    )
    assert rc == 0
    out = capsys.readouterr().out
    assert "WARN PYSEC-2026-404" in out
    assert "no longer reported" in out


def test_cli_duplicate_advisory_ids_count_once(tmp_path, capsys):
    bin_path = _write_fake_bin(tmp_path, DUPLICATE_ADVISORY_JSON, code=1)
    allowlist = _write_allowlist(tmp_path, "")
    rc = paw.main(
        [
            "--root", str(tmp_path),
            "--allowlist", str(allowlist),
            "--pip-audit-bin", str(bin_path),
        ]
    )
    assert rc == 1
    assert "1 failing advisory(ies)" in capsys.readouterr().err


def test_cli_unexpected_exit_code_exits_two(tmp_path, capsys):
    bin_path = _write_fake_bin(tmp_path, CLEAN_JSON, code=2)
    allowlist = _write_allowlist(tmp_path, "")
    rc = paw.main(
        [
            "--root", str(tmp_path),
            "--allowlist", str(allowlist),
            "--pip-audit-bin", str(bin_path),
        ]
    )
    assert rc == 2
    assert "outside its 0" in capsys.readouterr().err
