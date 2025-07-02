package controller

import (
	"os"

	infernoConfig "github.com/llm-inferno/optimizer/pkg/config"
	"github.com/llm-inferno/optimizer/pkg/utils"
)

var DataPath string

// read static data
func readStaticData() (*infernoConfig.SystemData, error) {

	systemData := &infernoConfig.SystemData{}

	// read data from files in data path
	if DataPath = os.Getenv(DataPathEnvName); DataPath == "" {
		DataPath = DefaultDataPath
	}

	// read accelerator data
	fn_acc := DataPath + AcceleratorFileName
	bytes_acc, err_acc := os.ReadFile(fn_acc)
	if err_acc != nil {
		return systemData, err_acc
	}
	if d, err := utils.FromDataToSpec(bytes_acc, infernoConfig.AcceleratorData{}); err == nil {
		systemData.Spec.Accelerators = *d
	} else {
		return systemData, err
	}

	// read model data
	fn_mod := DataPath + ModelFileName
	bytes_mod, err_mod := os.ReadFile(fn_mod)
	if err_mod != nil {
		return systemData, err_mod
	}
	if d, err := utils.FromDataToSpec(bytes_mod, infernoConfig.ModelData{}); err == nil {
		systemData.Spec.Models = *d
	} else {
		return systemData, err
	}

	// read service class data
	fn_svc := DataPath + ServiceClassFileName
	bytes_svc, err_svc := os.ReadFile(fn_svc)
	if err_svc != nil {
		return systemData, err_svc
	}
	if d, err := utils.FromDataToSpec(bytes_svc, infernoConfig.ServiceClassData{}); err == nil {
		systemData.Spec.ServiceClasses = *d
	} else {
		return systemData, err
	}

	// read optimizer data
	fn_opt := DataPath + OptimizerFileName
	bytes_opt, err_opt := os.ReadFile(fn_opt)
	if err_opt != nil {
		return systemData, err_opt
	}
	if d, err := utils.FromDataToSpec(bytes_opt, infernoConfig.OptimizerData{}); err == nil {
		systemData.Spec.Optimizer = *d
	} else {
		return systemData, err
	}

	return systemData, nil
}
