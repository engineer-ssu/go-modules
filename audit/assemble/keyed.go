package assemble

import (
	"fmt"
	"strconv"
)

// KeyRelations 는 n 의 relation 배열([]Node)을 각 relation 의 PK 값을 키로 하는 객체로 바꾼 사본을 반환한다.
// 하위 relation 도 재귀적으로 바꾸고, 컬럼 값(배열 컬럼 포함)은 그대로 둔다. n 은 수정하지 않는다.
//
//	"series": [{"id": 1, ...}, {"id": 2, ...}]  ->  "series": {"1": {"id": 1, ...}, "2": {"id": 2, ...}}
//
// 배열은 위치로만 비교되므로 가운데 행을 지우면 뒤 행들이 한 칸씩 당겨져 모든 필드가 바뀐 것처럼 diff 된다.
// PK 를 키로 쓰면 JSON Patch 경로가 /series/2/priority 처럼 행 id 로 고정된다.
// relation 의 PK 컬럼이 fields 에 없으면 키를 만들 수 없으므로 에러를 반환한다.
func (s *Spec) KeyRelations(n Node) (Node, error) {
	out := make(Node, len(n))
	for k, v := range n {
		out[k] = v
	}
	for _, rel := range s.Relations {
		children, _ := n[rel.Name].([]Node)
		keyed, err := rel.Spec.keyChildren(children, rel.Name)
		if err != nil {
			return nil, err
		}
		out[rel.Name] = keyed
	}
	return out, nil
}

func (s *Spec) keyChildren(children []Node, path string) (map[string]any, error) {
	keyed := make(map[string]any, len(children))
	if len(children) == 0 {
		return keyed, nil
	}
	if s.pkField == "" {
		return nil, fmt.Errorf("%s: PK 컬럼 %q 가 fields 에 없어 키를 만들 수 없습니다", path, s.PrimaryKey)
	}
	for _, child := range children {
		key, err := keyString(child[s.pkField])
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", path, s.pkField, err)
		}
		if _, dup := keyed[key]; dup {
			return nil, fmt.Errorf("%s: PK %q 가 중복되었습니다", path, key)
		}
		node, err := s.KeyRelations(child)
		if err != nil {
			return nil, err
		}
		keyed[key] = node
	}
	return keyed, nil
}

func keyString(v any) (string, error) {
	switch k := v.(type) {
	case int64:
		return strconv.FormatInt(k, 10), nil
	case string:
		return k, nil
	case nil:
		return "", fmt.Errorf("PK 값이 null 입니다")
	}
	return "", fmt.Errorf("지원하지 않는 PK 타입 %T", v)
}
