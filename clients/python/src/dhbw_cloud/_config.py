"""Where the service is and who asks. In this order: a personal API token (aiaas_…, created in the self-service UI) set in DHBW_CLOUD_TOKEN, at DHBW_CLOUD_URL (default production), as a deliberate choice for this shell; inside a workspace of the DHBW Cloud the workspace's own hub token (JUPYTERHUB_API_TOKEN) at the service's address for workspaces (DHBW_CLOUD_WORKSPACE_URL, set by the hub), with the person's rights in their projects (batch jobs and the project list), never more — before the token file, so that an old token kept in the home does not get in the way; elsewhere the token file ~/.config/dhbw-cloud/token, which `dhbw-cloud login` writes."""

import os
import stat
from pathlib import Path

from .api import AuthenticatedClient

DEFAULT_URL = "https://llm-api.llm.services.dhbw.cloud"
TOKEN_FILE = Path(os.environ.get("XDG_CONFIG_HOME") or Path.home() / ".config") / "dhbw-cloud" / "token"


def connect():
    """The address, the token and the prefix of the Authorization header ("Bearer" for an API token, "token" for a workspace's hub token); the token is None when there is none."""
    url = (os.environ.get("DHBW_CLOUD_URL") or DEFAULT_URL).rstrip("/")
    token = os.environ.get("DHBW_CLOUD_TOKEN")
    if token:
        return url, token, "Bearer"
    workspace = workspace_login()
    if workspace:
        return workspace[0], workspace[1], "token"
    if TOKEN_FILE.is_file():
        token = TOKEN_FILE.read_text().strip()
    return url, token or None, "Bearer"


def workspace_login():
    """The service's address for workspaces and the workspace's hub token, when running in a workspace that has both; else None."""
    url, token = os.environ.get("DHBW_CLOUD_WORKSPACE_URL"), os.environ.get("JUPYTERHUB_API_TOKEN")
    if url and token:
        return url.rstrip("/"), token
    return None


def save_token(token):
    """Writes the token file, readable only by its owner."""
    TOKEN_FILE.parent.mkdir(parents=True, exist_ok=True)
    fd = os.open(TOKEN_FILE, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, stat.S_IRUSR | stat.S_IWUSR)
    with os.fdopen(fd, "w") as f:
        f.write(token.strip() + "\n")


def client(timeout=30.0):
    """The generated client (dhbw_cloud.api.AuthenticatedClient) for the service, with the address and token from connect; raises RuntimeError without one."""
    url, token, prefix = connect()
    if not token:
        raise RuntimeError(f"Kein Token: DHBW_CLOUD_TOKEN setzen oder `dhbw-cloud login` ausführen (API-Token aus der Self-Service-Oberfläche, Datei {TOKEN_FILE}). In einem Workspace der DHBW Cloud geht es ohne.")
    return AuthenticatedClient(base_url=url, token=token, prefix=prefix, timeout=timeout, raise_on_unexpected_status=False)
