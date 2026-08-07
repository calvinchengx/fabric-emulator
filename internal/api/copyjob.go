package api

// A Copy Job copies. The typed collection has existed since v0.7.0 with
// create/get/list/patch/delete and definition round-trip, and parity.md's own
// sharpest self-criticism beside it: "a Copy Job does not copy". This closes
// that — NOT by building a second copy engine, but by translating the
// documented copyjob-content.json into the pipeline Copy activity the emulator
// already executes for real (bytes moved, rowsCopied reported, lineage edges
// with a stated producer). One engine, two front doors; the portal's DAX box
// set the precedent, sharing executeQueries' evaluator so box and wire cannot
// diverge.
//
// The contract implemented here is Microsoft's:
//   - definition part `copyjob-content.json` — properties{jobMode, source,
//     destination, policy} + activities[]{source/destination datasetSettings,
//     writeBehavior, translator} (rest/api/fabric/articles/item-management/
//     definitions/copyjob-definition, retrieved 2026-08-08).
//   - run on demand is `POST …/jobs/instances?jobType=Execute` — Execute, not
//     CopyJob; Microsoft's own capabilities article shows the asymmetry, with
//     the instance READBACK carrying jobType "CopyJob"
//     (fabric/data-factory/copy-job-rest-api-capabilities). Both spellings are
//     accepted on dispatch; what was submitted is what is stored.
//
// Honest boundary, refused loudly rather than half-served: only in-family
// Lakehouse connections execute. External stores (AzureSqlDatabase, Blob,
// Salesforce, …) arrive through Fabric *connections* whose credentials and
// drivers the emulator does not hold on this surface, and `jobMode: "CDC"`
// requires change tracking on a source we would first have to reach. Both fail
// with a code naming the boundary — a CopyJob that silently skipped its
// external legs would be the false green this repo keeps finding.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
)

// copyJobContent is copyjob-content.json, fields limited to what execution
// needs — the definition round-trip stores the full document regardless.
type copyJobContent struct {
	Properties struct {
		JobMode     string             `json:"jobMode"`
		Source      *copyJobConnection `json:"source"`
		Destination *copyJobConnection `json:"destination"`
	} `json:"properties"`
	Activities []struct {
		ID         string `json:"id"`
		Properties struct {
			Source struct {
				DatasetSettings copyJobDataset `json:"datasetSettings"`
			} `json:"source"`
			Destination struct {
				WriteBehavior   string         `json:"writeBehavior"`
				DatasetSettings copyJobDataset `json:"datasetSettings"`
			} `json:"destination"`
		} `json:"properties"`
	} `json:"activities"`
}

type copyJobConnection struct {
	Type               string `json:"type"`
	ConnectionSettings struct {
		Type           string `json:"type"`
		TypeProperties struct {
			WorkspaceID string `json:"workspaceId"`
			ArtifactID  string `json:"artifactId"`
			RootFolder  string `json:"rootFolder"`
		} `json:"typeProperties"`
		ExternalReferences *struct {
			Connection string `json:"connection"`
		} `json:"externalReferences"`
	} `json:"connectionSettings"`
}

type copyJobDataset struct {
	Schema string `json:"schema"`
	Table  string `json:"table"`
}

// copyJobDefinition reads the item's copyjob-content.json part.
func (a *API) copyJobDefinition(itemID string) ([]byte, error) {
	parts, err := a.Store.GetDefinition(itemID)
	if err != nil {
		return nil, err
	}
	for _, p := range parts {
		if p.Path == "copyjob-content.json" {
			return base64.StdEncoding.DecodeString(p.Payload)
		}
	}
	return nil, fmt.Errorf("no copyjob-content.json part")
}

// runCopyJob executes a CopyJob item's definition now, returning a failure
// code ("" on success). Mirrors runPipelineWith's contract so startJob treats
// both the same way.
func (a *API) runCopyJob(wid string, itemID, jobID string) string {
	raw, err := a.copyJobDefinition(itemID)
	if err != nil {
		return "CopyJobDefinitionInvalid"
	}
	var def copyJobContent
	if err := json.Unmarshal(raw, &def); err != nil {
		return "CopyJobDefinitionInvalid"
	}

	// CDC is change tracking against a source this surface cannot reach —
	// refuse by name rather than running a Batch copy and calling it CDC.
	if strings.EqualFold(def.Properties.JobMode, "CDC") {
		return "CopyJobCDCNotImplemented"
	}
	if def.Properties.Source == nil || def.Properties.Destination == nil {
		// Microsoft's minimal example ({"jobMode":"Batch","activities":[]})
		// carries neither; with nothing to copy the job completes empty, and
		// with activities but no endpoints the definition is broken.
		if len(def.Activities) == 0 {
			return ""
		}
		return "CopyJobDefinitionInvalid"
	}
	// The boundary, named with the side that hit it — as a distinct CODE per
	// side, because the failure code is the only channel a job carries and a
	// reader debugging a refused CopyJob needs to know which leg to fix.
	// Checked in fixed order (source first), NOT by ranging a map: Go
	// randomises map iteration, so with both sides external the reported side
	// would vary per run — a flake by construction. External connections are a
	// credential+driver surface the emulator does not fake.
	for _, side := range []struct {
		code string
		conn *copyJobConnection
	}{
		{"CopyJobExternalSourceNotSupported", def.Properties.Source},
		{"CopyJobExternalDestinationNotSupported", def.Properties.Destination},
	} {
		if !strings.Contains(side.conn.ConnectionSettings.Type, "Lakehouse") {
			return side.code
		}
		if side.conn.ConnectionSettings.TypeProperties.ArtifactID == "" {
			return "CopyJobDefinitionInvalid"
		}
	}

	e := &pipelineExecutor{a: a, wid: wid, jobID: jobID, chain: []string{itemID}}
	// CopyJob definitions carry no expression language, so raw JSON resolves
	// to itself — unlike a pipeline, where @-expressions earn a real resolver.
	literal := func(raw json.RawMessage) (any, error) {
		var v any
		err := json.Unmarshal(raw, &v)
		return v, err
	}

	for i, ca := range def.Activities {
		name := ca.ID
		if name == "" {
			name = fmt.Sprintf("copy-%d", i)
		}
		srcPath := datasetPath(def.Properties.Source, ca.Properties.Source.DatasetSettings)
		dstPath := datasetPath(def.Properties.Destination, ca.Properties.Destination.DatasetSettings)
		if srcPath == "" || dstPath == "" {
			return "CopyJobDefinitionInvalid"
		}

		// The synthetic pipeline Copy activity: the SAME typeProperties shape
		// copyActivity executes for a pipeline, so table sinks get real Delta
		// commits (Append appends, Overwrite replaces) and the lineage edge is
		// recorded with producer Copy — the emulator really watched these
		// bytes move.
		action := ca.Properties.Destination.WriteBehavior
		if action == "" {
			action = "Overwrite" // Fabric's UI default for a Batch table copy
		}
		if !strings.EqualFold(action, "Append") && !strings.EqualFold(action, "Overwrite") {
			// Merge/Upsert need key-based reconciliation this executor does
			// not have; a Merge quietly downgraded to Overwrite would destroy
			// rows and report success.
			return "CopyJobWriteBehaviorNotSupported"
		}
		tp, _ := json.Marshal(map[string]any{
			"source": map[string]any{"location": map[string]any{
				"workspaceId": def.Properties.Source.ConnectionSettings.TypeProperties.WorkspaceID,
				"itemId":      def.Properties.Source.ConnectionSettings.TypeProperties.ArtifactID,
				"path":        srcPath,
			}},
			"sink": map[string]any{
				"location": map[string]any{
					"workspaceId": def.Properties.Destination.ConnectionSettings.TypeProperties.WorkspaceID,
					"itemId":      def.Properties.Destination.ConnectionSettings.TypeProperties.ArtifactID,
					"path":        dstPath,
				},
				"tableActionOption": action,
			},
		})
		act := pipeline.Activity{Name: name, Type: "Copy", TypeProperties: tp}
		var tpMap map[string]json.RawMessage
		_ = json.Unmarshal(tp, &tpMap)

		out, err := e.copyActivity(act, tpMap, literal)
		status, errText := "Succeeded", ""
		if err != nil {
			status, errText = "Failed", err.Error()
		}
		// Announced like a pipeline activity, so the flow view shows a copy
		// job's tables moving the same way it shows a pipeline's.
		a.Store.PublishActivityEvent(wid, itemID, jobID, name, "Copy", status, errText, 0, 0)
		if err != nil {
			return "CopyJobActivityFailed"
		}
		_ = out
	}
	return ""
}

// datasetPath resolves one side's OneLake path: the connection's rootFolder
// (Tables unless stated) joined with the activity's schema-qualified table.
func datasetPath(conn *copyJobConnection, ds copyJobDataset) string {
	if ds.Table == "" {
		return ""
	}
	root := conn.ConnectionSettings.TypeProperties.RootFolder
	if root == "" {
		root = "Tables"
	}
	if ds.Schema != "" {
		return path.Join(root, ds.Schema, ds.Table)
	}
	return path.Join(root, ds.Table)
}
