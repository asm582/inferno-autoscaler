package modelanalyzer

import (
	"context"

	inferno "github.com/llm-inferno/optimizer/pkg/core"
)

type ModelAnalyzer struct {
	system *inferno.System
}

func NewModelAnalyzer(system *inferno.System) *ModelAnalyzer {
	return &ModelAnalyzer{system: system}
}

func (ma *ModelAnalyzer) AnalyzeModel(ctx context.Context, variantID string) (map[string]*inferno.Allocation, error) {
	result := make(map[string]*inferno.Allocation)
	for _, accelerator := range ma.system.Accelerators() {
		acceleratorName := accelerator.Name()
		if alloc := inferno.CreateAllocation(variantID, acceleratorName); alloc != nil {
			result[acceleratorName] = alloc
		}
	}
	return result, nil
}
