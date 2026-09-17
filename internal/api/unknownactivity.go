package api

import (
	"fmt"
	"sort"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
)

// The dispatch default used to answer {"status":"Succeeded"} for ANY activity
// type it did not recognise, justified as a CONNECTOR LEAF: a ServiceNow
// source really was reached in dependsOn order with its inputs resolved, so
// recording that much was honest.
//
// The justification described something that does not exist. A connector is
// not an activity type in either oracle — neither ADF's schema nor Fabric's
// DataPipelineActivityTypes table has a ServiceNow or Salesforce
// discriminator. A connector arrives as the `type` of a Copy activity's source
// or sink, and copyActivity already refuses the ones it cannot run BY NAME:
//
//	copy "sn" source: source type "ServiceNowSource" is not supported by the emulator
//
// Two tests had encoded the fiction, running activity types named
// "ServiceNowLeaf" and "SalesforceSource" — strings no authoring surface can
// produce. They were asserting the stub against inputs invented to reach it.
//
// What actually reached the default, measured: only type strings no oracle
// documents. Which is worse than the fiction, because that set contains
// TYPOS. `TridentNotebok` returned Succeeded having run no notebook, and so
// did the EMPTY STRING — an activity with no type at all was reported as done.
// Every documented type has had a real outcome since the discriminators were
// diffed against the dispatch (fabricactivitytypes_test.go walks Fabric's own
// list and fails on any that does not), so nothing legitimate is left here to
// pass through.
//
// So the default refuses, on the same rule as unrunnableactivities.go: a
// success that claims an effect downstream activities go on to consume is
// worse than a failure. The difference is only in what is being reported —
// there the emulator cannot run a named runtime, here it cannot tell what was
// named at all.

// acceptedActivityTypes is every type string the dispatch, the interpreter, or
// the refusal map answers for. It exists ONLY to suggest near-matches on a
// typo — a name missing from it degrades a hint and breaks nothing.
//
// The direction that IS enforced: TestEveryAcceptedActivityTypeIsReallyHandled
// runs each one and fails if it reaches the refusal below. A name may
// therefore be absent from this list, but never wrong in it.
var acceptedActivityTypes = func() []string {
	types := []string{
		// Fabric's own names and the ADF spellings kept for compatibility.
		"AppendVariable", "AzureDataExplorerCommand", "AzureFunction",
		"AzureFunctionActivity", "AzureHDInsight", "AzureMLBatchExecution",
		"AzureMLExecutePipeline", "AzureMLUpdateResource", "Copy", "Custom",
		"DatabricksNotebook", "DatabricksSparkJar", "DatabricksSparkPython",
		"Delete", "ExecuteDataFlow", "ExecutePipeline",
		"ExecutePowerQueryTemplate", "Fail", "Filter", "ForEach",
		"GetMetadata", "HDInsightSpark", "IfCondition", "InvokeCopyJob",
		"InvokePipeline", "KustoQueryLanguage", "Lookup", "RefreshDataFlow",
		"RefreshDataflow", "RunNotebook", "Script", "SetVariable",
		"SparkJobDefinition", "SqlPoolStoredProcedure",
		"SqlServerStoredProcedure", "Switch", "SynapseNotebook",
		"TridentNotebook", "Until", "Validation", "Wait", "Web", "WebActivity",
		"WebHook",
	}
	for typ := range unrunnableActivities {
		types = append(types, typ)
	}
	sort.Strings(types)
	return types
}()

// nearestActivityTypes returns the accepted names within a small edit distance
// of what the author wrote. A typo is the likeliest way to land here, and the
// refusal is far more useful when it can say which name was meant.
func nearestActivityTypes(typ string) []string {
	if typ == "" {
		return nil
	}
	// Distance scaled to the name's length: a 3-edit budget on a 4-character
	// string matches nearly anything, which is a worse answer than none.
	budget := len(typ) / 4
	if budget < 1 {
		budget = 1
	}
	if budget > 3 {
		budget = 3
	}
	lower := strings.ToLower(typ)
	var near []string
	for _, cand := range acceptedActivityTypes {
		lc := strings.ToLower(cand)
		// Edit distance alone misses the commonest real mistake, which is not
		// a typo at all: writing the BARE product name. `Notebook` is 8 edits
		// from TridentNotebook and means it unambiguously, so a name the
		// accepted spelling contains counts too. Four characters minimum —
		// below that a substring matches half the list.
		if editDistance(lower, lc) <= budget ||
			(len(typ) >= 4 && strings.Contains(lc, lower)) {
			near = append(near, cand)
		}
	}
	// A suggestion list long enough to scan is not a suggestion.
	if len(near) > 4 {
		near = near[:4]
	}
	return near
}

// editDistance is Levenshtein, iterative with two rows.
func editDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	prev := make([]int, len(br)+1)
	cur := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		cur[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			cur[j] = min(min(cur[j-1]+1, prev[j]+1), prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(br)]
}

// unknownActivityRefusal explains a type string neither oracle documents. The
// tail matches unrunnableRefusal's on purpose: the reason a fabricated success
// is worse than a failure does not vary, and a reader who has met one of these
// should recognise the next.
func unknownActivityRefusal(act pipeline.Activity) error {
	if strings.TrimSpace(act.Type) == "" {
		return fmt.Errorf("activity %q declares no type. An activity with no "+
			"type names no work, so there is nothing to run and nothing to "+
			"report; it fails rather than reporting Succeeded because a "+
			"success here would claim an effect that downstream activities go "+
			"on to consume", act.Name)
	}
	hint := ""
	if near := nearestActivityTypes(act.Type); len(near) > 0 {
		hint = fmt.Sprintf(" Did you mean %s?", strings.Join(near, " or "))
	}
	return fmt.Errorf("activity %q has type %q, which is not in Fabric's "+
		"DataPipelineActivityTypes table and is not an ADF type this emulator "+
		"accepts.%s A connector is NOT an activity type — a ServiceNow or "+
		"Salesforce source is the `type` of a Copy activity's source or sink, "+
		"and Copy reports on those itself. This fails rather than reporting "+
		"Succeeded because a success here would claim an effect that "+
		"downstream activities go on to consume. To skip a step deliberately, "+
		"set its state to \"Inactive\" with onInactiveMarkAs, which is "+
		"Fabric's own way of saying so", act.Name, act.Type, hint)
}
