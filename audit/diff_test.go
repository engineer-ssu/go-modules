package audit

import (
	"encoding/json"
	"testing"
)

// series 는 KeyRelations 를 거친 형태(PK 키 객체)로 만든다.
func seriesRow(id, priority int, name string) map[string]any {
	return map[string]any{"id": id, "priority": priority, "i18n": map[string]any{
		jsonKey(id*10 + 1): map[string]any{"id": id*10 + 1, "locale": "ko", "name": name},
	}}
}

func jsonKey(id int) string { b, _ := json.Marshal(id); return string(b) }

func seriesOf(rows ...map[string]any) map[string]any {
	keyed := map[string]any{}
	for _, r := range rows {
		keyed[jsonKey(r["id"].(int))] = r
	}
	return map[string]any{"series": keyed}
}

func TestDiffSnapshots(t *testing.T) {
	tests := []struct {
		name     string
		old, new map[string]any
		want     string
	}{
		{
			// 프론트가 삭제 후 남은 행의 priority 를 다시 매겨 보내는 경우. 배열이면 뒤 행이 당겨져 모든 필드가 replace 됐다.
			name: "가운데 행 삭제 + priority 재번호",
			old:  seriesOf(seriesRow(25730, 1, "시리즈1"), seriesRow(25734, 2, "시리즈111"), seriesRow(25736, 3, "시리즈3")),
			new:  seriesOf(seriesRow(25730, 0, "시리즈1"), seriesRow(25736, 1, "시리즈3")),
			want: `[{"value":0,"op":"replace","path":"/series/25730/priority"},` +
				`{"op":"remove","path":"/series/25734"},` +
				`{"value":1,"op":"replace","path":"/series/25736/priority"}]`,
		},
		{
			name: "순서 변경(priority 맞바꿈)",
			old:  seriesOf(seriesRow(25734, 1, "시리즈111"), seriesRow(25736, 2, "시리즈3")),
			new:  seriesOf(seriesRow(25734, 2, "시리즈111"), seriesRow(25736, 1, "시리즈3")),
			want: `[{"value":2,"op":"replace","path":"/series/25734/priority"},` +
				`{"value":1,"op":"replace","path":"/series/25736/priority"}]`,
		},
		{
			name: "행 추가",
			old:  seriesOf(seriesRow(25734, 0, "시리즈111")),
			new:  seriesOf(seriesRow(25734, 0, "시리즈111"), seriesRow(25740, 1, "새 시리즈")),
			want: `[{"value":{"i18n":{"257401":{"id":257401,"locale":"ko","name":"새 시리즈"}},"id":25740,"priority":1},"op":"add","path":"/series/25740"}]`,
		},
		{
			name: "하위 relation 값 변경",
			old:  seriesOf(seriesRow(25734, 0, "시리즈111")),
			new:  seriesOf(seriesRow(25734, 0, "시리즈222")),
			want: `[{"value":"시리즈222","op":"replace","path":"/series/25734/i18n/257341/name"}]`,
		},
		{
			name: "컬럼 값 변경은 replace",
			old:  map[string]any{"tel": "02-111", "fax": nil},
			new:  map[string]any{"tel": "02-222", "fax": nil},
			want: `[{"value":"02-222","op":"replace","path":"/tel"}]`,
		},
		{
			// 배열 컬럼(text[])은 배열로 남으므로 LCS 로 비교된다
			name: "배열 컬럼 가운데 값 삭제는 remove 하나",
			old:  map[string]any{"viewLanguages": []any{"en", "ko", "ja"}},
			new:  map[string]any{"viewLanguages": []any{"en", "ja"}},
			want: `[{"op":"remove","path":"/viewLanguages/1"}]`,
		},
		{
			name: "변경 없음은 빈 patch",
			old:  seriesOf(seriesRow(25734, 0, "시리즈111")),
			new:  seriesOf(seriesRow(25734, 0, "시리즈111")),
			want: `null`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			patch, err := diffSnapshots(tt.old, tt.new)
			if err != nil {
				t.Fatalf("diffSnapshots: %v", err)
			}
			got, _ := json.Marshal(patch)
			if string(got) != tt.want {
				t.Errorf("patch =\n %s\nwant\n %s", got, tt.want)
			}
		})
	}
}
