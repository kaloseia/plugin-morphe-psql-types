package psqldef

import "github.com/kalo-build/clone"

// Table represents a PSQL table for a model
type Table struct {
	Schema            string
	Name              string
	Columns           []TableColumn
	Indices           []Index
	ForeignKeys       []ForeignKey
	UniqueConstraints []UniqueConstraint
	SeedData          []InsertStatement
}

// DeepClone creates a deep copy of the Table
func (t Table) DeepClone() Table {
	tableCopy := Table{
		Schema:            t.Schema,
		Name:              t.Name,
		Columns:           clone.DeepCloneSlice(t.Columns),
		Indices:           clone.DeepCloneSlice(t.Indices),
		ForeignKeys:       clone.DeepCloneSlice(t.ForeignKeys),
		UniqueConstraints: clone.DeepCloneSlice(t.UniqueConstraints),
		SeedData:          clone.DeepCloneSlice(t.SeedData),
	}

	return tableCopy
}
