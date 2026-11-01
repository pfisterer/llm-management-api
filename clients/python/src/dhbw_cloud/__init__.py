"""Python client of the DHBW Cloud's AI service (llm-management-api).

dhbw_cloud.api is generated from the service's OpenAPI description and changes with it; dhbw_cloud.client() returns it set up with the address and token from the environment or the token file (see connect). The command dhbw-cloud (dhbw_cloud.cli) uses it, `dhbw-cloud job …` for batch jobs.
"""

from ._config import DEFAULT_URL, TOKEN_FILE, client, connect

__all__ = ["DEFAULT_URL", "TOKEN_FILE", "client", "connect"]
