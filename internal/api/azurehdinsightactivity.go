package api

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
)

// hdinsightPrograms maps the program an `AzureHDInsight` activity names to the
// ADF discriminator this emulator already reasons about.
//
// Fabric merges ADF's five HDInsight activity types into ONE — the
// DataPipelineActivityTypes table describes `AzureHDInsight` as "Runs various
// programs (Hive, Pig, MapReduce, Streaming, Spark)". A name alias would
// therefore be wrong in the most dangerous way available: it would send a Hive
// script to whichever single handler was chosen, and the Spark agent computes
// Spark SQL, not HiveQL. The overlap between two dialects is not the guarantee.
//
// So the program decides, and four of the five keep the refusals they already
// have with their causes intact.
var hdinsightPrograms = map[string]string{
	"spark":     "HDInsightSpark",
	"hive":      "HDInsightHive",
	"pig":       "HDInsightPig",
	"mapreduce": "HDInsightMapReduce",
	"streaming": "HDInsightStreaming",
}

// azureHDInsightActivity routes Fabric's single HDInsight activity to the
// program it actually names.
func (e *pipelineExecutor) azureHDInsightActivity(
	act pipeline.Activity,
	tp map[string]json.RawMessage,
	resolve func(json.RawMessage) (any, error),
) (map[string]any, error) {
	program, err := hdinsightProgram(tp, resolve)
	if err != nil {
		return nil, fmt.Errorf("HDInsight activity %q: %w", act.Name, err)
	}
	adf, ok := hdinsightPrograms[strings.ToLower(program)]
	if !ok {
		return nil, fmt.Errorf("HDInsight activity %q names program %q, which is not one of "+
			"the five this activity type covers (%s). Refused rather than run: guessing which "+
			"engine an unknown program wants is how a script gets executed under the wrong "+
			"semantics and reported as the right one", act.Name, program, knownHDInsightPrograms())
	}
	if adf == "HDInsightSpark" {
		return e.hdinsightSparkActivity(act, tp, resolve)
	}
	// The other four are refused with the cause already written for their ADF
	// discriminator, so a Fabric-authored pipeline gets the same honest answer
	// an ADF-authored one does rather than a fabricated success.
	cause, ok := unrunnableActivities[adf]
	if !ok {
		return nil, fmt.Errorf("HDInsight activity %q: program %q is not implemented", act.Name, program)
	}
	return nil, unrunnableRefusal(pipeline.Activity{Name: act.Name, Type: adf}, cause)
}

// hdinsightProgram reads the program name. Fabric's own designer writes it as
// `type` inside typeProperties; `program` is accepted as the other spelling
// seen in the wild. Absent, it cannot be guessed.
func hdinsightProgram(tp map[string]json.RawMessage, resolve func(json.RawMessage) (any, error)) (string, error) {
	for _, key := range []string{"type", "program", "programType"} {
		raw, ok := tp[key]
		if !ok || len(raw) == 0 {
			continue
		}
		v, err := resolve(raw)
		if err != nil {
			return "", fmt.Errorf("%s: %w", key, err)
		}
		if s := fmt.Sprint(v); v != nil && s != "" {
			return s, nil
		}
	}
	return "", fmt.Errorf("typeProperties names no program, so which of %s to run is unknown. "+
		"This activity covers five different engines and picking one would be a guess",
		knownHDInsightPrograms())
}

func knownHDInsightPrograms() string {
	names := make([]string, 0, len(hdinsightPrograms))
	for n := range hdinsightPrograms {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
