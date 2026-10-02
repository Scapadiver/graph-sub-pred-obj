package store

import (
	"graph-sub-pred-obj/model"

	aero "github.com/aerospike/aerospike-client-go/v8"
)

// GetTriple performs a direct primary key lookup (fastest path, sub-ms).
func (gs *GraphStore) GetTriple(subject, predicate, object string) (*model.Triple, error) {
	t := &model.Triple{Subject: subject, Predicate: predicate, Object: object}
	key, err := aero.NewKey(gs.Namespace, SetName, t.Key())
	if err != nil {
		return nil, err
	}
	rec, err := gs.Client.Get(nil, key)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, nil
	}
	return recordToTriple(rec), nil
}

// QueryBySubject returns all triples for a given subject (S?? pattern).
func (gs *GraphStore) QueryBySubject(subject string) ([]*model.Triple, error) {
	return gs.queryByBin("subject", subject, nil)
}

// QueryByPredicate returns all triples with a given predicate (?P? pattern).
func (gs *GraphStore) QueryByPredicate(predicate string) ([]*model.Triple, error) {
	return gs.queryByBin("predicate", predicate, nil)
}

// QueryByObject returns all triples pointing to a given object (??O pattern).
func (gs *GraphStore) QueryByObject(object string) ([]*model.Triple, error) {
	return gs.queryByBin("object", object, nil)
}

// QuerySP returns triples matching subject+predicate (SP? pattern).
func (gs *GraphStore) QuerySP(subject, predicate string) ([]*model.Triple, error) {
	exp := aero.ExpEq(
		aero.ExpStringBin("predicate"),
		aero.ExpStringVal(predicate),
	)
	return gs.queryByBin("subject", subject, exp)
}

// QueryPO returns triples matching predicate+object (?PO pattern).
func (gs *GraphStore) QueryPO(predicate, object string) ([]*model.Triple, error) {
	exp := aero.ExpEq(
		aero.ExpStringBin("object"),
		aero.ExpStringVal(object),
	)
	return gs.queryByBin("predicate", predicate, exp)
}

// QuerySO returns triples matching subject+object (S?O pattern).
func (gs *GraphStore) QuerySO(subject, object string) ([]*model.Triple, error) {
	exp := aero.ExpEq(
		aero.ExpStringBin("object"),
		aero.ExpStringVal(object),
	)
	return gs.queryByBin("subject", subject, exp)
}

// QueryByBinWithFilter is an exported version of queryByBin for use by graph package.
func (gs *GraphStore) QueryByBinWithFilter(binName, value string, filterExp *aero.Expression) ([]*model.Triple, error) {
	return gs.queryByBin(binName, value, filterExp)
}

// queryByBin runs a secondary index query on a single bin with an optional server-side filter expression.
func (gs *GraphStore) queryByBin(binName, value string, filterExp *aero.Expression) ([]*model.Triple, error) {
	stmt := aero.NewStatement(gs.Namespace, SetName)
	stmt.SetFilter(aero.NewEqualFilter(binName, value))

	qp := *gs.QueryPolicy // copy to avoid mutation
	if filterExp != nil {
		qp.FilterExpression = filterExp
	}

	rs, err := gs.Client.Query(&qp, stmt)
	if err != nil {
		return nil, err
	}
	defer rs.Close()

	var results []*model.Triple
	for r := range rs.Results() {
		if r.Err != nil {
			return nil, r.Err
		}
		results = append(results, recordToTriple(r.Record))
	}
	return results, nil
}

// ScanAll returns all triples in the store. Use sparingly.
func (gs *GraphStore) ScanAll() ([]*model.Triple, error) {
	sp := aero.NewScanPolicy()
	rs, err := gs.Client.ScanAll(sp, gs.Namespace, SetName)
	if err != nil {
		return nil, err
	}
	defer rs.Close()

	var results []*model.Triple
	for r := range rs.Results() {
		if r.Err != nil {
			return nil, r.Err
		}
		results = append(results, recordToTriple(r.Record))
	}
	return results, nil
}

func recordToTriple(rec *aero.Record) *model.Triple {
	t := &model.Triple{}

	if v, ok := rec.Bins["subject"].(string); ok {
		t.Subject = v
	}
	if v, ok := rec.Bins["predicate"].(string); ok {
		t.Predicate = v
	}
	if v, ok := rec.Bins["object"].(string); ok {
		t.Object = v
	}
	if v, ok := rec.Bins["props"].(map[interface{}]interface{}); ok {
		t.Props = model.ConvertMapKeys(v)
	}

	return t
}
