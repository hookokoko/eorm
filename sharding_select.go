// Copyright 2021 ecodeclub
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package eorm

import (
	"context"
	"database/sql"
	"sync"

	operator "github.com/ecodeclub/eorm/internal/operator"

	"github.com/gotomicro/ekit/slice"

	"github.com/ecodeclub/eorm/internal/sharding"

	"github.com/ecodeclub/eorm/internal/errs"
	"github.com/valyala/bytebufferpool"
	"golang.org/x/sync/errgroup"
)

type ShardingSelector[T any] struct {
	selectorBuilder
	table *T
	db    Session
	lock  sync.Mutex
}

func NewShardingSelector[T any](db Session) *ShardingSelector[T] {
	return &ShardingSelector[T]{
		selectorBuilder: selectorBuilder{
			builder: builder{
				core:   db.getCore(),
				buffer: bytebufferpool.Get(),
			},
		},
		db: db,
	}
}

func (s *ShardingSelector[T]) Build(ctx context.Context) ([]sharding.Query, error) {
	var err error
	if s.meta == nil {
		s.meta, err = s.metaRegistry.Get(new(T))
		if err != nil {
			return nil, err
		}
	}
	shardingRes, err := s.findDsts(ctx)
	if err != nil {
		return nil, err
	}
	res := make([]sharding.Query, 0, len(shardingRes.Dsts))
	defer bytebufferpool.Put(s.buffer)
	for _, dst := range shardingRes.Dsts {
		q, err := s.buildQuery(dst.DB, dst.Table, dst.Name)
		if err != nil {
			return nil, err
		}
		res = append(res, q)
		s.args = nil
		s.buffer.Reset()
	}
	return res, nil
}

func (s *ShardingSelector[T]) buildQuery(db, tbl, ds string) (sharding.Query, error) {
	var err error
	s.writeString("SELECT ")
	if len(s.columns) == 0 {
		if err = s.buildAllColumns(); err != nil {
			return sharding.EmptyQuery, err
		}
	} else {
		err = s.buildSelectedList()
		if err != nil {
			return sharding.EmptyQuery, err
		}
	}
	s.writeString(" FROM ")
	s.quote(db)
	s.writeByte('.')
	s.quote(tbl)

	if len(s.where) > 0 {
		s.writeString(" WHERE ")
		p := s.where[0]
		for i := 1; i < len(s.where); i++ {
			p = p.And(s.where[i])
		}
		if err = s.buildExpr(p); err != nil {
			return sharding.EmptyQuery, err
		}
	}

	// group by
	if len(s.groupBy) > 0 {
		err = s.buildGroupBy()
		if err != nil {
			return sharding.EmptyQuery, err
		}
	}

	// order by
	if len(s.orderBy) > 0 {
		err = s.buildOrderBy()
		if err != nil {
			return sharding.EmptyQuery, err
		}
	}

	// having
	if len(s.having) > 0 {
		s.writeString(" HAVING ")
		p := s.having[0]
		for i := 1; i < len(s.having); i++ {
			p = p.And(s.having[i])
		}
		if err = s.buildExpr(p); err != nil {
			return sharding.EmptyQuery, err
		}
	}

	if s.offset > 0 {
		s.writeString(" OFFSET ")
		s.parameter(s.offset)
	}

	if s.limit > 0 {
		s.writeString(" LIMIT ")
		s.parameter(s.limit)
	}
	s.end()
	return sharding.Query{SQL: s.buffer.String(), Args: s.args, Datasource: ds, DB: db}, nil
}

func (s *ShardingSelector[T]) findDsts(ctx context.Context) (sharding.Result, error) {
	//  通过遍历 pre 查找目标 shardingkey
	if len(s.where) > 0 {
		pre := s.where[0]
		for i := 1; i < len(s.where)-1; i++ {
			pre = pre.And(s.where[i])
		}
		return s.findDstByPredicate(ctx, pre)
	}
	res := sharding.Result{
		Dsts: s.meta.ShardingAlgorithm.Broadcast(ctx),
	}
	return res, nil
}

func (s *ShardingSelector[T]) findDstByPredicate(ctx context.Context, pre Predicate) (sharding.Result, error) {
	switch pre.op {
	case opAnd:
		left, err := s.findDstByPredicate(ctx, pre.left.(Predicate))
		if err != nil {
			return sharding.EmptyResult, err
		}
		right, err := s.findDstByPredicate(ctx, pre.right.(Predicate))
		if err != nil {
			return sharding.EmptyResult, err
		}
		return s.mergeAnd(left, right), nil
	case opOr:
		left, err := s.findDstByPredicate(ctx, pre.left.(Predicate))
		if err != nil {
			return sharding.EmptyResult, err
		}
		right, err := s.findDstByPredicate(ctx, pre.right.(Predicate))
		if err != nil {
			return sharding.EmptyResult, err
		}
		return s.mergeOR(left, right), nil
	case opIn:
		col := pre.left.(Column)
		right := pre.right.(values)
		var results []sharding.Result
		for _, val := range right.data {
			res, err := s.meta.ShardingAlgorithm.Sharding(ctx,
				sharding.Request{Op: opEQ, SkValues: map[string]any{col.name: val}})
			if err != nil {
				return sharding.EmptyResult, err
			}
			results = append(results, res)
		}
		return s.mergeIN(results), nil
	case opNot:
		nPre, err := s.negatePredicate(pre.right.(Predicate))
		if err != nil {
			return sharding.EmptyResult, err
		}
		return s.findDstByPredicate(ctx, nPre)
	case opNotIN:
		return s.meta.ShardingAlgorithm.Sharding(ctx,
			sharding.Request{Op: opNotIN, SkValues: map[string]any{}})
	case opEQ, opGT, opLT, opGTEQ, opLTEQ, opNEQ:
		col, isCol := pre.left.(Column)
		right, isVals := pre.right.(valueExpr)
		if !isCol || !isVals {
			return sharding.EmptyResult, errs.ErrUnsupportedTooComplexQuery
		}
		return s.meta.ShardingAlgorithm.Sharding(ctx,
			sharding.Request{Op: pre.op, SkValues: map[string]any{col.name: right.val}})
	default:
		return sharding.EmptyResult, errs.NewUnsupportedOperatorError(pre.op.Text)
	}
}

func (s *ShardingSelector[T]) negatePredicate(pre Predicate) (Predicate, error) {
	switch pre.op {
	case opAnd:
		left, err := s.negatePredicate(pre.left.(Predicate))
		if err != nil {
			return emptyPredicate, err
		}
		right, err := s.negatePredicate(pre.right.(Predicate))
		if err != nil {
			return emptyPredicate, err
		}
		return Predicate{
			left: left, op: opOr, right: right,
		}, nil
	case opOr:
		left, err := s.negatePredicate(pre.left.(Predicate))
		if err != nil {
			return emptyPredicate, err
		}
		right, err := s.negatePredicate(pre.right.(Predicate))
		if err != nil {
			return emptyPredicate, err
		}
		return Predicate{
			left: left, op: opAnd, right: right,
		}, nil
	default:
		nOp, err := operator.NegateOp(pre.op)
		if err != nil {
			return emptyPredicate, err
		}
		return Predicate{left: pre.left, op: nOp, right: pre.right}, nil
	}
}

// mergeAnd 两个分片结果的交集
func (*ShardingSelector[T]) mergeAnd(left, right sharding.Result) sharding.Result {
	dsts := slice.IntersectSetFunc[sharding.Dst](left.Dsts, right.Dsts, func(src, dst sharding.Dst) bool {
		return src.Equals(dst)
	})
	return sharding.Result{Dsts: dsts}
}

// mergeOR 两个分片结果的并集
func (*ShardingSelector[T]) mergeOR(left, right sharding.Result) sharding.Result {
	dsts := slice.UnionSetFunc[sharding.Dst](left.Dsts, right.Dsts, func(src, dst sharding.Dst) bool {
		return src.Equals(dst)
	})
	return sharding.Result{Dsts: dsts}
}

// mergeIN 多个分片结果的并集
func (*ShardingSelector[T]) mergeIN(vals []sharding.Result) sharding.Result {
	var dsts []sharding.Dst
	for _, val := range vals {
		dsts = slice.UnionSetFunc[sharding.Dst](dsts, val.Dsts, func(src, dst sharding.Dst) bool {
			return src.Equals(dst)
		})
	}
	return sharding.Result{Dsts: dsts}
}

func (s *ShardingSelector[T]) buildAllColumns() error {
	for i, cMeta := range s.meta.Columns {
		_ = s.buildColumns(i, cMeta.FieldName)
	}
	return nil
}

func (s *ShardingSelector[T]) buildSelectedList() error {
	for i, selectable := range s.columns {
		if i > 0 {
			s.comma()
		}
		switch expr := selectable.(type) {
		case Column:
			err := s.builder.buildColumn(expr)
			if err != nil {
				return errs.NewInvalidFieldError(expr.name)
			}
		case columns:
			for j, c := range expr.cs {
				err := s.buildColumns(j, c)
				if err != nil {
					return err
				}
			}
		case Aggregate:
			if err := s.selectAggregate(expr); err != nil {
				return err
			}
		case RawExpr:
			s.buildRawExpr(expr)
		}
	}
	return nil

}
func (s *ShardingSelector[T]) selectAggregate(aggregate Aggregate) error {
	s.writeString(aggregate.fn)

	s.writeByte('(')
	if aggregate.distinct {
		s.writeString("DISTINCT ")
	}
	cMeta, ok := s.meta.FieldMap[aggregate.arg]
	if !ok {
		return errs.NewInvalidFieldError(aggregate.arg)
	}
	if aggregate.table != nil {
		if alias := aggregate.table.getAlias(); alias != "" {
			s.quote(alias)
			s.point()
		}
	}
	s.quote(cMeta.ColumnName)
	s.writeByte(')')
	if aggregate.alias != "" {
		s.writeString(" AS ")
		s.quote(aggregate.alias)
	}
	return nil
}

func (s *ShardingSelector[T]) buildColumns(index int, name string) error {
	if index > 0 {
		s.comma()
	}
	cMeta, ok := s.meta.FieldMap[name]
	if !ok {
		return errs.NewInvalidFieldError(name)
	}
	s.quote(cMeta.ColumnName)
	return nil
}

func (s *ShardingSelector[T]) buildExpr(expr Expr) error {
	switch exp := expr.(type) {
	case nil:
	case Column:
		exp.alias = ""
		_ = s.buildColumn(exp)
	case valueExpr:
		s.parameter(exp.val)
	case RawExpr:
		s.buildRawExpr(exp)
	case Predicate:
		if err := s.buildBinaryExpr(binaryExpr(exp)); err != nil {
			return err
		}
	default:
		return errs.NewErrUnsupportedExpressionType()
	}
	return nil
}

func (s *ShardingSelector[T]) buildOrderBy() error {
	s.writeString(" ORDER BY ")
	for i, ob := range s.orderBy {
		if i > 0 {
			s.comma()
		}
		for _, c := range ob.fields {
			cMeta, ok := s.meta.FieldMap[c]
			if !ok {
				return errs.NewInvalidFieldError(c)
			}
			s.quote(cMeta.ColumnName)
		}
		s.space()
		s.writeString(ob.order)
	}
	return nil
}

func (s *ShardingSelector[T]) buildGroupBy() error {
	s.writeString(" GROUP BY ")
	for i, gb := range s.groupBy {
		cMeta, ok := s.meta.FieldMap[gb]
		if !ok {
			return errs.NewInvalidFieldError(gb)
		}
		if i > 0 {
			s.comma()
		}
		s.quote(cMeta.ColumnName)
	}
	return nil
}

func (s *ShardingSelector[T]) Get(ctx context.Context) (*T, error) {
	qs, err := s.Limit(1).Build(ctx)
	if err != nil {
		return nil, err
	}
	if len(qs) == 0 {
		return nil, errs.ErrNotGenShardingQuery
	}
	// TODO 要确保前面的改写 SQL 只能生成一个 SQL
	if len(qs) > 1 {
		return nil, errs.ErrOnlyResultOneQuery
	}
	q := qs[0]
	// TODO 利用 ctx 传递 DB name
	row, err := s.db.queryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	if !row.Next() {
		return nil, ErrNoRows
	}
	tp := new(T)
	val := s.valCreator.NewPrimitiveValue(tp, s.meta)
	if err = val.SetColumns(row); err != nil {
		return nil, err
	}
	return tp, nil
}

func (s *ShardingSelector[T]) GetMulti(ctx context.Context) ([]*T, error) {
	qs, err := s.Build(ctx)
	if err != nil {
		return nil, err
	}
	var rowsSlice []*sql.Rows
	var eg errgroup.Group
	for _, query := range qs {
		q := query
		eg.Go(func() error {
			s.lock.Lock()
			defer s.lock.Unlock()
			// TODO 利用 ctx 传递 DB name
			rows, err := s.db.queryContext(ctx, q)
			if err == nil {
				rowsSlice = append(rowsSlice, rows)
			}
			return err
		})
	}
	err = eg.Wait()
	if err != nil {
		return nil, err
	}
	var res []*T
	for _, rows := range rowsSlice {
		for rows.Next() {
			tp := new(T)
			val := s.valCreator.NewPrimitiveValue(tp, s.meta)
			if err = val.SetColumns(rows); err != nil {
				return nil, err
			}
			res = append(res, tp)
		}
	}
	return res, nil
}

// Select 指定查询的列。
// 列可以是物理列，也可以是聚合函数，或者 RawExpr
func (s *ShardingSelector[T]) Select(columns ...Selectable) *ShardingSelector[T] {
	s.columns = columns
	return s
}

// From specifies the table which must be pointer of structure
func (s *ShardingSelector[T]) From(tbl *T) *ShardingSelector[T] {
	s.table = tbl
	return s
}

// Where accepts predicates
func (s *ShardingSelector[T]) Where(predicates ...Predicate) *ShardingSelector[T] {
	s.where = predicates
	return s
}

// Having accepts predicates
func (s *ShardingSelector[T]) Having(predicates ...Predicate) *ShardingSelector[T] {
	s.having = predicates
	return s
}

// GroupBy means "GROUP BY"
func (s *ShardingSelector[T]) GroupBy(columns ...string) *ShardingSelector[T] {
	s.groupBy = columns
	return s
}

// OrderBy means "ORDER BY"
func (s *ShardingSelector[T]) OrderBy(orderBys ...OrderBy) *ShardingSelector[T] {
	s.orderBy = orderBys
	return s
}

// Limit limits the size of result set
func (s *ShardingSelector[T]) Limit(limit int) *ShardingSelector[T] {
	s.limit = limit
	return s
}

// Offset was used by "LIMIT"
func (s *ShardingSelector[T]) Offset(offset int) *ShardingSelector[T] {
	s.offset = offset
	return s
}
