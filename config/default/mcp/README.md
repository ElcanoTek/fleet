# MCP servers

The generic bundle ships no MCP servers. A client bundle places its Python MCP
server scripts here (e.g. `mcp/myserver.py`) and declares them in
`manifest.yaml` under `mcp_servers[]`, where `args` resolve relative to the
bundle root (e.g. `["mcp/myserver.py"]`).

A client bundle that ships MCP servers also ships a `requirements.txt` here (and
typically `ruff.toml` / `pytest.ini` at the bundle root) so the servers can be
installed into a `.venv` and tested independently of fleet.
