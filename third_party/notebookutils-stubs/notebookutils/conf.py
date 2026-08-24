_CONF: dict[str, str] = {}


def set(key: str, value: str) -> None:
    """Set a notebookutils config key/value (in-memory dummy implementation)."""

    _CONF[str(key)] = "" if value is None else str(value)


def get(key: str, default: str = "") -> str:
    """Get a notebookutils config value (in-memory dummy implementation)."""

    return _CONF.get(str(key), default)
