"""`help()` — the discovery mechanism Fabric documents and this shim lacked.

Fabric's `fs` page OPENS by telling you to run `notebookutils.fs.help()`, and
`help("methodName")` for one method. It is the first thing the documentation
asks a notebook author to do, and it was on no module here — the hand
transcription that built `notebookutils-reference.json` did not yield it,
because it is prose on the page rather than a row in the method table. The
vendored stubs carry it on every module, which is what a second source is for.

DERIVED FROM THE MODULE, NEVER A TABLE. The listing is built by introspecting
the module's own public functions and their docstrings, so it cannot describe a
method that does not exist or miss one that does. A hand-written help table
would be a second description of the same surface, and this repository keeps
finding that exact defect — a reference that says one thing while the code says
another, with nothing to notice.
"""
import inspect


def _public(module):
    """The module's own public functions, in the order a reader wants: as
    written, not alphabetically — related methods sit together in the source."""
    out = []
    for name in getattr(module, "__all__", None) or dir(module):
        if name.startswith("_"):
            continue
        value = getattr(module, name, None)
        if inspect.isfunction(value) and value.__module__ == module.__name__:
            out.append((name, value))
    out.sort(key=lambda pair: pair[1].__code__.co_firstlineno)
    return out


def _signature(name, fn):
    try:
        return f"{name}{inspect.signature(fn)}"
    except (TypeError, ValueError):  # pragma: no cover - builtins have none
        return f"{name}(...)"


def _first_line(fn):
    doc = inspect.getdoc(fn) or ""
    return doc.split("\n", 1)[0]


def help_string(module, method_name=None):
    """The module's methods, or one method's full documentation."""
    functions = dict(_public(module))
    if method_name:
        fn = functions.get(method_name)
        if fn is None:
            return (f"{module.__name__} has no method {method_name!r}. "
                    f"Available: {', '.join(sorted(functions))}")
        return _signature(method_name, fn) + "\n\n" + (inspect.getdoc(fn) or "")
    summary = (inspect.getdoc(module) or "").split("\n", 1)[0]
    # Every module here opens its docstring with its own name, so the heading
    # would otherwise read "notebookutils.fs — notebookutils.fs — file …".
    for prefix in (f"{module.__name__} — ", f"{module.__name__} - "):
        if summary.startswith(prefix):
            summary = summary[len(prefix):]
    lines = [f"{module.__name__} — {summary}" if summary else module.__name__, ""]
    for name, fn in _public(module):
        lines.append("    " + _signature(name, fn))
        first = _first_line(fn)
        if first:
            lines.append("        " + first)
    return "\n".join(lines)


def emit(module, method_name=None):
    """Print the help text. Returns None, as Fabric's does — it is an output
    call, and a caller wanting the string has `getHelpString`."""
    print(help_string(module, method_name))
