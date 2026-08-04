"""Run both items with `fab job run`, and check the outcome the hard way.

Two different kinds of execution, deliberately:

  * the **DataPipeline** is executed by the emulator ITSELF — it reads the CSV
    out of OneLake, commits the rows as Delta, and records the lineage. No
    client-side data movement, and no Spark involved.
  * the **Notebook** is executed by a real Spark engine (Sail, via the agent).
    The emulator drives it: it parses the notebook into cells, submits them, and
    reports the run. Without `FABRIC_SPARK_AGENT_URL` the job would park forever
    — the honest behaviour when no engine is attached.

WHAT IS NOT HERE. `check=True` on `fab job run`. It exits 0 on a job that was
cancelled or failed, which was measured rather than guessed
(../../docs/34-fab-driven-example.md). `fabctl.job_run` reads the outcome back
from `fab job run-list` and raises on anything but Completed.
"""
import fabctl as fab

pipeline = fab.job_run(fab.BRONZE_PIPELINE, timeout=600)
fab.log(f"bronze-ingest  job {pipeline['id']}  {pipeline['status']} "
        f"({pipeline.get('jobType')}, executed by the emulator)")

notebook = fab.job_run(fab.SILVER_NOTEBOOK, timeout=900)
fab.log(f"silver         job {notebook['id']}  {notebook['status']} "
        f"({notebook.get('jobType')}, executed on Spark)")
