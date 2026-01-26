package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	"github.com/doug-martin/goqu/v9"
	"github.com/doug-martin/goqu/v9/exp"
	"github.com/jmoiron/sqlx"
	"github.com/machinerd/go-module/db/schema"
	"github.com/machinerd/go-module/log"
	"github.com/wI2L/jsondiff"
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

type RecordVersionCounter struct {
	EntityType          string `db:"entity_type" json:"entityType" binding:"required"`
	RecordID            int    `db:"record_id" json:"recordId" binding:"required"`
	CurrentVersion      int    `db:"current_version" json:"currentVersion" binding:"required"`
	LastSnapshotVersion int    `db:"last_snapshot_version" json:"lastSnapshotVersion" binding:"required"`
}

func (p SnapshotProvider) Snapshot(ctx context.Context, id int, relationNames []string) (map[string]any, error) {
	base, err := p.LoadBase(ctx, id)
	if err != nil {
		log.Error("LoadBase failed", "err", err)
		return nil, err
	}

	snapshot, err := ToMap(base)
	if err != nil {
		log.Error("ToMap failed", "err", err)
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
		log.Error("Marshal failed", "err", err)
		return nil, err
	}

	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		log.Error("Unmarshal failed", "err", err)
		return nil, err
	}
	return result, nil
}

func isNilValue(value reflect.Value) bool {
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Slice, reflect.Map, reflect.Func:
		return value.IsNil()
	default:
		return false
	}
}

// 1. Patch 생성 및 저장 (스냅샷 주기 체크 포함)
func SavePatch(dbx *sqlx.DB, entityType string, recordID int, oldObj, newObj map[string]any, userID int) (int, error) {
	// 2. 카운터 행 배타 락(FOR UPDATE) 조회
	ctx := context.Background()
	tx := dbx.MustBegin()

	// 저상 커밋시 적용되지 않음.
	defer tx.Rollback()

	recordViewCounter, err := GetRecordVersionCounter(tx, entityType, recordID)

	if err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("failed to lock version counter: %w", err)
	}

	lastSnapshotVersion := recordViewCounter.LastSnapshotVersion
	currentVersion := recordViewCounter.CurrentVersion

	// 3. [핵심] 자가 치유(Self-Healing) 스냅샷 검사
	// 벌크 SQL/스크립트 등으로 SavePatch를 거치지 않고 Patch가 싸여
	// 스냅샷 경계(10의 배수)를 건너뛰었더라도, Patch 생성 '전'에 갭을 메웁니다.
	if currentVersion-lastSnapshotVersion >= 10 || (currentVersion == 0 && lastSnapshotVersion == 0) {
		oldJSON, err := json.Marshal(oldObj)
		if err != nil {
			return 0, fmt.Errorf("failed to marshal OldObj for healing snapshot: %w", err)
		}
		_, err = InsertRecordSnapshot(ctx, tx, entityType, recordID, currentVersion, oldJSON)
		if err != nil {
			return 0, fmt.Errorf("failed to insert self-healing snapshot: %w", err)
		}

		// 치유 스냅샷 버전 업데이트
		lastSnapshotVersion = currentVersion
	}

	// 4. RFC 6902 Patch 생성 (oldObj vs newObj)
	patch, err := jsondiff.Compare(oldObj, newObj)
	if err != nil {
		return 0, fmt.Errorf("failed to generate json diff patch: %w", err)
	}

	// 변경 사항이 전혀 없는 경우(Patch가 빈 배열) 저장 생략 가능 (필요 시 주석 해제)
	if len(patch) == 0 {
		return currentVersion, nil
	}

	newVersion, err := UpsertVersionCounter(ctx, tx, entityType, recordID, lastSnapshotVersion)
	if err != nil {
		return 0, fmt.Errorf("failed to upsert version counter:  %w", err)
	}

	newVersionIsSnapshotBoundary := newVersion%10 == 0

	// 6. 정기 스냅샷 생성 (신규 버전이 10의 배수인 경우)
	if newVersionIsSnapshotBoundary {
		newJSON, _ := json.Marshal(newObj)

		_, err := InsertRecordSnapshot(ctx, tx, entityType, recordID, newVersion, newJSON)
		if err != nil {
			return 0, fmt.Errorf("failed to insert record snapshot:  %w", err)
		}

		// 카운터 테이블의 last_snapshot_version 최신화
		_, err = UpdateVersionCounter(ctx, tx, entityType, recordID, newVersion)
		if err != nil {
			fmt.Println(err)
			return -1, fmt.Errorf("failed to update version counter:  %w", err)
		}
	}

	patchJSON, err := json.Marshal(patch)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal patch: %w", err)
	}
	err = InsertRecordPatch(ctx, tx, entityType, recordID, newVersion, patchJSON, userID)
	if err != nil {
		return 0, fmt.Errorf("failed to insert patch record: %w", err)
	}

	// 8. 트랜잭션 커밋
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit: %w", err)
	}

	return newVersion, nil
}

func InsertRecordSnapshot(ctx context.Context, tx *sqlx.Tx, entityType string, recordID, currentVersion int, data []byte) (bool, error) {
	ds := goqu.Dialect("postgres").
		Insert("record_snapshots").
		Rows(goqu.Record{
			"entity_type": entityType,
			"record_id":   recordID,
			"version":     currentVersion,
			"data":        data,
		}).OnConflict(goqu.DoNothing())

	query, args, _ := ds.ToSQL()
	_, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("failed to execute context: %w", err)
	}

	return true, nil
}

func GetRecordVersionCounter(tx *sqlx.Tx, entityType string, recordID int) (*RecordVersionCounter, error) {
	var data RecordVersionCounter

	ds := goqu.Dialect("postgres").
		From(goqu.T("record_version_counters")).
		Select(schema.GetFields(RecordVersionCounter{})...).
		Where(goqu.I("entity_type").Eq(entityType), goqu.I("record_id").Eq(recordID)).Limit(1).ForUpdate(exp.Wait)

	query, _, _ := ds.ToSQL()

	err := tx.Get(&data, query)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("failed to get data: %w", err)
	}

	return &data, nil
}

func UpdateVersionCounter(ctx context.Context, tx *sqlx.Tx, entityType string, recordID int, newVersion int) (bool, error) {

	ds := goqu.Update("record_version_counters").
		Set(goqu.Record{"last_snapshot_version": newVersion}).
		Where(goqu.I("entity_type").Eq(entityType), goqu.I("record_id").Eq(recordID))

	query, args, _ := ds.ToSQL()
	_, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("failed to update last_snapshot_version: %w", err)
	}

	return true, nil

}

func UpsertVersionCounter(ctx context.Context, tx *sqlx.Tx, entityType string, recordID int, newVersion int) (int, error) {
	ds := goqu.Dialect("postgres").
		Insert("record_version_counters").
		Rows(goqu.Record{
			"entity_type":           entityType,
			"record_id":             recordID,
			"current_version":       1,
			"last_snapshot_version": newVersion,
		}).
		OnConflict(
			goqu.DoUpdate(
				"entity_type, record_id",
				goqu.Record{
					"current_version": goqu.L(
						"record_version_counters.current_version + 1",
					),
					"last_snapshot_version": newVersion,
				},
			),
		).
		Returning("current_version").
		Prepared(true)

	query, args, err := ds.ToSQL()
	if err != nil {
		return -1, fmt.Errorf("failed to get query: %w", err)
	}

	var currentVersion int
	if err = tx.GetContext(ctx, &currentVersion, query, args...); err != nil {
		return -1, fmt.Errorf("failed to execute context: %w", err)
	}

	return int(currentVersion), nil
}

func InsertRecordPatch(ctx context.Context, tx *sqlx.Tx, entityType string, recordID, newVersion int, patchJSON []byte, userID int) error {
	ds := goqu.Dialect("postgres").Insert("record_patches").Rows(goqu.Record{
		"entity_type": entityType,
		"record_id":   recordID,
		"version":     newVersion,
		"patch":       patchJSON,
		"changed_by":  userID,
	})
	query, args, _ := ds.ToSQL()
	_, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("failed to execute context: %w", err)
	}
	return nil
}
