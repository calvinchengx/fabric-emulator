__version__ = "1.6.3"

__all__ = [
    "data",
    "fs",
    "lakehouse",
    "notebook",
    "session",
    "runtime",
    "fabricClient",
    "credentials",
    "udf",
    "conf",
    "connections",
    "variableLibrary",
]
from . import *


def help(module_name=""):
    pass


def __getattr__(name):
    pass


nbResPath = ""
