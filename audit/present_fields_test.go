package audit

import (
	"maps"
	"slices"
	"testing"
)

type presentFieldsTestInput struct {
	ID     int      `json:"id"`
	Tel    *string  `json:"tel,omitempty"`
	Logos  []string `json:"logos,omitempty"`
	NoTag  *string
	Hidden *string `json:"-"`
}

func TestPresentFields(t *testing.T) {
	tel := "02-000-0000"
	keys := func(m map[string]bool) []string { return slices.Sorted(maps.Keys(m)) }

	tests := []struct {
		name  string
		input any
		want  []string
	}{
		{"nil 필드 제외, int 는 항상 포함", &presentFieldsTestInput{ID: 1}, []string{"id"}},
		{"빈 슬라이스(전체 삭제)는 포함", &presentFieldsTestInput{Logos: []string{}}, []string{"id", "logos"}},
		{"구조체 값도 처리", presentFieldsTestInput{Tel: &tel}, []string{"id", "tel"}},
		{"태그 없음/- 는 제외", &presentFieldsTestInput{NoTag: &tel, Hidden: &tel}, []string{"id"}},
		{"map[string]bool 은 그대로", map[string]bool{"expandKikaive": true}, []string{"expandKikaive"}},
		{"nil 포인터는 빈 map", (*presentFieldsTestInput)(nil), []string{}},
		{"nil 은 빈 map", nil, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PresentFields(tt.input)
			if got == nil || !slices.Equal(keys(got), tt.want) {
				t.Errorf("PresentFields() keys = %v, want %v", keys(got), tt.want)
			}
		})
	}
}
