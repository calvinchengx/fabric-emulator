package api

import (
	"fmt"

	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
)

// The Azure Machine Learning activities — ADF/Fabric's `AzureMLExecutePipeline`
// and the two ML Studio (classic) activities `AzureMLBatchExecution` and
// `AzureMLUpdateResource`.
//
// ORACLE: ADF's published schema. Discriminators `AzureMLExecutePipeline`
// (required `mlPipelineId`; also `experimentName`, `mlPipelineParameters`,
// `mlParentRunId`, `continueOnStepFailure`), `AzureMLBatchExecution`
// (`globalParameters`, `webServiceInputs`, `webServiceOutputs`, the latter two
// blob files named through a LinkedServiceReference) and `AzureMLUpdateResource`
// (required `trainedModelName`, `trainedModelLinkedServiceName`,
// `trainedModelFilePath`).
//
// THE REFUSAL IS THE IMPLEMENTATION, AND IT IS A CORRECTION RATHER THAN A GAP.
// These three type strings were not in the dispatch switch, so they fell to its
// default — which returns {"status":"Succeeded"}. A pipeline that scored a
// model through one of these activities was reported as having done so, and a
// downstream step then read a scored output that was never written. That is the
// false green this repo exists to hunt, and it is worse here than for a
// connector leaf precisely because an ML pipeline run HAS a result other
// activities consume.
//
// WHY THESE DIFFER FROM HDINSIGHT / DATABRICKS / AZURE BATCH, all of which do
// run. Each of those activities names A THING TO EXECUTE that the emulator can
// actually get hold of: an entry file, a notebook path, a python file, a shell
// command. The submission protocol is external, the code is not, so terminating
// the protocol locally and letting the engine we already run compute is honest
// — the Livy precedent (docs/20).
//
// None of that holds here. `mlPipelineId` is an opaque identifier for a
// pipeline PUBLISHED IN AN AZURE ML WORKSPACE; its steps live in that
// workspace, and nothing about them is reachable from the pipeline definition,
// from OneLake, or from any item the emulator holds. There is no code to read
// and therefore nothing to terminate the protocol *onto*. Running some other
// artifact in its place would invent the published pipeline's behaviour, which
// is the same objection that refuses a `dbfs:` path in the Databricks activity
// — reinterpreting another service's namespace as ours invents a mapping
// nobody wrote.
//
// (docs/43's plan line sketched "python entry via the agent" for this row, by
// analogy with its neighbours. The schema does not support the analogy: none of
// the three activities names an entry point. The oracle wins.)
//
// The MLflow sidecar does not change this. Azure ML's run history is
// MLflow-shaped and the emulator runs a real MLflow, so the RECORD of a run is
// something it could produce — but a run record marked finished for steps that
// never executed just relocates the fabrication from the activity output into
// the tracking store, where it is harder to see rather than less false.
//
// SKIPPING ONE DELIBERATELY IS SUPPORTED, and by a real Fabric feature rather
// than an emulator flag: set the activity's `state` to "Inactive" with
// `onInactiveMarkAs`, which is how Fabric itself says "do not run this step,
// treat it as X". That covers wanting the orchestration shape without the
// compute, and it says so in the definition — where a reader can see it —
// instead of in an environment variable that silently turns failures green.
func (e *pipelineExecutor) azureMLActivity(act pipeline.Activity) (map[string]any, error) {
	var cause string
	switch act.Type {
	case "AzureMLBatchExecution":
		cause = "calls the Batch Execution Service of a published ML Studio (classic) web " +
			"service, reading its inputs and writing its outputs as blobs named through " +
			"storage linked services. The emulator hosts no such endpoint and models no " +
			"linked services, and Azure has retired ML Studio (classic) — so there is " +
			"nothing to call, and nothing left to be faithful to"

	case "AzureMLUpdateResource":
		cause = "uploads a trained model (an .ilearner file, named by " +
			"trainedModelLinkedServiceName and trainedModelFilePath) into a published ML " +
			"Studio (classic) web service. The emulator hosts no such web service and " +
			"models no linked services, and Azure has retired ML Studio (classic). For " +
			"model artifacts the emulator does keep, MLModel items on the real MLflow " +
			"sidecar are the modelled route"

	default:
		// AzureMLExecutePipeline — the only one of the three that Fabric's own
		// activity gallery still offers, and the one most likely to be reached.
		cause = "runs a pipeline PUBLISHED IN AN AZURE MACHINE LEARNING WORKSPACE, named " +
			"only by mlPipelineId. The steps it would run live in that workspace, not in " +
			"the pipeline definition and not in any artifact the emulator can read, so " +
			"there is nothing here to execute and no way to know what the run would " +
			"produce. Executing something else in its place would invent the published " +
			"pipeline's behaviour"
	}

	return nil, fmt.Errorf("azure ml activity %q (%s): this activity %s. It fails rather than "+
		"reporting Succeeded because a success here would claim a result that downstream "+
		"activities go on to read. To run code the emulator can execute, use a Notebook or "+
		"Spark Job Definition activity; to skip this step deliberately, set the activity's "+
		"state to \"Inactive\" with onInactiveMarkAs, which is Fabric's own way of saying so",
		act.Name, act.Type, cause)
}
