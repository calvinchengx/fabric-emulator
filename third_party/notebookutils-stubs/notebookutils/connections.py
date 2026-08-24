from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Dict, Optional


@dataclass
class DatasourceCredential:
    """A lightweight placeholder for Fabric/Synapse DatasourceCredential."""

    connectionId: str = ""
    artifactId: str = ""
    properties: Dict[str, Any] = field(default_factory=dict)


_dummyStr: str = ""


def getCredential(connectionId: str, artifactId: str = "") -> Optional[DatasourceCredential]:
    """Return a dummy DatasourceCredential.

    This project mirrors runtime APIs for local development; no real credential lookup is performed.
    """

    return None


def getHelpString(funcName: str = "", namespace: str = "") -> str:
    return _dummyStr
