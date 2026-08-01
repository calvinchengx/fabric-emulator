# examples

Working code that *uses* the emulator, kept deliberately apart from the code
that *is* the emulator.

| Example | What it shows | Tutorial | CI witness |
|---|---|---|---|
| [`medallion/`](medallion/) | the whole loop in one sitting: Key Vault → landing → bronze/silver → gold with dbt → semantic model, from one source | [docs/28](../docs/28-tutorial-end-to-end.md) | [`e2e/medallion`](../e2e/medallion/) |

## The conventions

**One directory per example, paired 1:1 with an `e2e/` harness of the same
name.** The harness owns the compose file and the CI plumbing; the example owns
every line of pipeline code. The harness runs the example *unmodified* — only
endpoints differ, passed as environment variables the example already reads. So
nothing can pass in CI that would fail for a reader typing the steps by hand.

**Each example owns its `pyproject.toml` and `uv.lock`.** Two reasons. It can be
copied out of this repo and run anywhere — `cp -r examples/medallion ~/mine &&
cd ~/mine && uv sync && uv run python run_all.py`. And its dependencies
(pandas, dbt, pyodbc, …) never enter the emulator's own dependency graph, which
stays about building and testing the emulator.

**Examples do not share helper modules.** Each one carries its own `common.py`
even where that duplicates another. Copy-paste independence is the property that
makes an example useful; DRY across examples would trade it away for nothing a
reader benefits from.

**Every example is executable end to end and asserts its own results.** An
example that merely *looks* right is a liability — these fail loudly instead.

## Running one

Start the family from the repo root, then follow the example's README:

```sh
docker compose up -d
cd examples/medallion && uv sync && uv run python run_all.py
```

Or run its CI harness, which does the same thing in containers:

```sh
python3 e2e/medallion/run.py
```
