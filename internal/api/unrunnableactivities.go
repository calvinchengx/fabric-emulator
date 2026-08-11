package api

import (
	"fmt"

	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
)

// The ADF activity types that name work the emulator cannot do, refused by
// name so they stop being reported as done.
//
// HOW THIS SET WAS FOUND, because the method matters more than the list: the
// 41 discriminators in ADF's published schema were diffed against what the
// dispatch switch and the pipeline interpreter actually handle. Nine were in
// neither, and every one of them fell to the dispatch default — which returns
// {"status":"Succeeded"}. The default is right for a CONNECTOR LEAF (a
// ServiceNow source needs a vendor SDK, and the run really did reach the leaf
// in dependsOn order with its inputs resolved), but these are not leaves: they
// are compute activities whose whole point is an effect other steps consume.
//
// Three of the nine had real compute already in the building and now run for
// real — Validation over OneLake, SqlPoolStoredProcedure on the same SQL Server
// the warehouse uses, AzureDataExplorerCommand on the Kusto engine behind
// Eventhouse. The six here do not, and the reason is the same shape in each
// case: the artifact names a runtime the emulator does not host, and the
// nearest thing it does host computes different semantics. Saying so by name
// is the deliverable; running an approximation and calling it the real thing
// would be the failure.
//
// ORACLE, and this line was WRONG in a way that cost twelve fabricated
// successes. It used to read "ADF's published schema" full stop, and the
// dispatch was diffed against exactly that. ADF's schema is the oracle for the
// ADF discriminators below and for the fields quoted in each cause — it is NOT
// the oracle for what Fabric sends. Fabric renames several types and merges
// five HDInsight ones into a single AzureHDInsight, so twelve of its names
// matched nothing here and fell to the dispatch default.
//
// Fabric's own list is the DataPipelineActivityTypes table, transcribed in
// fabricactivitytypes.go and walked by a test. Add to BOTH when adding a type:
// the ADF name for compatibility, the Fabric name because that is what arrives.
// dataLakeAnalyticsCause is shared by ADF's DataLakeAnalyticsU-SQL and
// Fabric's DataLakeAnalyticsScope — the same activity under two names.
const dataLakeAnalyticsCause = "runs a U-SQL/Scope script on Azure Data Lake Analytics. U-SQL is a " +
	"language of its own (SQL with inline C#), the emulator hosts no Data Lake Analytics " +
	"account, and AZURE HAS RETIRED THE SERVICE — so there is nothing to call and " +
	"nothing left to be faithful to"

var unrunnableActivities = map[string]string{
	"HDInsightHive": "runs a HiveQL script (scriptPath in a storage linked service) on an " +
		"HDInsight cluster. The Spark agent executes Python statements and routes SQL to " +
		"Spark SQL — a different dialect with its own semantics, so running a Hive script " +
		"through it would report Hive results the emulator never computed. The overlap " +
		"between the two dialects is not the same as the guarantee",

	"HDInsightPig": "runs a Pig Latin script on an HDInsight cluster. Pig is a distinct " +
		"dataflow language, not SQL and not Python, and nothing in the emulator interprets " +
		"it — there is no engine here to be approximately right",

	"HDInsightMapReduce": "submits a MapReduce job by className and jarFilePath. That asks " +
		"the emulator to EXECUTE A NAMED JAVA MAIN CLASS, and the Spark agent has no " +
		"submission path for one — the same boundary the Databricks JAR task is refused at, " +
		"and it holds on both engines (a JAR *library* attaching on the JVM overlay is a " +
		"different capability from running a main class)",

	"HDInsightStreaming": "runs Hadoop Streaming with a mapper and reducer executed as " +
		"processes over stdin/stdout on cluster nodes. There is no Hadoop Streaming harness " +
		"here, and the arbitrary-process half is the posture the Azure Batch activity gates " +
		"behind FABRIC_CUSTOM_ACTIVITY — that gate covers a command the caller wrote, not a " +
		"MapReduce runtime the emulator would have to supply around it",

	"DataLakeAnalyticsU-SQL": dataLakeAnalyticsCause,

	"ExecuteSSISPackage": "runs an SSIS package on an Azure-SSIS integration runtime, named " +
		"by packageLocation and reached through connectVia. The emulator hosts no " +
		"integration runtime and does not interpret the SSIS package format; a package's " +
		"work is defined inside the package, so there is nothing here to read and nothing " +
		"to run it with",

	// --- Fabric's own names, added after diffing its DataPipelineActivityTypes
	// table rather than ADF's schema. Every one of these was reporting
	// Succeeded having done nothing.

	// Fabric's spelling of the ADF DataLakeAnalyticsU-SQL activity above. The
	// SAME cause is reused rather than restated: two texts for one refusal
	// drift, and the second copy is the one nobody updates.
	"DataLakeAnalyticsScope": dataLakeAnalyticsCause,

	// The four notification types. They are grouped because the objection is
	// identical and worth stating once: each one's whole purpose is a SIDE
	// EFFECT LEAVING THE MACHINE. There is no local approximation of "a message
	// arrived in a channel", and a pipeline that branches on a notification
	// having been sent is exactly the case a fabricated success misleads.
	"Teams": "posts a message to a Teams channel or chat. The emulator has no Teams tenant " +
		"and no Graph credentials, and the activity's only observable effect is off-machine — " +
		"there is nothing local that could stand in for a message having been delivered",

	"MicrosoftTeams": "posts a message to Microsoft Teams. Fabric documents this alongside " +
		"`Teams` as a separate discriminator, so both are refused: an author who picked either " +
		"spelling gets the same honest answer rather than one of them silently succeeding",

	"Office365Email": "sends an email through Office 365. The emulator has no mailbox, no " +
		"SMTP path and no Graph credentials; delivery is the entire point of the activity and " +
		"none of it can happen here",

	"Email": "sends an email notification. Fabric lists this beside `Office365Email` as its " +
		"own discriminator, and it is refused for the same reason — the effect is delivery to " +
		"somewhere the emulator cannot reach",

	"PBISemanticModelRefresh": "refreshes a Power BI semantic model. The emulator stores and " +
		"evaluates models but does not implement the refresh pipeline that reloads a model's " +
		"data from its sources, so a success here would claim the model now holds data it " +
		"does not",
}

// unrunnableRefusal is the shared message. The tail is the same for all six on
// purpose: the reason a fabricated success is worse than a failure does not
// vary with the activity, and a reader who has met one of these should
// recognise the next.
func unrunnableRefusal(act pipeline.Activity, cause string) error {
	return fmt.Errorf("activity %q (%s): this activity %s. It fails rather than reporting "+
		"Succeeded because a success here would claim an effect that downstream activities go "+
		"on to consume. To run code the emulator can execute, use a Notebook or Spark Job "+
		"Definition activity; to skip this step deliberately, set the activity's state to "+
		"\"Inactive\" with onInactiveMarkAs, which is Fabric's own way of saying so",
		act.Name, act.Type, cause)
}
