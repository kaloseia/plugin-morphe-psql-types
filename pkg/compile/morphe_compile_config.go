package compile

import (
	r "github.com/kaloseia/morphe-go/pkg/registry"
	rcfg "github.com/kaloseia/morphe-go/pkg/registry/cfg"
	"github.com/kaloseia/plugin-morphe-psql-types/pkg/compile/cfg"
	"github.com/kaloseia/plugin-morphe-psql-types/pkg/compile/hook"
	"github.com/kaloseia/plugin-morphe-psql-types/pkg/compile/write"
)

type MorpheCompileConfig struct {
	rcfg.MorpheLoadRegistryConfig
	cfg.MorpheConfig

	RegistryHooks r.LoadMorpheRegistryHooks

	ModelWriter write.PSQLTableWriter
	ModelHooks  hook.CompileMorpheModel

	EnumWriter write.PSQLTableWriter
	EnumHooks  hook.CompileMorpheEnum

	StructureWriter write.PSQLTableWriter
	StructureHooks  hook.CompileMorpheStructure

	WriteTableHooks hook.WritePSQLTable
}

func (config MorpheCompileConfig) Validate() error {
	loadRegistryErr := config.MorpheLoadRegistryConfig.Validate()
	if loadRegistryErr != nil {
		return loadRegistryErr
	}

	morpheCfgErr := config.MorpheConfig.Validate()
	if morpheCfgErr != nil {
		return morpheCfgErr
	}

	return nil
}
