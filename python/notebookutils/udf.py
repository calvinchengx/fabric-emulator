"""notebookutils.udf — call User Data Functions from a notebook.

One documented method, `getFunctions`, and it returns an OBJECT rather than a
value: each function in the UDF item becomes a callable attribute on it, plus
`functionDetails` and `itemDetails` for inspection. Fabric's own examples read
`myFunctions.functionDetails` to check a function exists before invoking it,
and call `myFunctions.someName(...)` by attribute — so a shim returning a plain
dict would satisfy nobody, and one returning a bare callable would break the
guard those examples recommend.

WHERE THE FUNCTIONS COME FROM. A UDF item's definition is documented, and its
parts are the whole answer (rest/api/fabric/articles/item-management/
definitions/user-data-function-definition, read 2026-08-24):

    definition.json          functions[] — name, description
                             connectedDataSources[] — alias, artifactType
    resources/functions.json fabricFunctionParameters[] and
                             fabricFunctionReturnType, per function
    function_app.py          the Python implementing them

So this reads the item's real definition rather than asking for an invoke
endpoint that does not exist, and runs the function's real code.

THE DIVERGENCE, STATED. Fabric executes a UDF in its OWN isolated runtime,
with connection bindings (`fn.FabricLakehouseClient`, `fn.FabricSqlConnection`)
materialised by the platform. This runs `function_app.py` in the CALLER's
interpreter, and provides no connection objects — a function whose signature
takes one raises rather than receiving a stub that would fail later, further
from the cause. Functions that take plain values behave the same in both
places, which is most of what a notebook calls a UDF for.
"""
import base64
import json

from . import credentials
from ._config import config, session_workspace_id
from ._http import request

_DEFINITION = "definition.json"
_METADATA = "resources/functions.json"
_SCRIPT = "function_app.py"


class UDF:
    """Functions from one User Data Function item, callable by name."""

    def __init__(self, item, functions, invoke):
        self.itemDetails = item
        self.functionDetails = functions
        self._invoke = invoke
        self._names = {f.get("Name") for f in functions if isinstance(f, dict)}

    def __getattr__(self, name):
        # __getattr__ runs only when normal lookup fails, so it never shadows
        # itemDetails/functionDetails.
        names = object.__getattribute__(self, "_names")
        if name not in names:
            raise AttributeError(
                f"{name!r} is not a function in this UDF item; it has: "
                + ", ".join(sorted(n for n in names if n)))
        invoke = object.__getattribute__(self, "_invoke")

        def call(*args, **kwargs):
            return invoke(name, args, kwargs)

        call.__name__ = name
        return call

    def __repr__(self):
        return f"UDF({self.itemDetails.get('Name')!r}, {len(self._names)} function(s))"


def _ws(workspaceId):  # noqa: N803 - documented spelling
    ws = workspaceId or session_workspace_id(config().workspace_id)
    if not ws:
        raise RuntimeError("no workspace: pass workspaceId or set NOTEBOOKUTILS_WORKSPACE_ID")
    return ws


def _part(parts, path):
    for part in parts:
        if part.get("path") == path:
            payload = part.get("payload") or ""
            return base64.b64decode(payload).decode("utf-8", "replace")
    return None


def _fabric_functions_stub():
    """A stand-in for `fabric.functions`, so the item's own code imports.

    `function_app.py` opens with `import fabric.functions as fn` and decorates
    with `@udf.function()` / `@udf.connection(...)`. Those decorators are
    REGISTRATION, not behaviour — on Fabric they tell the platform what to
    publish. Returning the function unchanged is therefore a faithful stand-in,
    not a stub that fakes something.
    """
    import types

    mod = types.ModuleType("fabric.functions")

    class UserDataFunctions:
        def function(self, *_a, **_k):
            def wrap(fn):
                return fn
            return wrap

        def connection(self, *_a, **_k):
            def wrap(fn):
                return fn
            return wrap

    mod.UserDataFunctions = UserDataFunctions
    package = types.ModuleType("fabric")
    package.functions = mod
    return package, mod


def getFunctions(udf, workspaceId=""):  # noqa: N802,N803 - documented spelling
    """Retrieve every function from a UDF item, by artifact id or name."""
    cfg = config()
    ws = _ws(workspaceId)
    token = credentials.getToken("fabric")
    base = f"{cfg.fabric_url}/v1/workspaces/{ws}/userDataFunctions"

    listed = request("GET", base, token=token)
    candidates = (listed.get("value", []) if isinstance(listed, dict) else listed) or []
    item = next((c for c in candidates
                 if c.get("displayName") == udf or c.get("id") == udf), None)
    if item is None:
        raise KeyError(f"no User Data Function item named {udf!r} in workspace {ws}")

    got = request("POST", f"{base}/{item['id']}/getDefinition", token=token)
    parts = ((got.get("definition") or {}).get("parts")
             if isinstance(got, dict) else None) or []
    definition = json.loads(_part(parts, _DEFINITION) or "{}")
    metadata = json.loads(_part(parts, _METADATA) or "{}")
    script = _part(parts, _SCRIPT) or ""

    by_name = {m.get("name"): m for m in metadata.get("functionsMetadata", [])
               if isinstance(m, dict)}
    sources = {d.get("alias"): d for d in definition.get("connectedDataSources", [])
               if isinstance(d, dict)}

    details = []
    for fn_spec in definition.get("functions", []):
        name = fn_spec.get("name")
        meta = by_name.get(name, {})
        props = meta.get("fabricProperties", {}) or {}
        bound = [b.get("alias") for b in (meta.get("bindings") or [])
                 if isinstance(b, dict) and b.get("type") == "FabricItem"]
        details.append({
            "Name": name,
            "Description": fn_spec.get("description", ""),
            "Parameters": props.get("fabricFunctionParameters", []) or [],
            "FunctionReturnType": props.get("fabricFunctionReturnType", ""),
            "DataSourceConnections": [sources[a] for a in bound if a in sources],
        })

    def invoke(name, args, kwargs):
        namespace = _run_script(script)
        target = namespace.get(name)
        if not callable(target):
            raise AttributeError(
                f"{name!r} is declared in this item's definition but "
                f"{_SCRIPT} defines no such function")
        declared = next((d["Parameters"] for d in details if d["Name"] == name), [])
        payload = dict(kwargs)
        # Positional arguments are matched to the DECLARED order — Fabric
        # documents both spellings, and Scala/R can only pass positional, so
        # the item's own parameter list is what resolves them.
        # strict=False on purpose: a parameter with a default may be omitted,
        # so fewer args than declared is a legal call.
        for value, spec in zip(args, declared, strict=False):
            payload[spec.get("name") if isinstance(spec, dict) else spec] = value
        return target(**payload)

    return UDF({"Id": item["id"], "Name": item.get("displayName"),
                "WorkspaceId": ws, "CapacityId": item.get("capacityId", "")},
               details, invoke)


def _run_script(script):
    """Execute `function_app.py` and hand back what it defined."""
    import sys

    package, functions = _fabric_functions_stub()
    saved = {k: sys.modules.get(k) for k in ("fabric", "fabric.functions")}
    sys.modules["fabric"] = package
    sys.modules["fabric.functions"] = functions
    try:
        namespace = {"__name__": "function_app"}
        exec(compile(script, "function_app.py", "exec"), namespace)  # noqa: S102
        return namespace
    finally:
        for key, value in saved.items():
            if value is None:
                sys.modules.pop(key, None)
            else:
                sys.modules[key] = value
