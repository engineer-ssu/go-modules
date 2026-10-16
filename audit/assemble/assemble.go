// Package assemble 는 선언적 맵(AssembleMap)을 따라 관계형 데이터를 트리 형태로 조립한다.
//
// 재귀의 단위는 row가 아니라 "레벨(테이블)"이다. 각 레벨은 상위 레벨의 키 목록 전체를
// `= ANY($1)` 로 한 번에 조회하므로, 루트 행이 몇 개든 쿼리 수는 맵의 노드 수로 고정된다 (N+1 없음).
package assemble

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
)

// Node 는 조립 결과 한 건. 키는 맵에 정의한 JSON 키(alias)다.
type Node = map[string]any

// key 는 int64(정수 PK/FK) 또는 string(uuid) 중 하나.
type key = any

const (
	colPK     = "__pk"
	colParent = "__parent"
)

const (
	typeRoot       = ""
	typeOneToMany  = "one-to-many"
	typeManyToMany = "many-to-many"
)

// ---------------------------------------------------------------------------
// Spec: map[string]any 설정을 한 번 검증·컴파일한 결과
// ---------------------------------------------------------------------------

type Column struct {
	Name   string // JSON 키 (예: "desc")
	Column string // DB 컬럼명 (예: "description")
}

type Relation struct {
	Name string
	Spec *Spec
}

type Spec struct {
	Table        string
	PrimaryKey   string
	Type         string
	Secondary    string
	SecondaryKey string
	ForeignKey   string
	Sort         []string // DB 컬럼명 기준
	Columns      []Column
	Relations    []Relation

	order   []fieldRef // 맵에 기입한 필드 순서 (JSON 출력 순서)
	pkField string     // PrimaryKey 컬럼에 매핑된 JSON 키 (fields 에 없으면 "")
	query   string
}

type fieldRef struct {
	name string
	rel  *Spec // nil 이면 컬럼
}

// Fields 는 순서를 보존하는 필드 목록. 각 원소는 {JSON 키, 컬럼명(string) 또는 relation(map)}.
//
//	"fields": assemble.Fields{
//		{"id", "id"},
//		{"desc", "description"},
//		{"i18n", map[string]any{...}},
//	}
//
// Go 의 map 리터럴은 기입 순서를 보존하지 않으므로, 순서가 중요하면 map 대신 Fields 를 쓴다.
type Fields [][2]any

type pair struct {
	name  string
	value any
}

// fieldPairs 는 fields 설정을 순서 있는 목록으로 바꾼다.
// Fields 면 기입 순서, map[string]any 면 (순서 정보가 없으므로) 이름순.
func fieldPairs(v any, path string) ([]pair, error) {
	switch f := v.(type) {
	case Fields:
		out := make([]pair, len(f))
		for i, kv := range f {
			name, ok := kv[0].(string)
			if !ok {
				return nil, fmt.Errorf("%s: fields[%d] 의 첫 원소(JSON 키)는 string 이어야 합니다 (현재 %T)", path, i, kv[0])
			}
			out[i] = pair{name: name, value: kv[1]}
		}
		return out, nil
	case map[string]any:
		names := make([]string, 0, len(f))
		for name := range f {
			names = append(names, name)
		}
		sort.Strings(names)
		out := make([]pair, len(names))
		for i, name := range names {
			out[i] = pair{name: name, value: f[name]}
		}
		return out, nil
	}
	return nil, fmt.Errorf("%s: fields 는 assemble.Fields 또는 map[string]any 이어야 합니다 (현재 %T)", path, v)
}

// PickFields 는 설정 맵 m 의 최상위 fields 중 names 에 있는 JSON 키만 남긴 사본을 반환한다.
// 하위 relation 설정은 통째로 유지하고, fields 외의 설정(table, sort 등)은 그대로 복사한다.
// Fields 는 기입 순서를 유지한다. m 은 수정하지 않는다.
func PickFields(m map[string]any, names map[string]bool) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}

	switch fields := m["fields"].(type) {
	case Fields:
		filtered := Fields{}
		for _, f := range fields {
			if name, ok := f[0].(string); ok {
				if names[name] {
					filtered = append(filtered, f)
				}
			}
		}
		out["fields"] = filtered
	case map[string]any:
		filtered := map[string]any{}
		for name, v := range fields {
			if names[name] {
				filtered[name] = v
			}
		}
		out["fields"] = filtered
	}
	return out
}

// PrimaryKeyField 는 설정 맵 m 의 최상위 fields 중 primaryKey 컬럼(기본 "id")에 매핑된 JSON 키를 반환한다.
func PrimaryKeyField(m map[string]any) (string, bool) {
	pk, _ := m["primaryKey"].(string)
	if pk == "" {
		pk = "id"
	}
	pairs, err := fieldPairs(m["fields"], "root")
	if err != nil {
		return "", false
	}
	for _, p := range pairs {
		if column, ok := p.value.(string); ok && column == pk {
			return p.name, true
		}
	}
	return "", false
}

// Compile 은 설정 맵을 검증하고 쿼리를 미리 만들어 둔다.
// 잘못된 설정은 런타임 panic 대신 여기서 에러로 드러난다.
func Compile(m map[string]any) (*Spec, error) {
	return compile(m, "root", true)
}

func compile(m map[string]any, path string, isRoot bool) (*Spec, error) {
	s := &Spec{PrimaryKey: "id"}
	var err error

	str := func(k string, required bool) string {
		if err != nil {
			return ""
		}
		v, ok := m[k]
		if !ok {
			if required {
				err = fmt.Errorf("%s: %q 가 필요합니다", path, k)
			}
			return ""
		}
		sv, ok := v.(string)
		if !ok {
			err = fmt.Errorf("%s: %q 는 string 이어야 합니다 (현재 %T)", path, k, v)
		}
		return sv
	}

	s.Table = str("table", true)
	if pk := str("primaryKey", false); pk != "" {
		s.PrimaryKey = pk
	}
	s.Type = str("type", !isRoot)
	switch s.Type {
	case typeOneToMany:
		s.ForeignKey = str("foreignKey", true)
	case typeManyToMany:
		s.ForeignKey = str("foreignKey", true)
		s.Secondary = str("secondary", true)
		s.SecondaryKey = str("secondaryKey", true)
	case typeRoot:
		if !isRoot && err == nil {
			err = fmt.Errorf("%s: 하위 relation 에는 type 이 필요합니다", path)
		}
	default:
		if err == nil {
			err = fmt.Errorf("%s: 알 수 없는 type %q", path, s.Type)
		}
	}
	if err != nil {
		return nil, err
	}

	if v, ok := m["sort"]; ok {
		sv, ok := v.([]string)
		if !ok {
			return nil, fmt.Errorf("%s: sort 는 []string 이어야 합니다 (현재 %T)", path, v)
		}
		s.Sort = sv
	}

	pairs, err := fieldPairs(m["fields"], path)
	if err != nil {
		return nil, err
	}
	seenName := map[string]bool{}
	for _, p := range pairs {
		name := p.name
		if name == colPK || name == colParent {
			return nil, fmt.Errorf("%s: 필드명 %q 는 내부용으로 예약되어 있습니다", path, name)
		}
		if seenName[name] {
			return nil, fmt.Errorf("%s: 필드명 %q 가 중복되었습니다", path, name)
		}
		seenName[name] = true

		switch v := p.value.(type) {
		case string:
			s.Columns = append(s.Columns, Column{Name: name, Column: v})
			s.order = append(s.order, fieldRef{name: name})
		case map[string]any:
			sub, err := compile(v, path+"."+name, false)
			if err != nil {
				return nil, err
			}
			s.Relations = append(s.Relations, Relation{Name: name, Spec: sub})
			s.order = append(s.order, fieldRef{name: name, rel: sub})
		default:
			return nil, fmt.Errorf("%s.%s: string(컬럼명) 또는 map(relation) 이어야 합니다 (현재 %T)", path, name, v)
		}
	}

	// sort 에 JSON 키를 잘못 넣은 경우 잡기 (예: "desc" 대신 "description" 이어야 함)
	columns := map[string]bool{s.PrimaryKey: true}
	aliasToCol := map[string]string{}
	for _, c := range s.Columns {
		columns[c.Column] = true
		aliasToCol[c.Name] = c.Column
	}
	for _, c := range s.Sort {
		if !columns[c] {
			if col, ok := aliasToCol[c]; ok {
				return nil, fmt.Errorf("%s: sort 는 컬럼명 기준입니다. %q 대신 %q 를 사용하세요", path, c, col)
			}
		}
	}

	for _, c := range s.Columns {
		if c.Column == s.PrimaryKey {
			s.pkField = c.Name
			break
		}
	}

	s.query = s.buildQuery()
	return s, nil
}

func (s *Spec) buildQuery() string {
	cols := []string{fmt.Sprintf("t.%s AS %s", ident(s.PrimaryKey), colPK)}
	for _, c := range s.Columns {
		// alias 를 반드시 따옴표로: camelCase 보존 + "desc" 같은 예약어 대응
		cols = append(cols, fmt.Sprintf("t.%s AS %s", ident(c.Column), ident(c.Name)))
	}

	var from, parent string
	switch s.Type {
	case typeManyToMany:
		from = fmt.Sprintf("%s t JOIN %s s ON s.%s = t.%s",
			ident(s.Table), ident(s.Secondary), ident(s.SecondaryKey), ident(s.PrimaryKey))
		parent = "s." + ident(s.ForeignKey)
	case typeOneToMany:
		from = ident(s.Table) + " t"
		parent = "t." + ident(s.ForeignKey)
	default: // root
		from = ident(s.Table) + " t"
		parent = "t." + ident(s.PrimaryKey)
	}
	cols = append(cols, parent+" AS "+colParent)

	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s = ANY($1)", strings.Join(cols, ", "), from, parent)
	if len(s.Sort) > 0 {
		ss := make([]string, len(s.Sort))
		for i, c := range s.Sort {
			ss[i] = "t." + ident(c)
		}
		q += " ORDER BY " + strings.Join(ss, ", ")
	}
	return q
}

// Query 는 이 레벨에서 실행될 SQL (디버깅용).
func (s *Spec) Query() string { return s.query }

// ---------------------------------------------------------------------------
// Assembler
// ---------------------------------------------------------------------------

// QueryLog 는 쿼리 1회 실행 정보.
type QueryLog struct {
	Table   string
	Query   string
	Keys    int // ANY($1) 에 넘긴 키 개수
	Rows    int
	Elapsed time.Duration
}

// Queryer 는 *sqlx.DB 와 *sqlx.Tx 가 모두 만족한다.
type Queryer interface {
	QueryxContext(ctx context.Context, query string, args ...any) (*sqlx.Rows, error)
}

type Assembler struct {
	// DB 는 *sqlx.DB 또는 *sqlx.Tx. 트랜잭션 안의 변경을 읽으려면 *sqlx.Tx 를 넘긴다.
	DB Queryer
	// OnQuery 는 선택 사항. 형제 relation 이 병렬로 로드되므로 동시에 호출될 수 있다.
	OnQuery func(QueryLog)
}

type level struct {
	byParent map[key][]Node // 부모 키 -> 자식 노드 (sort 순서 유지)
	ordered  []Node         // 이 레벨의 고유 노드 (쿼리 순서)
}

// Assemble 은 루트 PK 목록으로 전체 트리를 구성한다. 결과 순서는 루트 spec 의 sort 를 따른다.
func (a *Assembler) Assemble(ctx context.Context, root *Spec, ids []int64) ([]Node, error) {
	keys := make([]key, len(ids))
	for i, id := range ids {
		keys[i] = id
	}
	lv, err := a.load(ctx, root, keys)
	if err != nil {
		return nil, err
	}
	if lv.ordered == nil {
		return []Node{}, nil
	}
	return lv.ordered, nil
}

func (a *Assembler) load(ctx context.Context, s *Spec, keys []key) (*level, error) {
	lv := &level{byParent: map[key][]Node{}}
	if len(keys) == 0 {
		return lv, nil
	}

	param, err := keysParam(keys)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.Table, err)
	}

	start := time.Now()
	rows, err := a.DB.QueryxContext(ctx, s.query, param)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.Table, err)
	}
	records, err := scanRows(rows)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.Table, err)
	}
	if a.OnQuery != nil {
		a.OnQuery(QueryLog{Table: s.Table, Query: s.query, Keys: len(keys), Rows: len(records), Elapsed: time.Since(start)})
	}

	nodes := map[key]Node{}
	seen := map[key]map[key]bool{} // 중간 테이블에 중복 행이 있어도 부모당 한 번만
	var pks []key
	for _, r := range records {
		pk, err := toKey(r[colPK])
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", s.Table, s.PrimaryKey, err)
		}
		parent, err := toKey(r[colParent])
		if err != nil {
			return nil, fmt.Errorf("%s parent key: %w", s.Table, err)
		}
		delete(r, colPK)
		delete(r, colParent)

		// many-to-many 에서는 같은 행이 여러 부모에 걸리므로 PK 기준으로 노드를 하나만 만든다.
		// (같은 map 을 공유하므로 하위 relation 을 한 번만 붙여도 모든 부모에 반영됨)
		node, ok := nodes[pk]
		if !ok {
			node = r
			nodes[pk] = node
			pks = append(pks, pk)
			lv.ordered = append(lv.ordered, node)
		}
		if seen[parent] == nil {
			seen[parent] = map[key]bool{}
		}
		if seen[parent][pk] {
			continue
		}
		seen[parent][pk] = true
		lv.byParent[parent] = append(lv.byParent[parent], node)
	}

	if len(s.Relations) == 0 || len(pks) == 0 {
		return lv, nil
	}

	results := make([]*level, len(s.Relations))
	errs := make([]error, len(s.Relations))
	if _, isTx := a.DB.(*sqlx.Tx); isTx {
		// 트랜잭션은 커넥션 하나를 쓰므로 동시에 여러 쿼리를 실행할 수 없다. 순차 로드.
		for i, rel := range s.Relations {
			if results[i], errs[i] = a.load(ctx, rel.Spec, pks); errs[i] != nil {
				break
			}
		}
	} else {
		// 형제 relation 은 서로 독립이므로 병렬 로드.
		// 고루틴 안에서는 노드를 건드리지 않고, 모두 끝난 뒤 한 고루틴에서 붙인다 (map 동시 쓰기 방지).
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		var wg sync.WaitGroup
		for i, rel := range s.Relations {
			wg.Add(1)
			go func(i int, rel Relation) {
				defer wg.Done()
				results[i], errs[i] = a.load(ctx, rel.Spec, pks)
				if errs[i] != nil {
					cancel()
				}
			}(i, rel)
		}
		wg.Wait()
	}

	for i, rel := range s.Relations {
		if errs[i] != nil {
			return nil, errs[i]
		}
		for pk, node := range nodes {
			if c, ok := results[i].byParent[pk]; ok {
				node[rel.Name] = c
			} else {
				node[rel.Name] = []Node{} // null 대신 빈 배열
			}
		}
	}
	return lv, nil
}

// ---------------------------------------------------------------------------
// 스캔 & 타입 정규화
// ---------------------------------------------------------------------------

func scanRows(rows *sqlx.Rows) ([]Node, error) {
	defer rows.Close()
	cts, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	var out []Node
	for rows.Next() {
		r := Node{}
		if err := rows.MapScan(r); err != nil {
			return nil, err
		}
		for _, ct := range cts {
			v, err := normalize(r[ct.Name()], ct.DatabaseTypeName())
			if err != nil {
				return nil, fmt.Errorf("column %s: %w", ct.Name(), err)
			}
			r[ct.Name()] = v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// normalize 는 드라이버(lib/pq, pgx stdlib)마다 다른 값을 JSON 친화적인 타입으로 통일한다.
//
//	int2/int4/int8   -> int64
//	uuid             -> "xxxxxxxx-xxxx-..." 문자열 (lib/pq 는 []byte 로 줌)
//	bpchar(n)        -> 뒤쪽 공백 패딩 제거
//	text[]/varchar[] -> []string (드라이버는 "{a,b}" 텍스트로 줌)
//	int2[]/int4[]/int8[] -> []int64
func normalize(v any, dbType string) (any, error) {
	if v == nil {
		return nil, nil
	}
	if dbType == "BYTEA" {
		return v, nil // 바이너리는 그대로 (JSON 에서는 base64)
	}
	if b, ok := v.([]byte); ok {
		v = string(b)
	}
	s, isStr := v.(string)
	if !isStr {
		return v, nil // int64, bool, float64, time.Time 등
	}
	switch dbType {
	case "BPCHAR":
		return strings.TrimRight(s, " "), nil
	case "_TEXT", "_VARCHAR", "_BPCHAR", "_UUID":
		var arr pq.StringArray
		if err := arr.Scan(s); err != nil {
			return nil, err
		}
		return []string(arr), nil
	case "_INT2", "_INT4", "_INT8":
		var arr pq.Int64Array
		if err := arr.Scan(s); err != nil {
			return nil, err
		}
		return []int64(arr), nil
	}
	return s, nil
}

func toKey(v any) (key, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case string:
		return n, nil
	}
	return nil, fmt.Errorf("unsupported key type %T", v)
}

// keysParam 은 ANY($1) 에 바인딩할 배열 파라미터를 만든다.
// pq.Array 는 lib/pq, pgx stdlib 양쪽에서 동작한다.
func keysParam(keys []key) (any, error) {
	switch keys[0].(type) {
	case int64:
		out := make([]int64, len(keys))
		for i, k := range keys {
			out[i] = k.(int64)
		}
		return pq.Array(out), nil
	case string: // uuid 컬럼이면 Postgres 가 $1 을 uuid[] 로 해석
		out := make([]string, len(keys))
		for i, k := range keys {
			out[i] = k.(string)
		}
		return pq.Array(out), nil
	}
	return nil, fmt.Errorf("unsupported key type %T", keys[0])
}

func ident(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
