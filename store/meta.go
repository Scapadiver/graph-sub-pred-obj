package store

import (
	"fmt"

	aero "github.com/aerospike/aerospike-client-go/v8"
)

// MetaSetName holds generator bookkeeping, kept apart from the triples set.
const MetaSetName = "meta"

func (gs *GraphStore) counterKey(name string) (*aero.Key, aero.Error) {
	return aero.NewKey(gs.Namespace, MetaSetName, name)
}

// GetCounter returns the named counter, or 0 if it has never been set.
func (gs *GraphStore) GetCounter(name string) (int64, error) {
	key, err := gs.counterKey(name)
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

// ReserveCounter atomically adds n to the named counter and returns the new
// value, so the caller owns the range (value-n, value]. Safe across hosts.
func (gs *GraphStore) ReserveCounter(name string, n int64) (int64, error) {
	key, err := gs.counterKey(name)
	if err != nil {
		return 0, err
	}
	rec, aerr := gs.Client.Operate(gs.WritePolicy, key,
		aero.AddOp(aero.NewBin("value", n)),
		aero.GetBinOp("value"),
	)
	if aerr != nil {
		return 0, aerr
	}
	switch v := rec.Bins["value"].(type) {
	case int:
		return int64(v), nil
	case aero.OpResults:
		if last, ok := v[len(v)-1].(int); ok {
			return int64(last), nil
		}
	case []interface{}:
		if last, ok := v[len(v)-1].(int); ok {
			return int64(last), nil
		}
	}
	return 0, fmt.Errorf("unexpected counter value %#v", rec.Bins["value"])
}

// RaiseCounter sets the named counter to at least min. Not atomic with
// concurrent reservations; use only while no loads are running.
func (gs *GraphStore) RaiseCounter(name string, min int64) error {
	cur, err := gs.GetCounter(name)
	if err != nil {
		return err
	}
	if cur >= min {
		return nil
	}
	_, err = gs.ReserveCounter(name, min-cur)
	return err
}
