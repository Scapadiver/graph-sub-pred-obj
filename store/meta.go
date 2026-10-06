package store

import (
	aero "github.com/aerospike/aerospike-client-go/v8"
)

// MetaSetName holds generator bookkeeping, kept apart from the triples set.
const MetaSetName = "meta"

// GetCounter returns the named counter, or 0 if it has never been set.
func (gs *GraphStore) GetCounter(name string) (int64, error) {
	key, err := aero.NewKey(gs.Namespace, MetaSetName, name)
	if err != nil {
		return 0, err
	}
	rec, err := gs.Client.Get(nil, key, "value")
	if err != nil {
		if err.Matches(aero.ErrKeyNotFound.ResultCode) {
			return 0, nil
		}
		return 0, err
	}
	if v, ok := rec.Bins["value"].(int); ok {
		return int64(v), nil
	}
	return 0, nil
}

// SetCounter stores the named counter.
func (gs *GraphStore) SetCounter(name string, value int64) error {
	key, err := aero.NewKey(gs.Namespace, MetaSetName, name)
	if err != nil {
		return err
	}
	return gs.Client.Put(gs.WritePolicy, key, aero.BinMap{"value": value})
}
