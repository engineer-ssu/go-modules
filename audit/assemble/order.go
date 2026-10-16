package assemble

import (
	"bytes"
	"encoding/json"
)

// Result 는 조립 결과를 맵에 기입한 필드 순서대로 JSON 직렬화한다.
//
// Node(map) 를 그대로 json.Marshal 하면 키가 알파벳순으로 정렬되므로,
// 순서가 필요한 곳(파일 저장, HTTP 응답 등)에서는 Result 로 감싸서 직렬화한다.
//
//	json.NewEncoder(w).Encode(assemble.Result{Spec: spec, Nodes: nodes})
//
// 주의: json.Marshal 은 MarshalJSON 결과에도 HTML 이스케이프(& -> \u0026)를 적용한다.
// 이를 피하려면 SetEscapeHTML(false) 를 설정한 json.Encoder 를 사용한다.
type Result struct {
	Spec  *Spec
	Nodes []Node
}

func (r Result) MarshalJSON() ([]byte, error) {
	w := &orderedWriter{}
	w.enc = json.NewEncoder(&w.buf)
	w.enc.SetEscapeHTML(false)
	if err := w.nodes(r.Spec, r.Nodes); err != nil {
		return nil, err
	}
	return w.buf.Bytes(), nil
}

type orderedWriter struct {
	buf bytes.Buffer
	enc *json.Encoder
}

func (w *orderedWriter) nodes(s *Spec, nodes []Node) error {
	w.buf.WriteByte('[')
	for i, n := range nodes {
		if i > 0 {
			w.buf.WriteByte(',')
		}
		if err := w.node(s, n); err != nil {
			return err
		}
	}
	w.buf.WriteByte(']')
	return nil
}

func (w *orderedWriter) node(s *Spec, n Node) error {
	w.buf.WriteByte('{')
	for i, f := range s.order {
		if i > 0 {
			w.buf.WriteByte(',')
		}
		if err := w.value(f.name); err != nil {
			return err
		}
		w.buf.WriteByte(':')

		if f.rel != nil {
			children, _ := n[f.name].([]Node) // 없으면 nil -> []
			if err := w.nodes(f.rel, children); err != nil {
				return err
			}
			continue
		}
		if err := w.value(n[f.name]); err != nil {
			return err
		}
	}
	w.buf.WriteByte('}')
	return nil
}

// value 는 단일 값을 HTML 이스케이프 없이 쓴다 (Encoder 가 붙이는 개행은 제거).
func (w *orderedWriter) value(v any) error {
	if err := w.enc.Encode(v); err != nil {
		return err
	}
	w.buf.Truncate(w.buf.Len() - 1)
	return nil
}
