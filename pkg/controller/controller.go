package controller

import (
	//todo change name
	"github.com/choerodon/choerodon-cluster-agent/pkg/util/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// AddToManagerFuncs is a list of functions to add all Controllers to the Manager
var AddToManagerFuncs []func(manager.Manager, *controller.Args) error

// AddToManager adds all Controllers to the Manager
func AddToManager(m manager.Manager, args *controller.Args) error {
	for _, f := range AddToManagerFuncs {
		if err := f(m, args); err != nil {
			return err
		}
	}
	return nil
}
