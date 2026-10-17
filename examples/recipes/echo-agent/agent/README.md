# Echo agent

A plain A2A agent in Python, with no model and no credentials. Read `agent.py` top to bottom: each block names the row
of the [agent contract](../../../../docs/reference/agent-contract.md) it satisfies, and the a2a-sdk serves the protocol.

It echoes what it is told, streaming the reply in pieces. Say `ask` and it pauses with a question; say `fail` and the
task fails; say `help` for the list. By default it accepts every caller. Set `ECHO_AGENT_TOKEN_FILE` to a file that
holds a token and it answers 401 to any request without `Authorization: Bearer <that token>`.

Run it from this directory (CPython 3.14), then print one exchange the way the recipe's README shows it:

```sh
python3.14 -m venv .venv && .venv/bin/pip install --require-hashes -r requirements.txt
.venv/bin/python agent.py                                   # serves on :8080; PORT changes it
.venv/bin/python capture_transcript.py http://localhost:8080 hello
```

From the repository root: `make echo-agent-test`, `make echo-agent-lint` and `make echo-agent-image`.
`requirements.txt` and `requirements-dev.txt` are compiled with hashes from the `.in` files; the command is at the top
of each.
