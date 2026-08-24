def updateNBSEndpoint(endpoint):
    pass


def help(method_name=None):
    pass


def run(
    path, timeout_seconds=90, arguments={}, workspaceId=""
):  # Pay attention to the parameters!
    return ""


def exit(value):
    pass


def runMultiple(dag, config=None):
    pass


def validateDAG(dag):
    pass


def create(
    name,
    description="",
    content=None,
    defaultLakehouse="",
    defaultLakehouseWorkspace="",
    workspaceId="",
):
    pass


def delete(name, workspaceId=""):
    pass


def list(workspaceId="", maxResults=1000):
    pass


def get(name, workspaceId=""):
    pass


def update(name, newName, description="", workspaceId=""):
    pass


def updateDefinition(
    name,
    content=None,
    defaultLakehouse="",
    defaultLakehouseWorkspace="",
    workspaceId="",
    environmentId="",
    environmentWorkspaceId="",
):
    pass


def getDefinition(name, workspaceId="", format="ipynb"):
    pass
