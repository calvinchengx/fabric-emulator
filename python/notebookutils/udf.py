"""notebookutils.udf — call User Data Functions from a notebook.

One documented method, `getFunctions`, and it returns an object rather than a
value: each function in the UDF item becomes a callable attribute on it, plus
`functionDetails` and `itemDetails` for inspection.

THE RETURNED OBJECT IS THE CONTRACT, not just the call. Fabric's own examples
read `myFunctions.functionDetails` to check a function exists before invoking
it, and call `myFunctions.someName(...)` by attribute — so a shim that returned
a plain dict would satisfy nobody, and one that returned a bare callable would
break the `functionDetails` guard those examples recommend.
"""
import json

from . import credentials
from ._config import config
from ._http import request


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
    ws = workspaceId or config().workspace_id
    if not ws:
        raise RuntimeError("no workspace: pass workspaceId or set NOTEBOOKUTILS_WORKSPACE_ID")
    return ws


def getFunctions(udf, workspaceId=""):  # noqa: N802,N803 - documented spelling
    """Retrieve every function from a UDF item, by artifact id or name."""
    cfg = config()
    ws = _ws(workspaceId)
    token = credentials.getToken("fabric")
    base = f"{cfg.fabric_url}/v1/workspaces/{ws}/userDataFunctions"

    item = None
    listed = request("GET", base, token=token)
    for candidate in (listed.get("value", []) if isinstance(listed, dict) else listed) or []:
        if candidate.get("displayName") == udf or candidate.get("id") == udf:
            item = candidate
            break
    if item is None:
        raise KeyError(f"no User Data Function item named {udf!r} in workspace {ws}")

    detail = request("GET", f"{base}/{item['id']}/functions", token=token)
    functions = (detail.get("value", detail) if isinstance(detail, dict) else detail) or []

    def invoke(name, args, kwargs):
        # Positional arguments are matched to the declared parameter order —
        # Fabric documents both spellings, and Scala/R can only pass
        # positional, so the item's own signature is what resolves them.
        payload = dict(kwargs)
        if args:
            declared = next((f.get("Parameters") or [] for f in functions
                             if f.get("Name") == name), [])
            # strict=False on purpose: the page documents that a parameter
            # with a default may be omitted, so fewer args than declared is a
            # legal call, not a length mismatch to raise on.
            for value, spec in zip(args, declared, strict=False):
                payload[spec.get("Name") if isinstance(spec, dict) else spec] = value
        return request("POST", f"{base}/{item['id']}/functions/{name}/invoke",
                       token=token, body={"parameters": payload})

    return UDF({"Id": item["id"], "Name": item.get("displayName"),
                "WorkspaceId": ws, "CapacityId": item.get("capacityId", "")},
               functions if isinstance(functions, list) else json.loads(json.dumps(functions)),
               invoke)
