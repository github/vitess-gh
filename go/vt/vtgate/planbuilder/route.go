/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package planbuilder

import (
	"strconv"

	"vitess.io/vitess/go/mysql/collations"
	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
	"vitess.io/vitess/go/vt/vterrors"
	"vitess.io/vitess/go/vt/vtgate/evalengine"
	"vitess.io/vitess/go/vt/vtgate/semantics"

	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/vtgate/engine"
	"vitess.io/vitess/go/vt/vtgate/vindexes"
)

var _ logicalPlan = (*route)(nil)

// route is used to build a Route primitive.
// It's used to build one of the Select routes like
// SelectScatter, etc. Portions of the original Select AST
// are moved into this node, which will be used to build
// the final SQL for this route.
type route struct {
	v3Plan
	order int

	// Redirect may point to another route if this route
	// was merged with it. The Resolve function chases
	// this pointer till the last un-redirected route.
	Redirect *route

	// Select is the AST for the query fragment that will be
	// executed by this route.
	Select sqlparser.SelectStatement

	// resultColumns represent the columns returned by this route.
	resultColumns []*resultColumn

	// weight_string keeps track of the weight_string expressions
	// that were added additionally for each column. These expressions
	// are added to be used for collation of text columns.
	weightStrings map[*resultColumn]int

	// substitutions contain the list of table expressions that
	// have to be substituted in the route's query.
	substitutions []*tableSubstitution

	// condition stores the AST condition that will be used
	// to resolve the ERoute Values field.
	condition sqlparser.Expr

	// eroute is the primitive being built.
	eroute *engine.Route
}

type tableSubstitution struct {
	newExpr, oldExpr *sqlparser.AliasedTableExpr
}

func newRoute(stmt sqlparser.SelectStatement) (*route, *symtab) {
	rb := &route{
		Select:        stmt,
		order:         1,
		weightStrings: make(map[*resultColumn]int),
	}
	return rb, newSymtabWithRoute(rb)
}

// Resolve resolves redirects, and returns the last
// un-redirected route.
func (rb *route) Resolve() *route {
	for rb.Redirect != nil {
		rb = rb.Redirect
	}
	return rb
}

// Order implements the logicalPlan interface
func (rb *route) Order() int {
	return rb.order
}

// Reorder implements the logicalPlan interface
func (rb *route) Reorder(order int) {
	rb.order = order + 1
}

// Primitive implements the logicalPlan interface
func (rb *route) Primitive() engine.Primitive {
	return rb.eroute
}

// ResultColumns implements the logicalPlan interface
func (rb *route) ResultColumns() []*resultColumn {
	return rb.resultColumns
}

// PushAnonymous pushes an anonymous expression like '*' or NEXT VALUES
// into the select expression list of the route. This function is
// similar to PushSelect.
func (rb *route) PushAnonymous(expr sqlparser.SelectExpr) *resultColumn {
	// TODO: we should not assume that the query is a SELECT
	sel := rb.Select.(*sqlparser.Select)
	sel.SelectExprs = append(sel.SelectExprs, expr)

	// We just create a place-holder resultColumn. It won't
	// match anything.
	rc := &resultColumn{column: &column{origin: rb}}
	rb.resultColumns = append(rb.resultColumns, rc)

	return rc
}

// SetLimit adds a LIMIT clause to the route.
func (rb *route) SetLimit(limit *sqlparser.Limit) {
	rb.Select.SetLimit(limit)
}

// Wireup implements the logicalPlan interface
func (rb *route) Wireup(plan logicalPlan, jt *jointab) error {
	// Precaution: update ERoute.Values only if it's not set already.
	if rb.eroute.Values == nil {
		// Resolve values stored in the logical plan.
		switch vals := rb.condition.(type) {
		case *sqlparser.ComparisonExpr:
			pv, err := rb.procureValues(plan, jt, vals.Right)
			if err != nil {
				return err
			}
			rb.eroute.Values = []evalengine.Expr{pv}
			vals.Right = sqlparser.ListArg(engine.ListVarName)
		case nil:
			// no-op.
		default:
			pv, err := rb.procureValues(plan, jt, vals)
			if err != nil {
				return err
			}
			rb.eroute.Values = []evalengine.Expr{pv}
		}
	}

	// Fix up the AST.
	_ = sqlparser.Walk(func(node sqlparser.SQLNode) (bool, error) {
		switch node := node.(type) {
		case *sqlparser.Select:
			if len(node.SelectExprs) == 0 {
				node.SelectExprs = []sqlparser.SelectExpr{
					&sqlparser.AliasedExpr{
						Expr: sqlparser.NewIntLiteral("1"),
					},
				}
			}
		case *sqlparser.ComparisonExpr:
			if node.Operator == sqlparser.EqualOp {
				if rb.exprIsValue(node.Left) && !rb.exprIsValue(node.Right) {
					node.Left, node.Right = node.Right, node.Left
				}
			}
		}
		return true, nil
	}, rb.Select)

	// Substitute table names
	for _, sub := range rb.substitutions {
		*sub.oldExpr = *sub.newExpr
	}

	// Generate query while simultaneously resolving values.
	varFormatter := func(buf *sqlparser.TrackedBuffer, node sqlparser.SQLNode) {
		switch node := node.(type) {
		case *sqlparser.ColName:
			if !rb.isLocal(node) {
				joinVar := jt.Procure(plan, node, rb.Order())
				buf.WriteArg(":", joinVar)
				return
			}
		case sqlparser.TableName:
			if !sqlparser.SystemSchema(node.Qualifier.String()) {
				node.Name.Format(buf)
				return
			}
			node.Format(buf)
			return
		}
		node.Format(buf)
	}
	buf := sqlparser.NewTrackedBuffer(varFormatter)
	varFormatter(buf, rb.Select)
	rb.eroute.Query = buf.ParsedQuery().Query
	rb.eroute.FieldQuery = rb.generateFieldQuery(rb.Select, jt)
	return nil
}

// prepareTheAST does minor fixups of the SELECT struct before producing the query string
func (rb *route) prepareTheAST() {
	_ = sqlparser.Walk(func(node sqlparser.SQLNode) (bool, error) {
		switch node := node.(type) {
		case *sqlparser.Select:
			if len(node.SelectExprs) == 0 {
				node.SelectExprs = []sqlparser.SelectExpr{
					&sqlparser.AliasedExpr{
						Expr: sqlparser.NewIntLiteral("1"),
					},
				}
			}
		case *sqlparser.ComparisonExpr:
			// 42 = colName -> colName = 42
			b := node.Operator == sqlparser.EqualOp
			value := sqlparser.IsValue(node.Left)
			name := sqlparser.IsColName(node.Right)
			if b &&
				value &&
				name {
				node.Left, node.Right = node.Right, node.Left
			}
		}
		return true, nil
	}, rb.Select)
}

// procureValues procures and converts the input into
// the expected types for rb.Values.
func (rb *route) procureValues(plan logicalPlan, jt *jointab, val sqlparser.Expr) (evalengine.Expr, error) {
	switch typedVal := val.(type) {
	case sqlparser.ValTuple:
		exprs := make([]evalengine.Expr, 0, len(typedVal))
		for _, item := range typedVal {
			v, err := rb.procureValues(plan, jt, item)
			if err != nil {
				return nil, err
			}
			exprs = append(exprs, v)
		}
		return evalengine.NewTupleExpr(exprs...), nil
	case *sqlparser.ColName:
		joinVar := jt.Procure(plan, typedVal, rb.Order())
		return evalengine.NewBindVar(joinVar, collations.TypedCollation{}), nil
	default:
		return evalengine.Translate(typedVal, semantics.EmptySemTable())
	}
}

func (rb *route) isLocal(col *sqlparser.ColName) bool {
	return col.Metadata.(*column).Origin() == rb
}

// generateFieldQuery generates a query with an impossible where.
// This will be used on the RHS node to fetch field info if the LHS
// returns no result.
func (rb *route) generateFieldQuery(sel sqlparser.SelectStatement, jt *jointab) string {
	formatter := func(buf *sqlparser.TrackedBuffer, node sqlparser.SQLNode) {
		switch node := node.(type) {
		case *sqlparser.ColName:
			if !rb.isLocal(node) {
				_, joinVar := jt.Lookup(node)
				buf.WriteArg(":", joinVar)
				return
			}
		case sqlparser.TableName:
			if !sqlparser.SystemSchema(node.Qualifier.String()) {
				node.Name.Format(buf)
				return
			}
			node.Format(buf)
			return
		}
		sqlparser.FormatImpossibleQuery(buf, node)
	}

	buffer := sqlparser.NewTrackedBuffer(formatter)
	node := buffer.WriteNode(sel)
	query := node.ParsedQuery()
	return query.Query
}

// SupplyVar implements the logicalPlan interface
func (rb *route) SupplyVar(from, to int, col *sqlparser.ColName, varname string) {
	// route is an atomic primitive. So, SupplyVar cannot be
	// called on it.
	panic("BUG: route is an atomic node.")
}

// SupplyCol implements the logicalPlan interface
func (rb *route) SupplyCol(col *sqlparser.ColName) (rc *resultColumn, colNumber int) {
	c := col.Metadata.(*column)
	for i, rc := range rb.resultColumns {
		if rc.column == c {
			return rc, i
		}
	}

	// A new result has to be returned.
	rc = &resultColumn{column: c}
	rb.resultColumns = append(rb.resultColumns, rc)
	// TODO: we should not assume that the query is a SELECT query
	sel := rb.Select.(*sqlparser.Select)
	sel.SelectExprs = append(sel.SelectExprs, &sqlparser.AliasedExpr{Expr: col})
	return rc, len(rb.resultColumns) - 1
}

// SupplyWeightString implements the logicalPlan interface
func (rb *route) SupplyWeightString(colNumber int, alsoAddToGroupBy bool) (weightcolNumber int, err error) {
	rc := rb.resultColumns[colNumber]
	s, ok := rb.Select.(*sqlparser.Select)
	if !ok {
		return 0, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "unexpected AST struct for query")
	}

	aliasExpr, ok := s.SelectExprs[colNumber].(*sqlparser.AliasedExpr)
	if !ok {
		return 0, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "unexpected AST struct for query %T", s.SelectExprs[colNumber])
	}
	weightStringExpr := &sqlparser.FuncExpr{
		Name: sqlparser.NewIdentifierCI("weight_string"),
		Exprs: []sqlparser.SelectExpr{
			&sqlparser.AliasedExpr{
				Expr: aliasExpr.Expr,
			},
		},
	}
	expr := &sqlparser.AliasedExpr{
		Expr: weightStringExpr,
	}
	if alsoAddToGroupBy {
		sel, isSelect := rb.Select.(*sqlparser.Select)
		if !isSelect {
			return 0, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "cannot add weight string in %T", rb.Select)
		}
		sel.AddGroupBy(weightStringExpr)
	}

	if weightcolNumber, ok := rb.weightStrings[rc]; ok {
		return weightcolNumber, nil
	}
	// It's ok to pass nil for pb and logicalPlan because PushSelect doesn't use them.
	// TODO: we are ignoring a potential error here. need to clean this up
	_, _, weightcolNumber, err = planProjection(nil, rb, expr, nil)
	if err != nil {
		return 0, err
	}
	rb.weightStrings[rc] = weightcolNumber
	return weightcolNumber, nil
}

// Rewrite implements the logicalPlan interface
func (rb *route) Rewrite(inputs ...logicalPlan) error {
	if len(inputs) != 0 {
		return vterrors.Errorf(vtrpcpb.Code_INTERNAL, "route: wrong number of inputs")
	}
	return nil
}

// Inputs implements the logicalPlan interface
func (rb *route) Inputs() []logicalPlan {
	return []logicalPlan{}
}

// MergeSubquery returns true if the subquery route could successfully be merged
// with the outer route.
func (rb *route) MergeSubquery(pb *primitiveBuilder, inner *route) bool {
	if rb.SubqueryCanMerge(pb, inner) {
		if inner.eroute.Opcode == engine.DBA && (len(inner.eroute.SysTableTableName) > 0 || len(inner.eroute.SysTableTableSchema) > 0) {
			switch rb.eroute.Opcode {
			case engine.DBA, engine.Reference:
				rb.eroute.SysTableTableSchema = append(rb.eroute.SysTableTableSchema, inner.eroute.SysTableTableSchema...)
				for k, v := range inner.eroute.SysTableTableName {
					if rb.eroute.SysTableTableName == nil {
						rb.eroute.SysTableTableName = map[string]evalengine.Expr{}
					}
					rb.eroute.SysTableTableName[k] = v
				}
				rb.eroute.Opcode = engine.DBA
			default:
				return false
			}
		} else {
			if rb.eroute.Opcode == engine.Reference {
				rb.eroute.RoutingParameters = inner.eroute.RoutingParameters
				rb.condition = inner.condition
			}
		}

		rb.substitutions = append(rb.substitutions, inner.substitutions...)
		inner.Redirect = rb
		return true
	}
	return false
}

// MergeUnion returns true if the rhs route could successfully be merged
// with the rb route.
func (rb *route) MergeUnion(right *route, isDistinct bool) bool {
	if rb.unionCanMerge(right, isDistinct) {
		rb.substitutions = append(rb.substitutions, right.substitutions...)
		right.Redirect = rb
		return true
	}
	return false
}

func (rb *route) isSingleShard() bool {
	return rb.eroute.Opcode.IsSingleShard()
}

// JoinCanMerge, SubqueryCanMerge and unionCanMerge have subtly different behaviors.
// The difference in behavior is around SelectReference.
// It's not worth trying to reuse the code between them.
func (rb *route) JoinCanMerge(pb *primitiveBuilder, rrb *route, ajoin *sqlparser.JoinTableExpr, where sqlparser.Expr) bool {
	if rb.eroute.Keyspace.Name != rrb.eroute.Keyspace.Name {
		return false
	}
	if rrb.eroute.Opcode == engine.Reference {
		// Any opcode can join with a reference table.
		return true
	}
	switch rb.eroute.Opcode {
	case engine.Unsharded:
		return rb.eroute.Opcode == rrb.eroute.Opcode
	case engine.EqualUnique:
		// Check if they target the same shard.
		if rrb.eroute.Opcode == engine.EqualUnique && rb.eroute.Vindex == rrb.eroute.Vindex && valEqual(rb.condition, rrb.condition) {
			return true
		}
	case engine.Reference:
		return true
	case engine.Next:
		return false
	case engine.DBA:
		if rrb.eroute.Opcode != engine.DBA {
			return false
		}
		if where == nil {
			return true
		}
		return ajoin != nil
	}
	if ajoin == nil {
		return false
	}
	for _, filter := range sqlparser.SplitAndExpression(nil, ajoin.Condition.On) {
		if rb.canMergeOnFilter(pb, rrb, filter) {
			return true
		}
	}
	return false
}

func (rb *route) SubqueryCanMerge(pb *primitiveBuilder, inner *route) bool {
	if rb.eroute.Keyspace.Name != inner.eroute.Keyspace.Name {
		return false
	}

	// if either side is a reference table, and we know the other side will only run once,
	// we can just merge them and use the opcode of the other side
	if rb.eroute.Opcode == engine.Reference || inner.eroute.Opcode == engine.Reference {
		return rb.isSingleShard() && inner.isSingleShard()
	}

	switch rb.eroute.Opcode {
	case engine.Unsharded, engine.DBA:
		return rb.eroute.Opcode == inner.eroute.Opcode
	case engine.EqualUnique:
		// Check if they target the same shard.
		if inner.eroute.Opcode == engine.EqualUnique && rb.eroute.Vindex == inner.eroute.Vindex && valEqual(rb.condition, inner.condition) {
			return true
		}
	case engine.Next:
		return false
	}

	switch vals := inner.condition.(type) {
	case *sqlparser.ColName:
		if pb.st.Vindex(vals, rb) == inner.eroute.Vindex {
			return true
		}
	}
	return false
}

func (rb *route) unionCanMerge(other *route, distinct bool) bool {
	if rb.eroute.Keyspace.Name != other.eroute.Keyspace.Name {
		return false
	}
	switch rb.eroute.Opcode {
	case engine.Unsharded, engine.Reference:
		return rb.eroute.Opcode == other.eroute.Opcode
	case engine.DBA:
		return other.eroute.Opcode == engine.DBA &&
			len(rb.eroute.SysTableTableSchema) == 0 &&
			len(rb.eroute.SysTableTableName) == 0 &&
			len(other.eroute.SysTableTableSchema) == 0 &&
			len(other.eroute.SysTableTableName) == 0
	case engine.EqualUnique:
		// Check if they target the same shard.
		if other.eroute.Opcode == engine.EqualUnique && rb.eroute.Vindex == other.eroute.Vindex && valEqual(rb.condition, other.condition) {
			return true
		}
	case engine.Scatter:
		return other.eroute.Opcode == engine.Scatter && !distinct
	case engine.Next:
		return false
	}
	return false
}

// canMergeOnFilter returns true if the join constraint makes the routes
// mergeable by unique vindex. The constraint has to be an equality
// like a.id = b.id where both columns have the same unique vindex.
func (rb *route) canMergeOnFilter(pb *primitiveBuilder, rrb *route, filter sqlparser.Expr) bool {
	comparison, ok := filter.(*sqlparser.ComparisonExpr)
	if !ok {
		return false
	}
	if comparison.Operator != sqlparser.EqualOp {
		return false
	}
	left := comparison.Left
	right := comparison.Right
	lVindex := pb.st.Vindex(left, rb)
	if lVindex == nil {
		left, right = right, left
		lVindex = pb.st.Vindex(left, rb)
	}
	if lVindex == nil || !lVindex.IsUnique() {
		return false
	}
	rVindex := pb.st.Vindex(right, rrb)
	if rVindex == nil {
		return false
	}
	return rVindex == lVindex
}

// UpdatePlan evaluates the primitive against the specified
// filter. If it's an improvement, the primitive is updated.
// We assume that the filter has already been pushed into
// the route.
func (rb *route) UpdatePlan(pb *primitiveBuilder, filter sqlparser.Expr) {
	switch rb.eroute.Opcode {
	// For these opcodes, a new filter will not make any difference, so we can just exit early
	case engine.Unsharded, engine.Next, engine.DBA, engine.Reference, engine.None:
		return
	}
	opcode, vindex, values := rb.computePlan(pb, filter)
	if opcode == engine.Scatter {
		return
	}
	// If we get SelectNone in next filters, override the previous route plan.
	if opcode == engine.None {
		rb.updateRoute(opcode, vindex, values)
		return
	}
	switch rb.eroute.Opcode {
	case engine.EqualUnique:
		if opcode == engine.EqualUnique && vindex.Cost() < rb.eroute.Vindex.Cost() {
			rb.updateRoute(opcode, vindex, values)
		}
	case engine.Equal:
		switch opcode {
		case engine.EqualUnique:
			rb.updateRoute(opcode, vindex, values)
		case engine.Equal:
			if vindex.Cost() < rb.eroute.Vindex.Cost() {
				rb.updateRoute(opcode, vindex, values)
			}
		}
	case engine.IN:
		switch opcode {
		case engine.EqualUnique, engine.Equal:
			rb.updateRoute(opcode, vindex, values)
		case engine.IN:
			if vindex.Cost() < rb.eroute.Vindex.Cost() {
				rb.updateRoute(opcode, vindex, values)
			}
		}
	case engine.MultiEqual:
		switch opcode {
		case engine.EqualUnique, engine.Equal, engine.IN:
			rb.updateRoute(opcode, vindex, values)
		case engine.MultiEqual:
			if vindex.Cost() < rb.eroute.Vindex.Cost() {
				rb.updateRoute(opcode, vindex, values)
			}
		}
	case engine.Scatter:
		switch opcode {
		case engine.EqualUnique, engine.Equal, engine.IN, engine.MultiEqual, engine.None:
			rb.updateRoute(opcode, vindex, values)
		}
	}
}

func (rb *route) updateRoute(opcode engine.Opcode, vindex vindexes.SingleColumn, condition sqlparser.Expr) {
	rb.eroute.Opcode = opcode
	rb.eroute.Vindex = vindex
	rb.condition = condition
}

// computePlan computes the plan for the specified filter.
func (rb *route) computePlan(pb *primitiveBuilder, filter sqlparser.Expr) (opcode engine.Opcode, vindex vindexes.SingleColumn, condition sqlparser.Expr) {
	switch node := filter.(type) {
	case *sqlparser.ComparisonExpr:
		switch node.Operator {
		case sqlparser.EqualOp:
			return rb.computeEqualPlan(pb, node)
		case sqlparser.InOp:
			return rb.computeINPlan(pb, node)
		case sqlparser.NotInOp:
			return rb.computeNotInPlan(node.Right), nil, nil
		case sqlparser.LikeOp:
			return rb.computeLikePlan(pb, node)
		}
	case *sqlparser.IsExpr:
		return rb.computeISPlan(pb, node)
	}
	return engine.Scatter, nil, nil
}

// computeLikePlan computes the plan for 'LIKE' constraint
func (rb *route) computeLikePlan(pb *primitiveBuilder, comparison *sqlparser.ComparisonExpr) (opcode engine.Opcode, vindex vindexes.SingleColumn, condition sqlparser.Expr) {

	left := comparison.Left
	right := comparison.Right

	if sqlparser.IsNull(right) {
		return engine.None, nil, nil
	}
	if !rb.exprIsValue(right) {
		return engine.Scatter, nil, nil
	}
	vindex = pb.st.Vindex(left, rb)
	if vindex == nil {
		// if there is no vindex defined, scatter
		return engine.Scatter, nil, nil
	}
	if subsharding, ok := vindex.(vindexes.Prefixable); ok {
		return engine.Equal, subsharding.PrefixVindex(), right
	}

	return engine.Scatter, nil, nil
}

// computeEqualPlan computes the plan for an equality constraint.
func (rb *route) computeEqualPlan(pb *primitiveBuilder, comparison *sqlparser.ComparisonExpr) (opcode engine.Opcode, vindex vindexes.SingleColumn, condition sqlparser.Expr) {
	left := comparison.Left
	right := comparison.Right

	if sqlparser.IsNull(right) {
		return engine.None, nil, nil
	}

	vindex = pb.st.Vindex(left, rb)
	if vindex == nil {
		left, right = right, left
		vindex = pb.st.Vindex(left, rb)
		if vindex == nil {
			return engine.Scatter, nil, nil
		}
	}
	if !rb.exprIsValue(right) {
		return engine.Scatter, nil, nil
	}
	if vindex.IsUnique() {
		return engine.EqualUnique, vindex, right
	}
	return engine.Equal, vindex, right
}

// computeIS computes the plan for an equality constraint.
func (rb *route) computeISPlan(pb *primitiveBuilder, comparison *sqlparser.IsExpr) (opcode engine.Opcode, vindex vindexes.SingleColumn, expr sqlparser.Expr) {
	// we only handle IS NULL correct. IsExpr can contain other expressions as well
	if comparison.Right != sqlparser.IsNullOp {
		return engine.Scatter, nil, nil
	}

	vindex = pb.st.Vindex(comparison.Left, rb)
	// fallback to scatter gather if there is no vindex
	if vindex == nil {
		return engine.Scatter, nil, nil
	}
	if _, isLookup := vindex.(vindexes.Lookup); isLookup {
		// the lookup table is keyed by the lookup value, so it does not support nulls
		return engine.Scatter, nil, nil
	}
	if vindex.IsUnique() {
		return engine.EqualUnique, vindex, &sqlparser.NullVal{}
	}
	return engine.Equal, vindex, &sqlparser.NullVal{}
}

// computeINPlan computes the plan for an IN constraint.
func (rb *route) computeINPlan(pb *primitiveBuilder, comparison *sqlparser.ComparisonExpr) (opcode engine.Opcode, vindex vindexes.SingleColumn, expr sqlparser.Expr) {
	switch comparison.Left.(type) {
	case *sqlparser.ColName:
		return rb.computeSimpleINPlan(pb, comparison)
	case sqlparser.ValTuple:
		return rb.computeCompositeINPlan(pb, comparison)
	}
	return engine.Scatter, nil, nil
}

// computeSimpleINPlan computes the plan for a simple IN constraint.
func (rb *route) computeSimpleINPlan(pb *primitiveBuilder, comparison *sqlparser.ComparisonExpr) (opcode engine.Opcode, vindex vindexes.SingleColumn, expr sqlparser.Expr) {
	vindex = pb.st.Vindex(comparison.Left, rb)
	if vindex == nil {
		return engine.Scatter, nil, nil
	}
	switch node := comparison.Right.(type) {
	case sqlparser.ValTuple:
		if len(node) == 1 && sqlparser.IsNull(node[0]) {
			return engine.None, nil, nil
		}

		for _, n := range node {
			if !rb.exprIsValue(n) {
				return engine.Scatter, nil, nil
			}
		}
		return engine.IN, vindex, comparison
	case sqlparser.ListArg:
		return engine.IN, vindex, comparison
	}
	return engine.Scatter, nil, nil
}

// computeCompositeINPlan computes the plan for a composite IN constraint.
func (rb *route) computeCompositeINPlan(pb *primitiveBuilder, comparison *sqlparser.ComparisonExpr) (opcode engine.Opcode, vindex vindexes.SingleColumn, values sqlparser.Expr) {
	leftTuple := comparison.Left.(sqlparser.ValTuple)
	return rb.iterateCompositeIN(pb, comparison, nil, leftTuple)
}

// iterateCompositeIN recursively walks the LHS tuple of the IN clause looking
// for column names. For those that match a vindex, it builds a multi-value plan
// using the corresponding values in the RHS. It returns the best of the plans built.
func (rb *route) iterateCompositeIN(
	pb *primitiveBuilder,
	comparison *sqlparser.ComparisonExpr,
	coordinates []int,
	tuple sqlparser.ValTuple,
) (opcode engine.Opcode, vindex vindexes.SingleColumn, values sqlparser.Expr) {
	opcode = engine.Scatter

	cindex := len(coordinates)
	coordinates = append(coordinates, 0)
	for idx, expr := range tuple {
		coordinates[cindex] = idx
		switch expr := expr.(type) {
		case sqlparser.ValTuple:
			newOpcode, newVindex, newValues := rb.iterateCompositeIN(pb, comparison, coordinates, expr)
			opcode, vindex, values = bestOfComposite(opcode, newOpcode, vindex, newVindex, values, newValues)
		case *sqlparser.ColName:
			newVindex := pb.st.Vindex(expr, rb)
			if newVindex != nil {
				newOpcode, newValues := rb.compositePlanForCol(pb, comparison, coordinates)
				opcode, vindex, values = bestOfComposite(opcode, newOpcode, vindex, newVindex, values, newValues)
			}
		}
	}
	return opcode, vindex, values
}

// compositePlanForCol builds a plan for a matched column in the LHS
// of a composite IN clause.
func (rb *route) compositePlanForCol(pb *primitiveBuilder, comparison *sqlparser.ComparisonExpr, coordinates []int) (opcode engine.Opcode, values sqlparser.Expr) {
	rightTuple, ok := comparison.Right.(sqlparser.ValTuple)
	if !ok {
		return engine.Scatter, nil
	}
	retVal := make(sqlparser.ValTuple, len(rightTuple))
	for i, rval := range rightTuple {
		val := tupleAccess(rval, coordinates)
		if val == nil {
			return engine.Scatter, nil
		}
		if !rb.exprIsValue(val) {
			return engine.Scatter, nil
		}
		retVal[i] = val
	}
	return engine.MultiEqual, retVal
}

// tupleAccess returns the value of the expression that corresponds
// to the specified coordinates.
func tupleAccess(expr sqlparser.Expr, coordinates []int) sqlparser.Expr {
	tuple, _ := expr.(sqlparser.ValTuple)
	for _, idx := range coordinates {
		if idx >= len(tuple) {
			return nil
		}
		expr = tuple[idx]
		tuple, _ = expr.(sqlparser.ValTuple)
	}
	return expr
}

// bestOfComposite returns the best of two composite IN clause plans.
func bestOfComposite(opcode1, opcode2 engine.Opcode, vindex1, vindex2 vindexes.SingleColumn, values1, values2 sqlparser.Expr) (opcode engine.Opcode, vindex vindexes.SingleColumn, values sqlparser.Expr) {
	if opcode1 == engine.Scatter {
		return opcode2, vindex2, values2
	}
	if opcode2 == engine.Scatter {
		return opcode1, vindex1, values1
	}
	if vindex1.Cost() < vindex2.Cost() {
		return opcode1, vindex1, values1
	}
	return opcode2, vindex2, values2
}

// computeNotInPlan looks for null values to produce a SelectNone if found
func (rb *route) computeNotInPlan(right sqlparser.Expr) engine.Opcode {
	switch node := right.(type) {
	case sqlparser.ValTuple:
		for _, n := range node {
			if sqlparser.IsNull(n) {
				return engine.None
			}
		}
	}

	return engine.Scatter
}

// exprIsValue returns true if the expression can be treated as a value
// for the routeOption. External references are treated as value.
func (rb *route) exprIsValue(expr sqlparser.Expr) bool {
	if node, ok := expr.(*sqlparser.ColName); ok {
		return node.Metadata.(*column).Origin() != rb
	}
	return sqlparser.IsValue(expr)
}

// queryTimeout returns DirectiveQueryTimeout value if set, otherwise returns 0.
func queryTimeout(d *sqlparser.CommentDirectives) int {
	val, _ := d.GetString(sqlparser.DirectiveQueryTimeout, "0")
	if intVal, err := strconv.Atoi(val); err == nil {
		return intVal
	}
	return 0
}
