def create(name, description="", definition=None, workspaceId=""):
    pass


def delete(name, workspaceId=""):
    pass


def list(workspaceId="", maxResults=1000):
    pass


def get(name="", workspaceId=""):
    pass


def update(name, newName, description="", workspaceId=""):
    pass


def getDefinition(name, workspaceId=""):
    pass


def updateDefinition(name, definition, workspaceId=""):
    pass


def listTables(lakehouse="", workspaceId="", maxResults=1000):
    pass


def loadTable(loadOption, table, lakehouse="", workspaceId=""):
    pass


def getWithProperties(name, workspaceId=""):
    pass


def help(method_name=None):
    pass
