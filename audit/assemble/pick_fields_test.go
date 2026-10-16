package assemble

import (
	"slices"
	"testing"
)

// testAssembleMap 은 컬럼, one-to-many, many-to-many(중첩 relation 포함)를 모두 가진 테스트용 맵이다.
var testAssembleMap = map[string]any{
	"table":      "machines_company",
	"primaryKey": "id",
	"sort":       []string{"id"},
	"fields": Fields{
		{"id", "id"},
		{"slug", "slug"},
		{"isBrand", "is_brand"},
		{"i18n", map[string]any{
			"table":      "company_i18n",
			"type":       "one-to-many",
			"foreignKey": "company_id",
			"sort":       []string{"priority", "locale"},
			"fields":     Fields{{"id", "id"}, {"locale", "locale"}, {"desc", "description"}},
		}},
		{"categories", map[string]any{
			"table":        "category",
			"type":         "many-to-many",
			"secondary":    "company_category",
			"secondaryKey": "category_id",
			"foreignKey":   "company_id",
			"fields": Fields{
				{"id", "id"},
				{"i18n", map[string]any{
					"table":      "category_i18n",
					"type":       "one-to-many",
					"foreignKey": "category_id",
					"fields":     Fields{{"id", "id"}, {"name", "name"}},
				}},
			},
		}},
	},
}

func fieldNames(m map[string]any) []string {
	var names []string
	for _, f := range m["fields"].(Fields) {
		names = append(names, f[0].(string))
	}
	return names
}

func TestPickFields(t *testing.T) {
	all := fieldNames(testAssembleMap)
	got := PickFields(testAssembleMap, map[string]bool{"id": true, "slug": true, "categories": true, "logos": true})

	if names := fieldNames(got); !slices.Equal(names, []string{"id", "slug", "categories"}) {
		t.Errorf("fields = %v, want [id slug categories] (맵 순서 유지, 맵에 없는 logos 무시)", names)
	}
	if got["table"] != testAssembleMap["table"] {
		t.Errorf("table = %v, want %v", got["table"], testAssembleMap["table"])
	}
	if !slices.Equal(fieldNames(testAssembleMap), all) {
		t.Errorf("원본 맵이 수정됨: %v", fieldNames(testAssembleMap))
	}
	if _, err := Compile(got); err != nil {
		t.Errorf("Compile(filtered): %v", err)
	}
}

func TestPrimaryKeyField(t *testing.T) {
	if got, ok := PrimaryKeyField(testAssembleMap); !ok || got != "id" {
		t.Errorf("PrimaryKeyField(testAssembleMap) = %q, %v, want \"id\", true", got, ok)
	}

	custom := map[string]any{
		"table":      "t",
		"primaryKey": "uuid",
		"fields":     Fields{{"key", "uuid"}, {"name", "name"}},
	}
	if got, ok := PrimaryKeyField(custom); !ok || got != "key" {
		t.Errorf("PrimaryKeyField(custom) = %q, %v, want \"key\", true", got, ok)
	}

	noPK := map[string]any{"table": "t", "fields": Fields{{"name", "name"}}}
	if got, ok := PrimaryKeyField(noPK); ok {
		t.Errorf("PrimaryKeyField(noPK) = %q, true, want false", got)
	}
}
