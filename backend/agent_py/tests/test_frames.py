"""FrameLog ring + cursor semantics (agent-stream-frame-replay task 1.1).

Pure in-process: no Redis, no DB, no network. Pins the cursor contract
the re-attach endpoint and the SPA ladder both depend on — append/trim,
strictly-after replay, and the three unservable-cursor shapes (malformed,
rolled past, from the future) that must all report truncation instead of
replaying a span with holes.
"""

from __future__ import annotations

from agent.frames import TRUNCATED, FrameLog, parse_cursor


def test_append_assigns_monotonic_sequences_and_trims():
    log = FrameLog(maxlen=3)
    seqs = [log.append(f"f{i}".encode())[0] for i in range(5)]
    assert seqs == [1, 2, 3, 4, 5]
    assert log.watermark() == 5
    assert log.oldest_seq() == 3


def test_append_stamps_the_id_into_the_finished_frame():
    log = FrameLog(maxlen=10)
    seq, frame = log.append(b"event: delta\ndata: {}\n\n")
    assert seq == 1
    assert frame == b"id: 1\nevent: delta\ndata: {}\n\n"


def test_since_returns_frames_strictly_after_the_cursor():
    log = FrameLog(maxlen=10)
    for i in range(4):
        log.append(f"f{i}".encode())

    frames = log.since(2, upto=log.watermark())
    assert frames == [b"id: 3\nf2", b"id: 4\nf3"]


def test_since_with_nothing_missed_is_empty():
    log = FrameLog(maxlen=10)
    last = log.append(b"only")[0]
    assert log.since(last, upto=log.watermark()) == []


def test_since_below_oldest_is_truncated():
    log = FrameLog(maxlen=3)
    for i in range(5):
        log.append(f"f{i}".encode())
    assert log.oldest_seq() == 3
    assert log.since(1, upto=log.watermark()) is TRUNCATED


def test_since_at_the_frame_just_below_oldest_is_servable():
    log = FrameLog(maxlen=3)
    for i in range(5):
        log.append(f"f{i}".encode())
    assert log.oldest_seq() == 3
    frames = log.since(2, upto=log.watermark())
    assert frames == [b"id: 3\nf2", b"id: 4\nf3", b"id: 5\nf4"]


def test_since_with_cursor_zero_on_an_untrimmed_log_replays_everything():
    log = FrameLog(maxlen=10)
    log.append(b"f1")
    log.append(b"f2")
    assert log.since(0, upto=log.watermark()) == [b"id: 1\nf1", b"id: 2\nf2"]


def test_since_above_watermark_is_truncated():
    log = FrameLog(maxlen=10)
    log.append(b"f0")
    assert log.since(9, upto=log.watermark()) is TRUNCATED


def test_since_on_an_empty_log_yields_empty_for_cursor_zero():
    log = FrameLog(maxlen=10)
    assert log.since(0, upto=log.watermark()) == []


def test_since_with_absent_cursor_is_truncated():
    log = FrameLog(maxlen=10)
    log.append(b"f0")
    assert log.since(None, upto=log.watermark()) is TRUNCATED


def test_parse_cursor_accepts_decimal_text_and_rejects_the_rest():
    assert parse_cursor(None) is None
    assert parse_cursor("42") == 42
    assert parse_cursor(7) == 7
    assert parse_cursor(" 12 ") == 12
    assert parse_cursor("") is None
    assert parse_cursor("abc") is None
    assert parse_cursor("-1") is None
    assert parse_cursor("4.2") is None
