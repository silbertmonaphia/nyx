import importlib.util
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parents[1] / "check_stream_budget.py"

_spec = importlib.util.spec_from_file_location("check_stream_budget", SCRIPT)
csb = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(csb)


@pytest.mark.parametrize(
    ("text", "seconds"),
    [("300s", 300), ("10m", 600), ("1h", 3600), ("1d", 86400), ("1.5m", 90)],
)
def test_parse_duration(text, seconds):
    assert csb.parse_duration(text) == seconds


@pytest.mark.parametrize("text", ["10", "m10", "10 m", "", "10x"])
def test_parse_duration_rejects_unknown_forms(text):
    with pytest.raises(ValueError):
        csb.parse_duration(text)


@pytest.mark.parametrize(
    ("cap", "ttl", "passes"),
    [
        (600, 300, True),
        (420, 300, True),
        (419, 300, False),
        (300, 300, False),
        (600, 600, False),
    ],
)
def test_required_cap(cap, ttl, passes):
    assert (cap >= csb.required_cap(ttl)) is passes


def _write_sources(root: Path, cap_literal: str, ttl_literal: str) -> None:
    go = root / "backend/internal/platform/config/config.go"
    go.parent.mkdir(parents=True)
    go.write_text(
        "package config\n\nfunc setDefaults() {\n"
        "\tviper.SetDefault(\"LLM_MAX_TOKENS\", 1024)\n"
        f'\tviper.SetDefault("LLM_MAX_STREAM_DURATION", "{cap_literal}")\n'
        "}\n"
    )
    py = root / "backend/agent_py/agent/config.py"
    py.parent.mkdir(parents=True)
    py.write_text(
        "class Settings(BaseSettings):\n"
        f"    approval_ttl_seconds: int = {ttl_literal}\n"
    )


def test_read_defaults_reads_both_sources(tmp_path):
    _write_sources(tmp_path, "10m", "300")
    assert csb.read_defaults(tmp_path) == (600, 300)


def test_read_defaults_fails_closed_when_cap_default_moves(tmp_path):
    _write_sources(tmp_path, "10m", "300")
    (tmp_path / "backend/internal/platform/config/config.go").write_text(
        "package config\n"
    )
    with pytest.raises(ValueError, match="LLM_MAX_STREAM_DURATION"):
        csb.read_defaults(tmp_path)


def test_read_defaults_fails_closed_when_ttl_default_moves(tmp_path):
    _write_sources(tmp_path, "10m", "300")
    (tmp_path / "backend/agent_py/agent/config.py").write_text(
        "class Settings(BaseSettings):\n"
    )
    with pytest.raises(ValueError, match="approval_ttl_seconds"):
        csb.read_defaults(tmp_path)


def test_cli_passes_on_current_repo_defaults(tmp_path):
    _write_sources(tmp_path, "10m", "300")
    assert csb.main(["--root", str(tmp_path)]) == 0


def test_cli_fails_on_a_short_cap(tmp_path, capsys):
    _write_sources(tmp_path, "4m", "300")
    assert csb.main(["--root", str(tmp_path)]) == 1
    assert "needs >=" in capsys.readouterr().err


def test_cli_exits_two_when_sources_change_shape(tmp_path, capsys):
    _write_sources(tmp_path, "10m", "300")
    (tmp_path / "backend/agent_py/agent/config.py").write_text("class Settings:\n")
    assert csb.main(["--root", str(tmp_path)]) == 2
    assert "cannot read defaults" in capsys.readouterr().err
