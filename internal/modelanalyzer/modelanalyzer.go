package modelanalyzer

import (
	"context"

	inferno "github.com/llm-inferno/optimizer/pkg/core"
)

type ModelAnalyzer struct {
	systemData *inferno.System
}

func NewModelAnalyzer(systemData *inferno.System) *ModelAnalyzer {
	return &ModelAnalyzer{systemData: systemData}
}

func (ma *ModelAnalyzer) AnalyzeModel(ctx context.Context, variantID string) (map[string]*inferno.Allocation, error) {
	result := make(map[string]*inferno.Allocation)
	for _, accelerator := range ma.systemData.Accelerators() {
		acceleratorName := accelerator.Name()
		if alloc := inferno.CreateAllocation(variantID, acceleratorName); alloc != nil {
			result[acceleratorName] = alloc
		}
	}
	return result, nil
}
