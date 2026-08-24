"""notebookutils.variableLibrary — read a workspace Variable Library.

The notebook half of Fabric's own environment-abstraction mechanism. A
notebook names a variable; the workspace's ACTIVE VALUE SET decides its value,
so the same notebook yields different values in dev, test and prod without the
code changing. See docs/48-variable-libraries.md for the pipeline half and for
the definition format.

Unlike the pipeline surface, this API is fully documented, so the shape below
follows the reference rather than a capture:

    notebookutils.variableLibrary.getLibrary("sampleVL").test_int
    notebookutils.variableLibrary.get("$(/**/sampleVL/test_int)")

Three documented rules this honours rather than smooths over:

  * **Names are CASE-SENSITIVE here.** Note the asymmetry with pipelines, where
    Fabric documents the library name as *not* case sensitive. Same library,
    two consumers, two matching rules — so a name that resolves in a pipeline
    can legitimately fail in a notebook, and softening this would hide that.
  * **Same workspace only.** Cross-workspace access is not supported, so there
    is deliberately no workspaceId argument.
  * **Read-only.** Changes go through the UI or the REST API.

It is built entirely on the public item APIs — list, getDefinition, and the
item's `activeValueSetName` property — so it needs no emulator-specific route
and the same code path works against real Fabric.
"""
import base64
import json
import sys as _sys

from . import _help, _lro, credentials
from ._config import config, session_workspace_id
from ._http import request


class VariableLibraryError(Exception):
    """A library or variable could not be resolved."""


def _ws():
    ws = session_workspace_id(config().workspace_id)
    if not ws:
        raise VariableLibraryError(
            "no workspace: set NOTEBOOKUTILS_WORKSPACE_ID. Variable libraries "
            "resolve within the notebook's own workspace only."
        )
    return ws


def _tok():
    return credentials.getToken("fabric")


def _find_item(name):
    """The workspace's VariableLibrary with exactly this display name.

    Case-SENSITIVE, per the reference: "Variable and library names are
    case-sensitive. Use exact name matching when you reference variables."
    """
    base = f"{config().fabric_url}/v1/workspaces/{_ws()}"
    resp = request("GET", base + "/items?type=VariableLibrary", token=_tok())
    items = resp.get("value", []) if isinstance(resp, dict) else []
    for it in items:
        if it.get("displayName") == name:
            return it
    available = sorted(i.get("displayName", "") for i in items)
    raise VariableLibraryError(
        f"no variable library named {name!r} in this workspace "
        f"(names are case-sensitive; available: {available})"
    )


def _definition_parts(item_id):
    """getDefinition, handling BOTH documented outcomes.

    The API answers 200 with the body or 202 with an operation — the reference
    documents both and a real tenant uses the 202. A client that reads the 202
    body gets `null` and reports an empty definition rather than an error, so
    this follows the operation instead. The emulator can produce that shape too
    (FABRIC_FORCE_LRO), which is what lets this path be exercised locally.
    """
    base = f"{config().fabric_url}/v1/workspaces/{_ws()}"
    status, headers, payload = request(
        "POST", f"{base}/items/{item_id}/getDefinition", token=_tok(), raw=True
    )
    body = _lro.follow(status, headers, payload, what="getDefinition",
                       token=_tok(), send=request, error=VariableLibraryError)
    return body.get("definition", {}).get("parts", [])


def _active_value_set(item_id):
    base = f"{config().fabric_url}/v1/workspaces/{_ws()}"
    it = request("GET", f"{base}/items/{item_id}", token=_tok())
    return (it.get("properties") or {}).get("activeValueSetName", "")


def _decode(parts):
    out = {}
    for p in parts:
        payload = p.get("payload", "")
        try:
            out[p.get("path", "")] = base64.b64decode(payload)
        except Exception:  # noqa: BLE001 - a part we cannot decode is not one we need
            continue
    return out


def _resolve(files, active):
    """Merge the active value set over the declared defaults.

    A value set overrides a SUBSET; anything it omits keeps its default. An
    active set with no file resolves to the defaults, which is the ordinary
    case and not an error: Fabric's out-of-the-box active set is named
    "Default value set" and has no file under valueSets/.
    """
    variables = {}
    for path, raw in files.items():
        if path.rsplit("/", 1)[-1] == "variables.json":
            for v in json.loads(raw).get("variables", []):
                variables[v["name"]] = v.get("value")
    if not variables:
        raise VariableLibraryError("the library definition has no variables.json")
    for path, raw in files.items():
        segs = path.split("/")
        if len(segs) < 2 or segs[-2] != "valueSets":
            continue
        doc = json.loads(raw)
        if doc.get("name") != active:
            continue
        for o in doc.get("variableOverrides", []):
            if o.get("name") in variables:
                variables[o["name"]] = o.get("value")
    return variables


class VariableLibrary:
    """A resolved library. Variables are readable as attributes, by
    `getVariable(name)`, or with bracket syntax — the three forms the
    reference documents."""

    def __init__(self, name, variables):
        self._name = name
        self._variables = dict(variables)

    def getVariable(self, name):  # noqa: N802 - the documented spelling
        try:
            return self._variables[name]
        except KeyError:
            raise VariableLibraryError(
                f"{self._name!r} has no variable {name!r} "
                f"(names are case-sensitive; available: {sorted(self._variables)})"
            ) from None

    def __getitem__(self, name):
        return self.getVariable(name)

    def __getattr__(self, name):
        # Only reached for names that are not real attributes, so the private
        # fields above still resolve normally.
        if name.startswith("_"):
            raise AttributeError(name)
        return self.getVariable(name)

    def __contains__(self, name):
        return name in self._variables

    def __iter__(self):
        return iter(self._variables)

    def __repr__(self):
        return f"VariableLibrary({self._name!r}, variables={sorted(self._variables)})"

    def asDict(self):  # noqa: N802 - matches the surface's camelCase
        """Every resolved variable. Not in Microsoft's table; useful for
        debugging which value set actually won."""
        return dict(self._variables)


def getLibrary(variableLibraryName):  # noqa: N802 - the documented spelling
    """The library as an object whose variables are its properties."""
    item = _find_item(variableLibraryName)
    files = _decode(_definition_parts(item["id"]))
    variables = _resolve(files, _active_value_set(item["id"]))
    return VariableLibrary(variableLibraryName, variables)


def get(variableReference):
    """One variable by reference: `$(/**/libraryName/variableName)`.

    The `/**/` prefix is required by the reference, so a reference without it
    is refused rather than quietly accepted — accepting a shorter form here
    would let a notebook work locally and fail in Fabric.
    """
    ref = variableReference.strip() if isinstance(variableReference, str) else ""
    if not (ref.startswith("$(") and ref.endswith(")")):
        raise VariableLibraryError(
            f"{variableReference!r} is not a variable reference; "
            "the documented form is $(/**/libraryName/variableName)"
        )
    inner = ref[2:-1]
    if not inner.startswith("/**/"):
        raise VariableLibraryError(
            f"{variableReference!r} is missing the required '/**/' prefix; "
            "the documented form is $(/**/libraryName/variableName)"
        )
    segs = inner[len("/**/"):].split("/")
    if len(segs) != 2 or not all(segs):
        raise VariableLibraryError(
            f"{variableReference!r} does not name exactly one library and one variable; "
            "the documented form is $(/**/libraryName/variableName)"
        )
    library, variable = segs
    return getLibrary(library).getVariable(variable)


class FabricVariablesClient:
    """The shape a **user data function** receives for a variable-library
    connection, per the `fabric-user-data-functions` programming model:

        @udf.connection(argName="varLib", alias="<My Variable Library Alias>")
        @udf.function()
        def get_storage_path(dataset: str, varLib: fn.FabricVariablesClient) -> str:
            variables = varLib.getVariables()
            env = variables.get("ENV")

    `getVariables()` returns a mapping supporting both documented reads,
    `variables["ENV"]` and `variables.get("ENV")`.

    **This deliberately does NOT shadow `fabric.functions`.** Microsoft ships
    `fabric-user-data-functions` as a real, installable PyPI package, so
    providing a module of that name would override a package a user may have
    installed — a different situation from `notebookutils`, which Microsoft
    ships as an import-only stub outside the Fabric runtime and which this repo
    therefore makes work. The shape is matched so a function body ports with one
    import line; the namespace is not hijacked.

    The emulator has **no user-data-function runtime**, so this makes a
    function's BODY testable against a real library; it does not make the item
    type execute. `docs/48-variable-libraries.md` says which is which.
    """

    def __init__(self, variableLibraryName):
        self._name = variableLibraryName

    def getVariables(self):  # noqa: N802 - the documented spelling
        """Every variable in the library, resolved under the active value set."""
        return _Variables(getLibrary(self._name))

    def __repr__(self):
        return f"FabricVariablesClient({self._name!r})"


class _Variables:
    """A read-only mapping over a resolved library.

    `.get()` returns None for an unknown name rather than raising, because that
    is what the documented sample relies on — it calls `variables.get("ENV")`
    and branches on the value. Bracket access raises, as a mapping should.
    """

    def __init__(self, library):
        self._library = library

    def __getitem__(self, name):
        return self._library.getVariable(name)

    def get(self, name, default=None):
        try:
            return self._library.getVariable(name)
        except VariableLibraryError:
            return default

    def __contains__(self, name):
        return name in self._library

    def __iter__(self):
        return iter(self._library)

    def asDict(self):  # noqa: N802 - matches the surface's camelCase
        return self._library.asDict()

    def __repr__(self):
        return f"Variables({sorted(self._library)})"


def getHelpString(funcName="", namespace=""):  # noqa: N802,N803 - Microsoft's spelling
    """This module's help text as a string.

    No `help()` beside it, deliberately: Microsoft's stub gives this module
    `getHelpString` and no `help`, and inventing the missing half would be this
    shim's shape rather than Fabric's.
    """
    return _help.help_string(_sys.modules[__name__], funcName or None)
