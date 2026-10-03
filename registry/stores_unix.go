//go:build unix

package registry

import (
	"encoding/json"

	"github.com/yaad-index/bonyan/approval"
	"github.com/yaad-index/bonyan/runstore"
)

func registerDirStores(r *Registry) {
	r.approvals.factories[ApprovalsDir] = func(options json.RawMessage) (approval.Store, error) {
		path, err := pathOption(options)
		if err != nil {
			return nil, err
		}
		return approval.OpenDir(path)
	}
	r.runStores.factories[RunStoreDir] = func(options json.RawMessage) (runstore.Store, error) {
		path, err := pathOption(options)
		if err != nil {
			return nil, err
		}
		return runstore.OpenDir(path)
	}
}
