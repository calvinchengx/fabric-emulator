def getToken(audience, name=""):
    pass


def getConnectionStringOrCreds(linkedService):
    pass


def getFullConnectionString(linkedService):
    pass


def getPropertiesAll(linkedService):
    pass


def getSPTokenWithCert(resource, authority, clientId, akvNameOrUri, certificateName):
    pass


def getSPTokenWithCertLS(
    resource, authority, clientId, akvLinkedService, certificateName
):
    pass


def getSecret(akvName, secret, linkedService=""):
    pass


def getSecretWithLS(linkedService, secret):
    pass


def putSecret(akvName, secretName, secretValue, linkedService=""):
    pass


def putSecretWithLS(linkedService, secretName, secretValue):
    pass


def isValidToken(token):
    return False


def configureAzureBlobStorageSASBased(linkedService, container, sparkSession):
    pass


def configureADLS2TokenBased(linkedService, sparkSession):
    pass


def configureADLS2SASBased(linkedService, sparkSession):
    pass


def configureADLS2AzureKeyVaultBased(
    akvLinkedService, storageAccountName, secretName, sparkSession
):
    pass


def tridentHelp():
    pass


def synapseHelp():
    pass


def help(method_name=None):
    pass
