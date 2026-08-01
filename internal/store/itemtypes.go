package store

import "strings"

// The ItemType enumeration, verbatim from the Fabric REST reference
// (core/items/list-items → ItemType). The reference notes that "additional
// item types may be added over time", so this is what today's Fabric accepts,
// not a permanent closed world — which is exactly why the reference also
// documents an `InvalidItemType` error for anything outside it.
//
// The fabric-docs clone this repo grounds against carries the *user*
// documentation and does not include this enumeration; it comes from the REST
// reference.
var itemTypes = []string{
	"Dashboard", "Report", "SemanticModel", "PaginatedReport", "Datamart",
	"Lakehouse", "Eventhouse", "Environment", "KQLDatabase", "KQLQueryset",
	"KQLDashboard", "DataPipeline", "Notebook", "SparkJobDefinition",
	"MLExperiment", "MLModel", "Warehouse", "Eventstream", "SQLEndpoint",
	"MirroredWarehouse", "MirroredDatabase", "Reflex", "GraphQLApi",
	"MountedDataFactory", "SQLDatabase", "CopyJob", "VariableLibrary",
	"Dataflow", "ApacheAirflowJob", "WarehouseSnapshot", "DigitalTwinBuilder",
	"DigitalTwinBuilderFlow", "MirroredAzureDatabricksCatalog", "Map",
	"AnomalyDetector", "UserDataFunction", "GraphModel", "GraphQuerySet",
	"SnowflakeDatabase", "OperationsAgent", "CosmosDBDatabase", "Ontology",
	"EventSchemaSet", "DataAgent", "MirroredCatalog", "AppBackend", "OrgApp",
	"OrgAppAudience", "DataBuildToolJob", "AzureDatabricksStorage",
}

// canonicalItemTypes maps a lowercased type name to its canonical spelling.
var canonicalItemTypes = func() map[string]string {
	m := make(map[string]string, len(itemTypes))
	for _, t := range itemTypes {
		m[strings.ToLower(t)] = t
	}
	return m
}()

// CanonicalItemType resolves a caller-supplied item type to its canonical
// spelling, reporting whether it is a known type.
//
// Matching is case-insensitive and the canonical spelling is what gets
// stored: item display names are already compared case-insensitively per
// (workspace, type), so letting `notebook` and `Notebook` become two distinct
// types would quietly break that uniqueness rule.
func CanonicalItemType(t string) (string, bool) {
	c, ok := canonicalItemTypes[strings.ToLower(strings.TrimSpace(t))]
	return c, ok
}
