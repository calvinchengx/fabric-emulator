"""`%run` — run another notebook's code IN THIS SESSION.

WHAT MAKES IT DIFFERENT FROM notebook.run, and the reason it needs its own
implementation at all: `notebookutils.notebook.run` starts a SEPARATE session,
runs a notebook to completion and hands back its exit value. `%run` splices the
other notebook's code into the CALLER's namespace, so the functions, variables
and imports it defines are afterwards available to the cell that ran it. That is
the whole point — it is how a Fabric notebook shares helpers without a package.

WHY A REWRITE AND NOT A PARSER FEATURE. `%run` is a LINE magic: it appears
inside an ordinary Python cell, possibly after other statements, so the cell's
language is still `python` and the cell parser correctly leaves it alone. But
`%run helpers` is a syntax error to Python — the line has to become a call
before `ast.parse` ever sees it. Rewriting the source is the only place that
can happen.

The rewrite is PURE and lives here so it can be tested without an engine, a
control plane or a notebook. The thing it rewrites to is bound per session by
the agent.
"""
import json
import re

# `%run name`, `%run "name with spaces"`, `%run name {"p": 1}`.
#
# ANCHORED TO THE LINE, with only leading whitespace allowed before it. A `%run`
# inside a string literal or a comment is not a magic, and matching one would
# rewrite the user's data — the same mistake the T-SQL dialect layer refuses to
# make when it declines to tokenize inside quotes.
_RUN = re.compile(
    r'^(?P<indent>[ \t]*)%run\s+(?P<target>"[^"]+"|\'[^\']+\'|\S+)'
    r'(?P<rest>.*)$')

HELPER = "__fabric_run_notebook__"


def expand(source, helper=HELPER):
    """Rewrite `%run` lines into calls to `helper`. Returns the new source.

    Everything else is returned byte-identical: a cell with no `%run` must come
    back exactly as written, because this runs on EVERY statement and a rewrite
    that reformatted innocent code would be a far worse bug than the feature is
    a win.
    """
    if "%run" not in source:
        return source
    out, changed = [], False
    for line in source.split("\n"):
        m = _RUN.match(line)
        if m is None:
            out.append(line)
            continue
        target = m.group("target").strip("\"'")
        rest = m.group("rest").strip()
        args = _arguments(rest)
        out.append(f"{m.group('indent')}{helper}({target!r}, {args!r})")
        changed = True
    return "\n".join(out) if changed else source


def _arguments(rest):
    """The optional argument object after the notebook name.

    Fabric documents `%run notebook {"param": value}`. Anything that is not
    JSON is passed through as the raw string rather than guessed at: a caller
    who wrote something this cannot parse should see their own text in the
    error, not a silently empty parameter set.
    """
    if not rest:
        return {}
    try:
        parsed = json.loads(rest)
    except ValueError:
        return {"__unparsed__": rest}
    return parsed if isinstance(parsed, dict) else {"__unparsed__": rest}


def code_cells(definition):
    """The executable Python of a notebook definition, in order.

    Accepts the `.ipynb` JSON `notebook.getDefinition` returns. Markdown cells
    are skipped; a non-Python cell is skipped too rather than spliced into a
    Python namespace, which would fail with a SyntaxError naming the CALLER's
    cell — the same defect internal/notebook/celllang.go exists to prevent, one
    layer down.
    """
    if isinstance(definition, str):
        try:
            definition = json.loads(definition)
        except ValueError:
            # Not JSON: treat it as a plain script, which is what the
            # `# CELL ****` source format is.
            return [definition]
    cells = []
    for cell in (definition or {}).get("cells", []):
        if cell.get("cell_type") != "code":
            continue
        lang = ((cell.get("metadata") or {}).get("language") or "python").lower()
        if lang not in ("", "python", "pyspark"):
            continue
        source = cell.get("source", "")
        cells.append("".join(source) if isinstance(source, list) else source)
    return cells


def make_runner(namespace, get_definition):
    """The callable `%run` rewrites to, bound to ONE session's namespace.

    It lives here rather than in agent.py because agent.py needs a live engine
    to import and is omitted from the coverage gate — and what this does is not
    plumbing. It decides where the referenced notebook's code lands, which IS
    the semantic that separates `%run` from `notebookutils.notebook.run`. A
    version that executed the code anywhere else would look identical until the
    caller tried to use what it defined.

    `get_definition(name)` returns the notebook definition; the agent passes
    `notebookutils.notebook.getDefinition`.
    """
    def run_notebook(name, arguments=None):
        arguments = arguments or {}
        if "__unparsed__" in arguments:
            raise ValueError(
                f"%run {name}: could not parse the arguments "
                f"{arguments['__unparsed__']!r} as a JSON object — Fabric "
                'documents `%run notebook {"param": value}`')
        # Parameters are bound BEFORE the code runs, the way a parameters cell
        # is: the referenced notebook reads them as ordinary globals.
        namespace.update(arguments)
        cells = code_cells(get_definition(name))
        if not cells:
            raise ValueError(
                f"%run {name}: the notebook has no runnable Python cells")
        for cell in cells:
            # EXECUTED IN `namespace`, not a copy and not a child session.
            # Nested `%run` is expanded too, so a helper notebook may itself
            # pull in another.
            exec(compile(expand(cell), f"<%run {name}>", "exec"), namespace)  # noqa: S102
        return None

    return run_notebook
