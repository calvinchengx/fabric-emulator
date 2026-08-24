def getToken(audience, name=""):
    return ""


def getConnectionStringOrCreds(linkedService):
    return ""


def getFullConnectionString(linkedService):
    return ""


def getPropertiesAll(linkedService):
    return ""


def getSPTokenWithCert(resource, authority, clientId, akvNameOrUri, certificateName):
    return ""


def getSPTokenWithCertLS(
    resource, authority, clientId, akvLinkedService, certificateName
):
    return ""


def getSecret(akvName, secret, linkedService=""):
    return ""


def getSecretWithLS(linkedService, secret):
    return ""


def putSecret(akvName, secretName, secretValue, linkedService=""):
    return ""


def putSecretWithLS(linkedService, secretName, secretValue):
    return ""


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


def help():
    pass
