"""Where the service is and who asks: the address from DHBW_CLOUD_URL (default production), the token from DHBW_CLOUD_TOKEN or the token file ~/.config/dhbw-cloud/token, which `dhbw-cloud login` writes. Tokens are the personal API tokens of the service (aiaas_…), created in the self-service UI."""

import os
import stat
from pathlib import Path

from .api import AuthenticatedClient

DEFAULT_URL = "https://llm-api.llm.services.dhbw.cloud"
TOKEN_FILE = Path(os.environ.get("XDG_CONFIG_HOME") or Path.home() / ".config") / "dhbw-cloud" / "token"


def connect():
    """The address and the token; the token is None when neither the environment nor the token file has one."""
    url = (os.environ.get("DHBW_CLOUD_URL") or DEFAULT_URL).rstrip("/")
    token = os.environ.get("DHBW_CLOUD_TOKEN")
    if not token and TOKEN_FILE.is_file():
        token = TOKEN_FILE.read_text().strip()
    return url, token or None


def save_token(token):
    """Writes the token file, readable only by its owner."""
    TOKEN_FILE.parent.mkdir(parents=True, exist_ok=True)
    fd = os.open(TOKEN_FILE, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, stat.S_IRUSR | stat.S_IWUSR)
    with os.fdopen(fd, "w") as f:
        f.write(token.strip() + "\n")


def client(timeout=30.0):
    """The generated client (dhbw_cloud.api.AuthenticatedClient) for the service, with the token from connect; raises RuntimeError without one."""
    url, token = connect()
    if not token:
        raise RuntimeError(f"Kein Token: DHBW_CLOUD_TOKEN setzen oder `dhbw-cloud login` ausführen (API-Token aus der Self-Service-Oberfläche, Datei {TOKEN_FILE}).")
    return AuthenticatedClient(base_url=url, token=token, timeout=timeout, raise_on_unexpected_status=False)
