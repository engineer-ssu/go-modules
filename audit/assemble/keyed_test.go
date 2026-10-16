package assemble

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestKeyRelations(t *testing.T) {
	spec, err := Compile(testAssembleMap)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	node := Node{
		"id": int64(1), "slug": "a", "isBrand": false,
		"i18n": []Node{
			{"id": "uuid-b", "locale": "ko", "desc": "에이"},
			{"id": "uuid-a", "locale": "en", "desc": "A"},
		},
		"categories": []Node{
			{"id": int64(10), "i18n": []Node{{"id": int64(100), "name": "보안"}}},
		},
	}

	keyed, err := spec.KeyRelations(node)
	if err != nil {
		t.Fatalf("KeyRelations: %v", err)
	}
	got, _ := json.Marshal(keyed)
	want := `{"categories":{"10":{"i18n":{"100":{"id":100,"name":"보안"}},"id":10}},` +
		`"i18n":{"uuid-a":{"desc":"A","id":"uuid-a","locale":"en"},"uuid-b":{"desc":"에이","id":"uuid-b","locale":"ko"}},` +
		`"id":1,"isBrand":false,"slug":"a"}`
	if string(got) != want {
		t.Errorf("KeyRelations =\n %s\nwant\n %s", got, want)
	}

	// 원본은 그대로
	if _, ok := node["i18n"].([]Node); !ok {
		t.Errorf("원본 node 가 수정됨: %T", node["i18n"])
	}

	// relation 이 비어 있거나 없으면 빈 객체
	empty, err := spec.KeyRelations(Node{"id": int64(2), "i18n": []Node{}})
	if err != nil {
		t.Fatalf("KeyRelations(empty): %v", err)
	}
	if b, _ := json.Marshal(empty); string(b) != `{"categories":{},"i18n":{},"id":2}` {
		t.Errorf("빈 relation = %s, want {} 객체", b)
	}
}

func TestKeyRelationsErrors(t *testing.T) {
	noPK := map[string]any{
		"table": "t",
		"fields": Fields{
			{"id", "id"},
			{"rows", map[string]any{"table": "r", "type": "one-to-many", "foreignKey": "t_id", "fields": Fields{{"name", "name"}}}},
		},
	}
	spec, err := Compile(noPK)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if _, err := spec.KeyRelations(Node{"rows": []Node{{"name": "x"}}}); err == nil || !strings.Contains(err.Error(), "PK") {
		t.Errorf("PK 필드 없는 relation: err = %v, want PK 에러", err)
	}

	spec, _ = Compile(testAssembleMap)
	if _, err := spec.KeyRelations(Node{"i18n": []Node{{"id": nil}}}); err == nil {
		t.Error("PK 값이 null 인데 에러가 없음")
	}
}
