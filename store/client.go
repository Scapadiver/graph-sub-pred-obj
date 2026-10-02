package store

import (
	aero "github.com/aerospike/aerospike-client-go/v8"
)

const SetName = "triples"

// GraphStore wraps an Aerospike client with preconfigured policies for graph operations.
type GraphStore struct {
	Client      *aero.Client
	Namespace   string
	WritePolicy *aero.WritePolicy
	QueryPolicy *aero.QueryPolicy
	BatchPolicy *aero.BatchPolicy
}

// NewGraphStore connects to Aerospike and returns a configured GraphStore.
func NewGraphStore(host string, port int, namespace string) (*GraphStore, error) {
	client, err := aero.NewClient(host, port)
	if err != nil {
		return nil, err
	}

	wp := aero.NewWritePolicy(0, 0)
	wp.RecordExistsAction = aero.UPDATE // upsert semantics

	qp := aero.NewQueryPolicy()

	bp := aero.NewBatchPolicy()

	return &GraphStore{
		Client:      client,
		Namespace:   namespace,
		WritePolicy: wp,
		QueryPolicy: qp,
		BatchPolicy: bp,
	}, nil
}

// Close shuts down the Aerospike client connection.
func (gs *GraphStore) Close() {
	gs.Client.Close()
}
