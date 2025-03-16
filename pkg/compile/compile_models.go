package compile

import (
	"fmt"
	"slices"
	"strings"

	"github.com/kaloseia/clone"
	"github.com/kaloseia/go-util/core"
	"github.com/kaloseia/go-util/strcase"
	"github.com/kaloseia/morphe-go/pkg/registry"
	"github.com/kaloseia/morphe-go/pkg/yaml"
	"github.com/kaloseia/morphe-go/pkg/yamlops"
	"github.com/kaloseia/plugin-morphe-psql-types/pkg/compile/cfg"
	"github.com/kaloseia/plugin-morphe-psql-types/pkg/compile/hook"
	"github.com/kaloseia/plugin-morphe-psql-types/pkg/psqldef"
	"github.com/kaloseia/plugin-morphe-psql-types/pkg/typemap"
)

func AllMorpheModelsToPSQLTables(config MorpheCompileConfig, r *registry.Registry) (map[string][]*psqldef.Table, error) {
	allModelTableDefs := map[string][]*psqldef.Table{}
	for modelName, model := range r.GetAllModels() {
		modelTables, modelErr := MorpheModelToPSQLTables(config, r, model)
		if modelErr != nil {
			return nil, modelErr
		}
		allModelTableDefs[modelName] = modelTables
	}
	return allModelTableDefs, nil
}

func MorpheModelToPSQLTables(config MorpheCompileConfig, r *registry.Registry, model yaml.Model) ([]*psqldef.Table, error) {
	morpheConfig, model, compileStartErr := triggerCompileMorpheModelStart(config.ModelHooks, config.MorpheConfig, model)
	if compileStartErr != nil {
		return nil, triggerCompileMorpheModelFailure(config.ModelHooks, morpheConfig, model, compileStartErr)
	}
	config.MorpheConfig = morpheConfig

	allModelTables, tablesErr := morpheModelToPSQLTables(config.MorpheConfig, r, model)
	if tablesErr != nil {
		return nil, triggerCompileMorpheModelFailure(config.ModelHooks, morpheConfig, model, tablesErr)
	}

	allModelTables, compileSuccessErr := triggerCompileMorpheModelSuccess(config.ModelHooks, allModelTables)
	if compileSuccessErr != nil {
		return nil, triggerCompileMorpheModelFailure(config.ModelHooks, morpheConfig, model, compileSuccessErr)
	}
	return allModelTables, nil
}

func morpheModelToPSQLTables(config cfg.MorpheConfig, r *registry.Registry, model yaml.Model) ([]*psqldef.Table, error) {
	validateConfigErr := config.Validate()
	if validateConfigErr != nil {
		return nil, validateConfigErr
	}
	validateMorpheErr := model.Validate(r.GetAllEnums())
	if validateMorpheErr != nil {
		return nil, validateMorpheErr
	}

	schema := config.MorpheModelsConfig.Schema
	modelName := model.Name
	tableName := GetTableNameFromModel(modelName)

	var typeMap map[yaml.ModelFieldType]psqldef.PSQLType
	var relatedTypeMap map[yaml.ModelFieldType]psqldef.PSQLType

	if config.MorpheModelsConfig.UseBigSerial {
		typeMap = typemap.MorpheModelFieldToPSQLFieldBigSerial
		relatedTypeMap = typemap.MorpheModelFieldToPSQLFieldBigSerialForeign
	} else {
		typeMap = typemap.MorpheModelFieldToPSQLField
		relatedTypeMap = typemap.MorpheModelFieldToPSQLFieldForeign
	}

	primaryID, primaryIDExists := model.Identifiers["primary"]
	if !primaryIDExists {
		return nil, fmt.Errorf("no primary identifier set for model '%s'", model.Name)
	}

	fieldColumns, enumForeignKeys, fieldColumnsErr := getColumnsForModelFields(config, r, typeMap, tableName, primaryID, model.Fields)
	if fieldColumnsErr != nil {
		return nil, fieldColumnsErr
	}

	relatedColumns, relatedColumnsErr := getColumnsForModelRelations(r, relatedTypeMap, model.Related)
	if relatedColumnsErr != nil {
		return nil, relatedColumnsErr
	}

	modelTable := psqldef.Table{
		Schema:            schema,
		Name:              tableName,
		Columns:           append(fieldColumns, relatedColumns...),
		ForeignKeys:       enumForeignKeys,
		Indices:           []psqldef.Index{},
		UniqueConstraints: []psqldef.UniqueConstraint{},
	}

	relationForeignKeys, foreignKeysErr := getForeignKeysForModelRelations(schema, tableName, r, model.Related)
	if foreignKeysErr != nil {
		return nil, foreignKeysErr
	}
	modelTable.ForeignKeys = append(modelTable.ForeignKeys, relationForeignKeys...)

	indices := getIndicesForForeignKeys(tableName, modelTable.ForeignKeys)
	modelTable.Indices = indices

	// Apply spec-compliant processing to the model table
	addUniqueIndicesFromIdentifiers(&modelTable, model.Identifiers)
	quoteReservedColumnNames(&modelTable)
	ensureNamedForeignKeyConstraints(&modelTable)

	junctionTables, junctionTablesErr := getJunctionTablesForForManyRelations(schema, r, model)
	if junctionTablesErr != nil {
		return nil, junctionTablesErr
	}

	// Process junction tables as well
	for tableIdx := range junctionTables {
		quoteReservedColumnNames(junctionTables[tableIdx])
		ensureNamedForeignKeyConstraints(junctionTables[tableIdx])
	}

	tables := []*psqldef.Table{&modelTable}
	if len(junctionTables) > 0 {
		tables = append(tables, junctionTables...)
	}

	return tables, nil
}

func getColumnsForModelFields(config cfg.MorpheConfig, r *registry.Registry, typeMap map[yaml.ModelFieldType]psqldef.PSQLType, tableName string, primaryID yaml.ModelIdentifier, modelFields map[string]yaml.ModelField) ([]psqldef.Column, []psqldef.ForeignKey, error) {
	columns := []psqldef.Column{}
	enumForeignKeys := []psqldef.ForeignKey{}

	modelFieldNames := core.MapKeysSorted(modelFields)
	for _, fieldName := range modelFieldNames {
		field := modelFields[fieldName]
		columnName := GetColumnNameFromField(fieldName)

		columnType, supported := typeMap[field.Type]
		if supported {
			column := psqldef.Column{
				Name:       columnName,
				Type:       columnType,
				NotNull:    false,
				PrimaryKey: slices.Index(primaryID.Fields, fieldName) != -1,
				Default:    "",
			}
			columns = append(columns, column)
			continue
		}

		enumType, enumErr := r.GetEnum(string(field.Type))
		if enumErr != nil {
			return nil, nil, fmt.Errorf("morphe model field '%s' has unsupported type '%s'", fieldName, field.Type)
		}

		columnName = columnName + "_id"
		enumTableName := Pluralize(strcase.ToSnakeCaseLower(enumType.Name))

		foreignKey := psqldef.ForeignKey{
			Schema:         config.MorpheModelsConfig.Schema,
			Name:           GetForeignKeyConstraintName(tableName, columnName),
			TableName:      tableName,
			ColumnNames:    []string{columnName},
			RefTableName:   enumTableName,
			RefColumnNames: []string{"id"},
			OnDelete:       "CASCADE",
			OnUpdate:       "",
		}
		enumForeignKeys = append(enumForeignKeys, foreignKey)

		column := psqldef.Column{
			Name:       columnName,
			Type:       psqldef.PSQLTypeInteger,
			NotNull:    true,
			PrimaryKey: slices.Index(primaryID.Fields, fieldName) != -1,
			Default:    "",
		}
		columns = append(columns, column)
	}

	return columns, enumForeignKeys, nil
}

func getColumnsForModelRelations(r *registry.Registry, typeMap map[yaml.ModelFieldType]psqldef.PSQLType, relatedModels map[string]yaml.ModelRelation) ([]psqldef.Column, error) {
	columns := []psqldef.Column{}

	relatedModelNames := core.MapKeysSorted(relatedModels)
	for _, relatedModelName := range relatedModelNames {
		modelRelation := relatedModels[relatedModelName]
		relationType := modelRelation.Type
		relatedModel, modelErr := r.GetModel(relatedModelName)
		if modelErr != nil {
			return nil, modelErr
		}
		primaryID, hasPrimary := relatedModel.Identifiers["primary"]
		if !hasPrimary {
			return nil, fmt.Errorf("related model %s has no primary identifier", relatedModelName)
		}

		if len(primaryID.Fields) != 1 {
			return nil, fmt.Errorf("related entity %s primary identifier must have exactly one field", relatedModelName)
		}

		targetPrimaryIdName := primaryID.Fields[0]
		targetPrimaryIdField, primaryFieldExists := relatedModel.Fields[targetPrimaryIdName]
		if !primaryFieldExists {
			return nil, fmt.Errorf("related entity %s primary identifier field %s not found", relatedModelName, targetPrimaryIdName)
		}

		if yamlops.IsRelationFor(relationType) && yamlops.IsRelationOne(relationType) {
			columnName := GetForeignKeyColumnName(relatedModelName, targetPrimaryIdName)

			columnType, supported := typeMap[targetPrimaryIdField.Type]
			if !supported {
				return nil, fmt.Errorf("morphe related model field '%s' has unsupported type '%s'", targetPrimaryIdName, targetPrimaryIdField.Type)
			}

			column := psqldef.Column{
				Name:       columnName,
				Type:       columnType,
				NotNull:    true,
				PrimaryKey: false,
				Default:    "",
			}

			columns = append(columns, column)
		}
	}

	return columns, nil
}

func getForeignKeysForModelRelations(schema string, tableName string, r *registry.Registry, relatedModels map[string]yaml.ModelRelation) ([]psqldef.ForeignKey, error) {
	foreignKeys := []psqldef.ForeignKey{}

	relatedModelNames := core.MapKeysSorted(relatedModels)
	for _, relatedModelName := range relatedModelNames {
		modelRelation := relatedModels[relatedModelName]
		relationType := modelRelation.Type
		relatedModel, modelErr := r.GetModel(relatedModelName)
		if modelErr != nil {
			return nil, modelErr
		}
		primaryID, hasPrimary := relatedModel.Identifiers["primary"]
		if !hasPrimary {
			return nil, fmt.Errorf("related model %s has no primary identifier", relatedModelName)
		}

		if len(primaryID.Fields) != 1 {
			return nil, fmt.Errorf("related entity %s primary identifier must have exactly one field", relatedModelName)
		}

		targetPrimaryIdName := primaryID.Fields[0]
		_, primaryFieldExists := relatedModel.Fields[targetPrimaryIdName]
		if !primaryFieldExists {
			return nil, fmt.Errorf("related entity %s primary identifier field %s not found", relatedModelName, targetPrimaryIdName)
		}

		if yamlops.IsRelationFor(relationType) && yamlops.IsRelationOne(relationType) {
			columnName := GetForeignKeyColumnName(relatedModelName, targetPrimaryIdName)
			refTableName := GetTableNameFromModel(relatedModelName)
			refColumnName := GetColumnNameFromField(targetPrimaryIdName)

			foreignKey := psqldef.ForeignKey{
				Schema:         schema,
				Name:           GetForeignKeyConstraintName(tableName, columnName),
				TableName:      tableName,
				ColumnNames:    []string{columnName},
				RefTableName:   refTableName,
				RefColumnNames: []string{refColumnName},
				OnDelete:       "CASCADE",
				OnUpdate:       "",
			}

			foreignKeys = append(foreignKeys, foreignKey)
		}
	}

	return foreignKeys, nil
}

func getIndicesForForeignKeys(tableName string, foreignKeys []psqldef.ForeignKey) []psqldef.Index {
	indices := []psqldef.Index{}

	for _, fk := range foreignKeys {
		for _, columnName := range fk.ColumnNames {
			index := psqldef.Index{
				Name:      GetIndexName(tableName, columnName),
				TableName: tableName,
				Columns:   []string{columnName},
				IsUnique:  false,
			}

			indices = append(indices, index)
		}
	}

	return indices
}

func triggerCompileMorpheModelStart(modelHooks hook.CompileMorpheModel, config cfg.MorpheConfig, model yaml.Model) (cfg.MorpheConfig, yaml.Model, error) {
	if modelHooks.OnCompileMorpheModelStart == nil {
		return config, model, nil
	}

	updatedConfig, updatedModel, startErr := modelHooks.OnCompileMorpheModelStart(config, model)
	if startErr != nil {
		return cfg.MorpheConfig{}, yaml.Model{}, startErr
	}

	return updatedConfig, updatedModel, nil
}

func triggerCompileMorpheModelSuccess(hooks hook.CompileMorpheModel, allModelTables []*psqldef.Table) ([]*psqldef.Table, error) {
	if hooks.OnCompileMorpheModelSuccess == nil {
		return allModelTables, nil
	}
	if allModelTables == nil {
		return nil, ErrNoModelTables
	}
	allModelTablesClone := clone.DeepCloneSlicePointers(allModelTables)

	allModelTables, successErr := hooks.OnCompileMorpheModelSuccess(allModelTablesClone)
	if successErr != nil {
		return nil, successErr
	}
	return allModelTables, nil
}

func triggerCompileMorpheModelFailure(hooks hook.CompileMorpheModel, morpheConfig cfg.MorpheConfig, model yaml.Model, failureErr error) error {
	if hooks.OnCompileMorpheModelFailure == nil {
		return failureErr
	}

	return hooks.OnCompileMorpheModelFailure(morpheConfig, model.DeepClone(), failureErr)
}

// getJunctionTablesForForManyRelations creates junction tables for ForMany relationships
func getJunctionTablesForForManyRelations(schema string, r *registry.Registry, model yaml.Model) ([]*psqldef.Table, error) {
	junctionTables := []*psqldef.Table{}
	modelName := model.Name
	tableName := GetTableNameFromModel(modelName)

	// Get primary ID field for this model
	primaryID, hasPrimary := model.Identifiers["primary"]
	if !hasPrimary {
		return nil, fmt.Errorf("model %s has no primary identifier", modelName)
	}
	if len(primaryID.Fields) != 1 {
		return nil, fmt.Errorf("model %s primary identifier must have exactly one field", modelName)
	}
	primaryIdName := primaryID.Fields[0]

	relatedModelNames := core.MapKeysSorted(model.Related)
	for _, relatedModelName := range relatedModelNames {
		modelRelation := model.Related[relatedModelName]
		relationType := modelRelation.Type

		if yamlops.IsRelationFor(relationType) && yamlops.IsRelationMany(relationType) {
			relatedModel, modelErr := r.GetModel(relatedModelName)
			if modelErr != nil {
				return nil, modelErr
			}

			// Get primary ID field for related model
			relatedPrimaryID, hasRelatedPrimary := relatedModel.Identifiers["primary"]
			if !hasRelatedPrimary {
				return nil, fmt.Errorf("related model %s has no primary identifier", relatedModelName)
			}
			if len(relatedPrimaryID.Fields) != 1 {
				return nil, fmt.Errorf("related model %s primary identifier must have exactly one field", relatedModelName)
			}
			relatedPrimaryIdName := relatedPrimaryID.Fields[0]

			// Create junction table
			junctionTableName := GetJunctionTableName(modelName, relatedModelName)

			// Create column names
			sourceColumnName := GetForeignKeyColumnName(modelName, primaryIdName)
			targetColumnName := GetForeignKeyColumnName(relatedModelName, relatedPrimaryIdName)

			// Create columns
			columns := []psqldef.Column{
				{
					Name:       "id",
					Type:       psqldef.PSQLTypeSerial,
					PrimaryKey: true,
				},
				{
					Name: sourceColumnName,
					Type: psqldef.PSQLTypeInteger,
				},
				{
					Name: targetColumnName,
					Type: psqldef.PSQLTypeInteger,
				},
			}

			// Create foreign keys
			foreignKeys := []psqldef.ForeignKey{
				{
					Schema:       schema,
					Name:         GetJunctionTableForeignKeyConstraintName(junctionTableName, modelName, primaryIdName),
					TableName:    junctionTableName,
					ColumnNames:  []string{sourceColumnName},
					RefTableName: tableName,
					RefColumnNames: []string{
						GetColumnNameFromField(primaryIdName),
					},
					OnDelete: "CASCADE",
				},
				{
					Schema:       schema,
					Name:         GetJunctionTableForeignKeyConstraintName(junctionTableName, relatedModelName, relatedPrimaryIdName),
					TableName:    junctionTableName,
					ColumnNames:  []string{targetColumnName},
					RefTableName: GetTableNameFromModel(relatedModelName),
					RefColumnNames: []string{
						GetColumnNameFromField(relatedPrimaryIdName),
					},
					OnDelete: "CASCADE",
				},
			}

			// Create unique constraint
			uniqueConstraints := []psqldef.UniqueConstraint{
				{
					Name: GetJunctionTableUniqueConstraintName(
						junctionTableName,
						modelName, primaryIdName,
						relatedModelName, relatedPrimaryIdName,
					),
					TableName: junctionTableName,
					ColumnNames: []string{
						sourceColumnName,
						targetColumnName,
					},
				},
			}

			// Create indices for foreign keys
			indices := getIndicesForForeignKeys(junctionTableName, foreignKeys)

			// Create junction table
			junctionTable := &psqldef.Table{
				Schema:            schema,
				Name:              junctionTableName,
				Columns:           columns,
				ForeignKeys:       foreignKeys,
				Indices:           indices,
				UniqueConstraints: uniqueConstraints,
			}

			junctionTables = append(junctionTables, junctionTable)
		}
	}

	return junctionTables, nil
}

// addUniqueIndicesFromIdentifiers adds unique indices for model identifiers
func addUniqueIndicesFromIdentifiers(table *psqldef.Table, identifiers map[string]yaml.ModelIdentifier) {
	tableName := table.Name

	// Add unique indices for identifiers
	for idName, identifier := range identifiers {
		if idName == "primary" {
			continue
		}
		columnNames := make([]string, len(identifier.Fields))
		for fieldIdx, field := range identifier.Fields {
			columnNames[fieldIdx] = GetColumnNameFromField(field)
		}

		var indexName string
		if len(columnNames) == 1 {
			indexName = fmt.Sprintf("idx_%s_%s", tableName, columnNames[0])
		} else {
			indexName = fmt.Sprintf("idx_%s_%s", tableName, strings.Join(columnNames, "_"))
		}

		table.Indices = append(table.Indices, psqldef.Index{
			Name:      indexName,
			TableName: tableName,
			Columns:   columnNames,
			IsUnique:  true,
		})
	}
}

// quoteReservedColumnNames quotes column names that are PostgreSQL reserved words
func quoteReservedColumnNames(table *psqldef.Table) {
	reservedWords := map[string]bool{
		"name":    true,
		"type":    true,
		"user":    true,
		"case":    true,
		"when":    true,
		"then":    true,
		"else":    true,
		"end":     true,
		"null":    true,
		"true":    true,
		"false":   true,
		"select":  true,
		"insert":  true,
		"update":  true,
		"delete":  true,
		"from":    true,
		"where":   true,
		"group":   true,
		"order":   true,
		"limit":   true,
		"offset":  true,
		"join":    true,
		"on":      true,
		"using":   true,
		"and":     true,
		"or":      true,
		"not":     true,
		"between": true,
		"alter":   true,
		"table":   true,
		"index":   true,
		"unique":  true,
		"primary": true,
		"foreign": true,
		"key":     true,
	}

	for idxIdx, idx := range table.Indices {
		for colIdx, colName := range idx.Columns {
			if reservedWords[colName] {
				idx.Columns[colIdx] = fmt.Sprintf("\"%s\"", colName)
				table.Indices[idxIdx] = idx
			}
		}
	}
}

// ensureNamedForeignKeyConstraints ensures all foreign keys have proper names and CASCADE behavior
func ensureNamedForeignKeyConstraints(table *psqldef.Table) {
	for fkIdx, fk := range table.ForeignKeys {
		if fk.Name == "" {
			fk.Name = GetForeignKeyConstraintName(table.Name, fk.ColumnNames[0])
			table.ForeignKeys[fkIdx] = fk
		}

		// Ensure CASCADE behavior as per spec
		if fk.OnDelete == "" {
			fk.OnDelete = "CASCADE"
			table.ForeignKeys[fkIdx] = fk
		}
	}
}
