// Package compute parses Fabric compute item definitions into engine-neutral
// execution contracts used by notebook and Spark Job Definition runners.
package compute

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Binding identifies the Fabric resources attached to a compute run.
type Binding struct {
	WorkspaceID            string `json:"workspaceId,omitempty"`
	LakehouseID            string `json:"lakehouseId,omitempty"`
	LakehouseName          string `json:"lakehouseName,omitempty"`
	EnvironmentID          string `json:"environmentId,omitempty"`
	EnvironmentWorkspaceID string `json:"environmentWorkspaceId,omitempty"`
}

// Environment is the supported, portable subset of a Fabric Environment.
type Environment struct {
	PythonPackages []string          `json:"pythonPackages,omitempty"`
	SparkConfig    map[string]string `json:"sparkConfig,omitempty"`
	JARs           []string          `json:"jars,omitempty"`
}

// SparkJob describes an executable Spark Job Definition.
type SparkJob struct {
	MainFile  string   `json:"mainFile"`
	Arguments []string `json:"arguments,omitempty"`
	Libraries []string `json:"libraries,omitempty"`
	Source    string   `json:"source"`
}

func decodedParts(parts []store.DefinitionPart) (map[string][]byte, error) {
	out := make(map[string][]byte, len(parts))
	for _, p := range parts {
		b, err := base64.StdEncoding.DecodeString(p.Payload)
		if err != nil {
			return nil, fmt.Errorf("decode definition part %q: %w", p.Path, err)
		}
		out[p.Path] = b
	}
	return out, nil
}

// NotebookBinding reads the dependencies object from a Fabric notebook's
// METADATA section. Both snake_case export keys and REST-style camelCase keys
// are accepted because Fabric exports in both forms.
func NotebookBinding(source []byte) Binding {
	var metaLines []string
	for _, line := range strings.Split(string(source), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# META ") {
			metaLines = append(metaLines, strings.TrimPrefix(line, "# META "))
		}
	}
	var root map[string]any
	decoder := json.NewDecoder(strings.NewReader(strings.Join(metaLines, "\n")))
	if decoder.Decode(&root) != nil {
		return Binding{}
	}
	deps, _ := root["dependencies"].(map[string]any)
	lake, _ := deps["lakehouse"].(map[string]any)
	env, _ := deps["environment"].(map[string]any)
	return Binding{
		WorkspaceID:            firstString(lake, "default_lakehouse_workspace_id", "workspaceId", "workspace_id"),
		LakehouseID:            firstString(lake, "default_lakehouse", "lakehouseId", "id"),
		LakehouseName:          firstString(lake, "default_lakehouse_name", "lakehouseName", "name"),
		EnvironmentID:          firstString(env, "environmentId", "environment_id", "id"),
		EnvironmentWorkspaceID: firstString(env, "workspaceId", "workspace_id"),
	}
}

func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if s, ok := m[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// ParseEnvironment resolves requirements, Spark properties, and JAR library
// declarations from common Environment definition export shapes.
func ParseEnvironment(parts []store.DefinitionPart) (Environment, error) {
	decoded, err := decodedParts(parts)
	if err != nil {
		return Environment{}, err
	}
	env := Environment{SparkConfig: map[string]string{}}
	for path, body := range decoded {
		lower := strings.ToLower(path)
		switch {
		case strings.HasSuffix(lower, "requirements.txt"):
			for _, line := range strings.Split(string(body), "\n") {
				line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
				if line != "" {
					env.PythonPackages = append(env.PythonPackages, line)
				}
			}
		case strings.HasSuffix(lower, ".jar"):
			env.JARs = append(env.JARs, path)
		case strings.HasSuffix(lower, ".json"):
			var raw map[string]any
			if json.Unmarshal(body, &raw) != nil {
				continue
			}
			collectEnvironmentJSON(raw, &env)
		}
	}
	sort.Strings(env.PythonPackages)
	sort.Strings(env.JARs)
	return env, nil
}

func collectEnvironmentJSON(raw map[string]any, env *Environment) {
	for _, key := range []string{"sparkProperties", "sparkConfig", "properties"} {
		if props, ok := raw[key].(map[string]any); ok {
			for k, v := range props {
				env.SparkConfig[k] = fmt.Sprint(v)
			}
		}
	}
	for _, key := range []string{"pythonLibraries", "libraries"} {
		if values, ok := raw[key].([]any); ok {
			for _, value := range values {
				s := fmt.Sprint(value)
				if strings.HasSuffix(strings.ToLower(s), ".jar") {
					env.JARs = append(env.JARs, s)
				} else if s != "" {
					env.PythonPackages = append(env.PythonPackages, s)
				}
			}
		}
	}
}

// ParseSparkJob resolves V1 path-based and V2 inline Spark Job Definitions.
func ParseSparkJob(parts []store.DefinitionPart) (SparkJob, Binding, error) {
	decoded, err := decodedParts(parts)
	if err != nil {
		return SparkJob{}, Binding{}, err
	}
	var config map[string]any
	for path, body := range decoded {
		if strings.EqualFold(path, "SparkJobDefinitionV1.json") || strings.HasSuffix(strings.ToLower(path), ".json") {
			if json.Unmarshal(body, &config) == nil {
				break
			}
		}
	}
	if config == nil {
		return SparkJob{}, Binding{}, fmt.Errorf("SparkJobDefinitionV1.json is required")
	}
	main := firstString(config, "executableFile", "mainFile", "main")
	if mainMap, ok := config["main"].(map[string]any); ok {
		main = firstString(mainMap, "path", "file", "name")
	}
	job := SparkJob{MainFile: main}
	if args, ok := config["arguments"].([]any); ok {
		for _, arg := range args {
			job.Arguments = append(job.Arguments, fmt.Sprint(arg))
		}
	}
	if libs, ok := config["libraries"].([]any); ok {
		for _, lib := range libs {
			job.Libraries = append(job.Libraries, fmt.Sprint(lib))
		}
	}
	if body, ok := decoded[main]; ok {
		job.Source = string(body)
	}
	if job.Source == "" {
		for path, body := range decoded {
			if strings.EqualFold(path, main) || (main == "" && strings.HasSuffix(strings.ToLower(path), ".py")) {
				job.MainFile, job.Source = path, string(body)
				break
			}
		}
	}
	if job.MainFile == "" || job.Source == "" {
		return SparkJob{}, Binding{}, fmt.Errorf("Spark job main Python file is missing")
	}
	lake, _ := config["defaultLakehouseArtifactId"].(string)
	workspace, _ := config["defaultLakehouseWorkspaceId"].(string)
	environment, _ := config["environmentArtifactId"].(string)
	environmentWorkspace, _ := config["environmentWorkspaceId"].(string)
	return job, Binding{WorkspaceID: workspace, LakehouseID: lake, EnvironmentID: environment,
		EnvironmentWorkspaceID: environmentWorkspace}, nil
}
