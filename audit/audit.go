package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
)

type RelationLoader func(context.Context, int) (any, error)

type Relation struct {
	JSONName string
	Load     RelationLoader
}

type SnapshotProvider struct {
	LoadBase  func(context.Context, int) (any, error)
	Relations map[string]Relation
}

func (p SnapshotProvider) Snapshot(ctx context.Context, id int, relationNames []string) (map[string]any, error) {
	base, err := p.LoadBase(ctx, id)
	if err != nil {
		return nil, err
	}

	snapshot, err := ToMap(base)
	if err != nil {
		return nil, err
	}

	for _, name := range relationNames {
		relation, ok := p.Relations[name]
		if !ok {
			return nil, fmt.Errorf("audit relation %q is not registered", name)
		}
		value, err := relation.Load(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("load audit relation %q: %w", name, err)
		}
		snapshot[relation.JSONName] = value
	}

	return snapshot, nil
}

func RelationNames(input any, relations map[string]Relation) []string {
	value := reflect.ValueOf(input)
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return nil
	}

	names := make([]string, 0, len(relations))
	for name := range relations {
		field := value.FieldByName(name)
		if !field.IsValid() || isNilValue(field) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func ToMap(value any) (map[string]any, error) {
	if value == nil {
		return map[string]any{}, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}

	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, nil
}

type Recorder struct {
	BaseURL string
	Client  *http.Client
}

func isNilValue(value reflect.Value) bool {
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Slice, reflect.Map, reflect.Func:
		return value.IsNil()
	default:
		return false
	}
}
