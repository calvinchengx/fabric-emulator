package api

// fabricActivityTypes is Fabric's OWN list of data-pipeline activity type
// discriminators — the `DataPipelineActivityTypes` table in:
//
//	https://learn.microsoft.com/en-us/rest/api/fabric/articles/item-management/definitions/datapipeline-definition
//
// THIS IS THE ORACLE, AND NAMING THE WRONG ONE IS WHAT CAUSED THE BUG IT
// EXISTS TO PREVENT. unrunnableactivities.go previously recorded its oracle as
// "ADF's published schema", and the dispatch was diffed against that. Fabric
// renames several of ADF's discriminators and merges others, so twelve Fabric
// names matched nothing in the dispatch and fell to its default — which
// returns {"status":"Succeeded"}. A Fabric-authored pipeline was told its Spark
// job ran, its email sent, its semantic model refreshed, having done none of it.
//
// A type name is part of the wire contract exactly as a response code is. Both
// were checked against the wrong document, which is why the fix is a list with
// its source attached and a test that walks it, rather than twelve more cases.
//
// Nothing here is invented: every entry is transcribed from that table. ADF
// names the emulator also accepts (HDInsightHive, AzureFunctionActivity,
// RunNotebook, …) are deliberately ABSENT — they are not Fabric's vocabulary,
// they are compatibility with the other authoring surface, and mixing the two
// lists is the mistake being corrected.
var fabricActivityTypes = []string{
	"AppendVariable",
	"AzureFunction",
	"AzureHDInsight",
	"AzureMLExecutePipeline",
	"Copy",
	"Custom",
	"DataLakeAnalyticsScope",
	"DatabricksNotebook",
	"Delete",
	"Email",
	"ExecutePipeline",
	"ExecuteSSISPackage",
	"Fail",
	"Filter",
	"ForEach",
	"GetMetadata",
	"IfCondition",
	"InvokeCopyJob",
	"InvokePipeline",
	"KustoQueryLanguage",
	"Lookup",
	"MicrosoftTeams",
	"Office365Email",
	"PBISemanticModelRefresh",
	"RefreshDataFlow",
	"Script",
	"SetVariable",
	"SparkJobDefinition",
	"SqlServerStoredProcedure",
	"Switch",
	"Teams",
	"TridentNotebook",
	"Until",
	"Wait",
	"WebActivity",
	"WebHook",
}
