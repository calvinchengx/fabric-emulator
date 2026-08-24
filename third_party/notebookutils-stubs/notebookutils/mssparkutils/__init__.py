__all__ = ["fs", "notebook", "env", "credentials", "runtime", "session", "lakehouse"]

# put import * statement here after __all__ variable initialized to
# make sure all sub-module from mssparkutils is imported to current namespace *mssparkutils*,
# so people can use the exposed interfaces like `mssparkutils.<interface>`.
# reference https://docs.python.org/3/tutorial/modules.html#importing-from-a-package
from notebookutils.mssparkutils import *


def help(module_name=""):
    pass


nbResPath = ""
