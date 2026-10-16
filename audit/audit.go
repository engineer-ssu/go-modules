package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strings"

	"github.com/doug-martin/goqu/v9"
	"github.com/engineer-ssu/go-modules/audit/assemble"
	"github.com/jmoiron/sqlx"
	"github.com/wI2L/jsondiff"
)

// BaseManager 는 assemble 맵으로 엔티티의 변경 이력을 캡처한다.
//   - snapshotAssembleMap: 엔티티 맵 전체. 저장용 스냅샷(snapshot)에 사용
//   - patchAssembleMap: 이번 요청의 input 필드만 고른 맵. 수정 전/후 상태(patch 계산)에 사용
//
// NewBaseManager / NewCreationBaseManager 로만 만들어야 한다 (잠금과 수정 전 캡처를 생성 시 수행).
type BaseManager struct {
	entityType          string
	userID              int
	recordID            int
	currentVersion      int
	lastSnapshotVersion int
	oldObj              map[string]any
	newObj              map[string]any
	snapshotAssembleMap map[string]any
	patchAssembleMap    map[string]any
	snapshotSpec        *assemble.Spec
	patchSpec           *assemble.Spec
}

// NewBaseManager 는 input 에서 nil 이 아닌 필드로 snapshotAssembleMap 을 골라 patchAssembleMap 을 만들고,
// 레코드 잠금을 잡은 뒤 수정 전 상태를 oldObj 에 캡처한다. 데이터를 수정하기 전에 같은 tx 로 호출해야 한다.
//
// input 은 구조체(포인터) 또는 map[string]bool 이며, 맵에 없는 필드는 무시한다.
// 레코드 존재 여부를 확인할 수 있도록 PK 필드는 항상 남긴다.
func NewBaseManager(ctx context.Context, tx *sqlx.Tx, entityType string, userID, recordID int,
	snapshotAssembleMap map[string]any, input any) (BaseManager, error) {
	names := maps.Clone(PresentFields(input)) // input 이 넘긴 map 은 수정하지 않는다
	if pk, ok := assemble.PrimaryKeyField(snapshotAssembleMap); ok {
		names[pk] = true
	}

	b, err := newBaseManager(entityType, userID, recordID, snapshotAssembleMap, assemble.PickFields(snapshotAssembleMap, names))
	if err != nil {
		return BaseManager{}, err
	}
	if err := b.prepare(ctx, tx); err != nil {
		return BaseManager{}, err
	}
	if err := b.captureBefore(ctx, tx); err != nil {
		return BaseManager{}, err
	}
	return b, nil
}

// NewCreationBaseManager 는 생성용이다. 입력과 상관없이 생성되는 데이터(기본값 등)가 있을 수 있으므로
// 맵 전체로 생성 후 상태를 캡처하고, 생성 전 상태(oldObj)는 {} 로 기록한다.
// 레코드를 INSERT 한 뒤 같은 tx 로 호출한다.
func NewCreationBaseManager(ctx context.Context, tx *sqlx.Tx, entityType string, userID, recordID int,
	snapshotAssembleMap map[string]any) (BaseManager, error) {
	b, err := newBaseManager(entityType, userID, recordID, snapshotAssembleMap, snapshotAssembleMap)
	if err != nil {
		return BaseManager{}, err
	}
	if err := b.prepare(ctx, tx); err != nil {
		return BaseManager{}, err
	}
	b.oldObj = map[string]any{}
	return b, nil
}

func newBaseManager(entityType string, userID, recordID int, snapshotAssembleMap, patchAssembleMap map[string]any) (BaseManager, error) {
	snapshotSpec, err := assemble.Compile(snapshotAssembleMap)
	if err != nil {
		return BaseManager{}, fmt.Errorf("compile %s snapshot assemble map: %w", entityType, err)
	}
	patchSpec, err := assemble.Compile(patchAssembleMap)
	if err != nil {
		return BaseManager{}, fmt.Errorf("compile %s patch assemble map: %w", entityType, err)
	}
	return BaseManager{
		entityType:          entityType,
		userID:              userID,
		recordID:            recordID,
		snapshotAssembleMap: snapshotAssembleMap,
		patchAssembleMap:    patchAssembleMap,
		snapshotSpec:        snapshotSpec,
		patchSpec:           patchSpec,
	}, nil
}

// prepare 는 레코드 잠금을 잡고 현재 버전과 마지막 스냅샷 버전을 읽는다.
func (b *BaseManager) prepare(ctx context.Context, tx *sqlx.Tx) error {
	if err := LockRecord(ctx, tx, b.entityType, b.recordID); err != nil {
		return err
	}
	if err := b.setCurrentVersion(ctx, tx); err != nil {
		return fmt.Errorf("get %s %d current version: %w", b.entityType, b.recordID, err)
	}
	if err := b.setLastSnapshotVersion(ctx, tx); err != nil {
		return fmt.Errorf("get %s %d last snapshot version: %w", b.entityType, b.recordID, err)
	}
	return nil
}

func (b *BaseManager) EntityType() string { return b.entityType }
func (b *BaseManager) UserID() int        { return b.userID }
func (b *BaseManager) RecordID() int      { return b.recordID }

func (b *BaseManager) setCurrentVersion(ctx context.Context, tx *sqlx.Tx) error {
	var currentVersion int
	if err := tx.QueryRowContext(ctx, `
        SELECT COALESCE(MAX(version), 0) FROM record_patches
        WHERE entity_type = $1 AND record_id = $2
    `, b.entityType, b.recordID).Scan(&currentVersion); err != nil {
		return err
	}
	b.currentVersion = currentVersion
	return nil
}

func (b *BaseManager) setLastSnapshotVersion(ctx context.Context, tx *sqlx.Tx) error {
	var lastSnapshotVersion int
	if err := tx.QueryRowContext(ctx, `
        SELECT COALESCE(MAX(version), 0) FROM record_snapshots
        WHERE entity_type = $1 AND record_id = $2
    `, b.entityType, b.recordID).Scan(&lastSnapshotVersion); err != nil {
		return err
	}
	b.lastSnapshotVersion = lastSnapshotVersion
	return nil
}

// captureBefore 는 생성자에서만 호출한다 (잠금을 잡은 뒤 수정 전 상태를 읽어야 하므로).
func (b *BaseManager) captureBefore(ctx context.Context, tx *sqlx.Tx) error {
	snap, err := b.assemble(ctx, tx, b.patchSpec)
	if err != nil {
		return err
	}
	b.oldObj = snap
	return nil
}

// captureAfter 는 SavePatch 에서만 호출한다.
func (b *BaseManager) captureAfter(ctx context.Context, tx *sqlx.Tx) error {
	snap, err := b.assemble(ctx, tx, b.patchSpec)
	if err != nil {
		return err
	}
	b.newObj = snap
	return nil
}

// snapshot 은 저장용으로 맵 전체를 조립한다.
func (b *BaseManager) snapshot(ctx context.Context, tx *sqlx.Tx) (map[string]any, error) {
	return b.assemble(ctx, tx, b.snapshotSpec)
}

func (b *BaseManager) assemble(ctx context.Context, tx *sqlx.Tx, spec *assemble.Spec) (map[string]any, error) {
	nodes, err := (&assemble.Assembler{DB: tx}).Assemble(ctx, spec, []int64{int64(b.recordID)})
	if err != nil {
		return nil, fmt.Errorf("assemble %s %d: %w", b.entityType, b.recordID, err)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("%s %d does not exist", b.entityType, b.recordID)
	}
	// relation 배열을 PK 키 객체로 바꿔서, 행 추가/삭제/순서 변경이 행 id 경로로 diff 되게 한다.
	keyed, err := spec.KeyRelations(nodes[0])
	if err != nil {
		return nil, fmt.Errorf("key %s %d relations: %w", b.entityType, b.recordID, err)
	}
	return keyed, nil
}

// PresentFields 는 input 구조체에서 nil 이 아닌 필드의 json 태그 이름 집합을 반환한다.
// nil 은 필드 미전송(변경 없음), 빈 슬라이스는 전체 삭제 요청이므로 len 이 아니라 nil 여부로 판단한다.
// (json.Marshal 은 omitempty 때문에 빈 슬라이스를 빠뜨리므로 쓰지 않는다.)
// input 이 map[string]bool 이면 그대로 반환하고, nil 이거나 구조체가 아니면 빈 map 을 반환한다.
func PresentFields(input any) map[string]bool {
	if m, ok := input.(map[string]bool); ok {
		return m
	}
	result := map[string]bool{}
	value := reflect.ValueOf(input)
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return result
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return result
	}

	t := value.Type()
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" || isNilValue(value.Field(i)) {
			continue
		}
		result[name] = true
	}
	return result
}

func isNilValue(value reflect.Value) bool {
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Slice, reflect.Map, reflect.Func:
		return value.IsNil()
	default:
		return false
	}
}

const SnapshotInterval = 10

// LockRecord 는 트랜잭션이 끝날 때까지 해당 레코드의 audit 작업을 직렬화한다.
// 같은 트랜잭션에서 여러 번 호출해도 된다 (advisory lock 은 같은 세션에서 재진입 가능).
func LockRecord(ctx context.Context, tx *sqlx.Tx, entityType string, recordID int) error {
	key := fmt.Sprintf("audit:%s:%d", entityType, recordID)
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", key); err != nil {
		return fmt.Errorf("lock audit record: %w", err)
	}
	return nil
}

// diffSnapshots 는 수정 전/후 상태의 JSON Patch(RFC 6902)를 만든다.
// relation 은 KeyRelations 로 PK 키 객체가 되어 있으므로 행 변경은 /series/{id}/... 경로의
// add/remove/replace 로 기록된다. 아래 옵션은 배열로 남는 배열 컬럼(text[] 등)에 적용된다.
//   - LCS: 배열을 인덱스별이 아니라 최장 공통 부분수열로 비교한다 (가운데 값 삭제 → remove 하나).
//   - Factorize: 같은 값의 remove+add 를 move 로, 바뀌지 않은 값과 같은 add 를 copy 로 합친다.
func diffSnapshots(oldObj, newObj map[string]any) (jsondiff.Patch, error) {
	return jsondiff.Compare(oldObj, newObj, jsondiff.Factorize(), jsondiff.LCS())
}

// shouldSaveSnapshot 은 newVersion 에서 스냅샷을 저장할지 정한다.
// 최초 저장이거나, 마지막 스냅샷 이후 SnapshotInterval 개 이상 patch 가 쌓였으면 저장한다.
// 스냅샷은 수정 후 상태(= newVersion 상태)이므로 복원 시 replay 할 patch 는 최대 SnapshotInterval-1 개다.
func shouldSaveSnapshot(currentVersion, lastSnapshotVersion, newVersion int) bool {
	return currentVersion == 0 || newVersion-lastSnapshotVersion >= SnapshotInterval
}

func saveSnapshotData(ctx context.Context, tx *sqlx.Tx, b *BaseManager, version int, data map[string]any) error {
	encoded, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}
	_, err = InsertRecordSnapshot(ctx, tx, b.entityType, b.recordID, version, encoded)
	return err
}

// SavePatch 는 수정 후 상태를 캡처하고 patch/스냅샷을 저장한다. 데이터 수정을 마친 뒤 생성자와 같은 tx 로 호출한다.
func (b *BaseManager) SavePatch(ctx context.Context, tx *sqlx.Tx) (int, error) {
	if err := b.captureAfter(ctx, tx); err != nil {
		return 0, err
	}

	patch, err := diffSnapshots(b.oldObj, b.newObj)

	if err != nil {
		return 0, fmt.Errorf("generate patch: %w", err)
	}
	// 변경 사항이 없으면 저장하지 않는다. (생성은 oldObj 가 {} 이므로 항상 patch 가 있다)
	if len(patch) == 0 {
		return b.currentVersion, nil
	}
	patchJSON, err := json.Marshal(patch)
	if err != nil {
		return 0, fmt.Errorf("marshal patch: %w", err)
	}
	newVersion := b.currentVersion + 1

	if shouldSaveSnapshot(b.currentVersion, b.lastSnapshotVersion, newVersion) {
		data, err := b.snapshot(ctx, tx)
		if err != nil {
			return 0, fmt.Errorf("load after snapshot: %w", err)
		}
		if err := saveSnapshotData(ctx, tx, b, newVersion, data); err != nil {
			return 0, err
		}
	}

	if _, err := InsertRecordPatch(ctx, tx, b.entityType, b.recordID, newVersion, patchJSON, b.userID); err != nil {
		return 0, err
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
			"data":        string(data),
		}).OnConflict(goqu.DoNothing()).Prepared(true)

	query, args, err := ds.ToSQL()
	if err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("failed to exec context: %w", err)
	}

	return true, nil
}

func InsertRecordPatch(ctx context.Context, tx *sqlx.Tx, entityType string, recordID, newVersion int, patchJSON []byte, userID int) (int, error) {
	ds := goqu.Dialect("postgres").Insert("record_patches").Rows(goqu.Record{
		"entity_type": entityType,
		"record_id":   recordID,
		"version":     newVersion,
		"patch":       string(patchJSON),
		"changed_by":  userID,
	}).Prepared(true)
	query, args, err := ds.ToSQL()
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("failed to execute context: %w", err)
	}
	return 0, nil
}
