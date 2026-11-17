import importlib.util
import json
import shlex
from datetime import date, timedelta
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parents[1] / "ci" / "npm_audit_with_allowlist.py"

_spec = importlib.util.spec_from_file_location("npm_audit_with_allowlist", SCRIPT)
naw = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(naw)

PICOMATCH_GHSA = "GHSA-fc5q-39xc-vjpf"
ADVISORY = {
    "title": "picomatch vulnerable to ReDoS through single-branch repeated extglobs",
    "url": f"https://github.com/advisories/{PICOMATCH_GHSA}",
    "severity": "high",
    "range": "<=4.0.7",
}


def _report(vulnerabilities: dict) -> str:
    return json.dumps(
        {
            "auditReportVersion": 2,
            "vulnerabilities": vulnerabilities,
            "metadata": {"vulnerabilities": {}},
        }
    )


def _write_allowlist(root: Path, body: str, name: str | None = None) -> Path:
    path = root / name if name else root / "allowlist.toml"
    path.write_text(body)
    return path


def _write_fake_bin(root: Path, payload: str, code: int) -> Path:
    data_path = root / "audit.json"
    data_path.write_text(payload)
    bin_path = root / "fake-npm"
    bin_path.write_text(
        "#!/bin/sh\n" f"cat {shlex.quote(str(data_path))}\n" f"exit {code}\n"
    )
    bin_path.chmod(0o755)
    return bin_path


def _run(root: Path, allowlist: Path, bin_path: Path) -> int:
    return naw.main(
        [
            "--root",
            str(root),
            "--allowlist",
            str(allowlist),
            "--npm-bin",
            str(bin_path),
        ]
    )


def _entry(entry_date: date) -> str:
    return (
        "[[advisories]]\n"
        f'id = "{PICOMATCH_GHSA}"\n'
        f'date = "{entry_date.isoformat()}"\n'
        'rationale = "no patched release yet"\n'
    )


@pytest.fixture
def workdir(tmp_path: Path) -> Path:
    (tmp_path / "frontend").mkdir()
    return tmp_path


def test_clean_report_passes(workdir):
    allowlist = _write_allowlist(workdir, "")
    fake = _write_fake_bin(workdir, _report({}), 0)
    assert _run(workdir, allowlist, fake) == 0


def test_unallowlisted_high_fails(workdir):
    allowlist = _write_allowlist(workdir, "")
    fake = _write_fake_bin(
        workdir, _report({"picomatch": {"severity": "high", "via": [ADVISORY]}}), 1
    )
    assert _run(workdir, allowlist, fake) == 1


def test_allowlisted_advisory_passes(workdir):
    allowlist = _write_allowlist(workdir, _entry(date.today()))
    fake = _write_fake_bin(
        workdir, _report({"picomatch": {"severity": "high", "via": [ADVISORY]}}), 1
    )
    assert _run(workdir, allowlist, fake) == 0


def test_propagated_packages_are_covered(workdir):
    allowlist = _write_allowlist(workdir, _entry(date.today()))
    vulnerabilities = {
        "picomatch": {"severity": "high", "via": [ADVISORY]},
        "tinyglobby": {"severity": "high", "via": ["picomatch"]},
        "lint-staged": {"severity": "high", "via": ["picomatch"]},
        "minimatch": {
            "severity": "moderate",
            "via": [
                {
                    "title": "minimatch DoS",
                    "url": "https://github.com/advisories/GHSA-g3hv-75wg-m249",
                    "severity": "moderate",
                }
            ],
        },
        "typescript-estree": {
            "severity": "high",
            "via": ["minimatch", "tinyglobby"],
        },
    }
    fake = _write_fake_bin(workdir, _report(vulnerabilities), 1)
    assert _run(workdir, allowlist, fake) == 0


def test_expired_entry_fails(workdir):
    allowlist = _write_allowlist(
        workdir, _entry(date.today() - timedelta(days=naw.MAX_AGE_DAYS + 1))
    )
    fake = _write_fake_bin(
        workdir, _report({"picomatch": {"severity": "high", "via": [ADVISORY]}}), 1
    )
    assert _run(workdir, allowlist, fake) == 1


def test_future_date_is_a_load_error(workdir):
    allowlist = _write_allowlist(
        workdir, _entry(date.today() + timedelta(days=5))
    )
    fake = _write_fake_bin(workdir, _report({}), 0)
    assert _run(workdir, allowlist, fake) == 2


def test_stale_entry_warns_and_passes(workdir, capsys):
    allowlist = _write_allowlist(workdir, _entry(date.today()))
    fake = _write_fake_bin(workdir, _report({}), 0)
    assert _run(workdir, allowlist, fake) == 0
    assert "WARN" in capsys.readouterr().out


def test_moderate_only_report_passes(workdir):
    allowlist = _write_allowlist(workdir, "")
    vulnerabilities = {
        "minimatch": {
            "severity": "moderate",
            "via": [
                {
                    "title": "minimatch DoS",
                    "url": "https://github.com/advisories/GHSA-g3hv-75wg-m249",
                    "severity": "moderate",
                }
            ],
        }
    }
    fake = _write_fake_bin(workdir, _report(vulnerabilities), 1)
    assert _run(workdir, allowlist, fake) == 0


def test_npm_error_exit_is_wrapper_error(workdir):
    allowlist = _write_allowlist(workdir, "")
    fake = _write_fake_bin(workdir, "", 2)
    assert _run(workdir, allowlist, fake) == 2


def test_missing_allowlist_is_wrapper_error(workdir):
    fake = _write_fake_bin(workdir, _report({}), 0)
    assert _run(workdir, workdir / "absent.toml", fake) == 2


def test_duplicate_entry_rejected(workdir):
    allowlist = _write_allowlist(
        workdir, _entry(date.today()) + _entry(date.today()), name="dup.toml"
    )
    fake = _write_fake_bin(workdir, _report({}), 0)
    assert _run(workdir, allowlist, fake) == 2


def test_non_ghsa_id_rejected(workdir):
    allowlist = _write_allowlist(
        workdir,
        "[[advisories]]\n"
        'id = "CVE-2026-1234"\n'
        'date = "2026-11-18"\n'
        'rationale = "wrong id namespace"\n',
    )
    fake = _write_fake_bin(workdir, _report({}), 0)
    assert _run(workdir, allowlist, fake) == 2
