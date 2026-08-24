def help(method_name=None):
    pass


def cp(src, dest, recurse=False):
    return False


def mv(src, dest, create_path=False, overwrite=False):
    return False


def ls(dir):
    return []


def mkdirs(dir):
    return False


def put(file, content, overwrite=False):
    return False


def head(file, max_bytes=1024 * 100):
    return False


def append(file, content, createFileIfNotExists=False):
    return False


def rm(dir, recurse=False):
    return False


def exists(file):
    return False


def mountToDriverNode(source, mountPoint, extraConfigs={}):
    return False


def unmountFromDriverNode(mountPoint):
    return False


def mount(source, mountPoint, extraConfigs={}):
    return False


def unmount(mountPoint, extraOptions={}):
    return False


def mounts(extraOptions={}):
    return False


def refreshMounts():
    return False


def getMountPath(mountPoint, scope=""):
    return False


def fastcp(src, dest, recurse=True, extraConfigs={}):
    return False
