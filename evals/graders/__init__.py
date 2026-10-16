"""Promptfoo python assertions for the Nyx agent golden set.

Each module here exposes `get_assert(output, context) -> {"pass", "score",
"reason"}` per the frozen grader contract. `output` is the provider's
transcript JSON; `context["vars"]` carries the case's `id`/`expect` block and
the rendered prompt. Modules are loaded by file path (promptfoo's python
assertion loader), which is why each one bootstraps its own directory onto
`sys.path` before importing `_common`.
"""
