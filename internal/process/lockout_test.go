package process

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/maintenance"
	"github.com/wyw14/cry-97/internal/model"
)

var testLineIDs = []model.LineID{"line-1", "line-2", "line-3"}

func newTestPlant(t *testing.T) (*Plant, time.Time) {
	t.Helper()
	return newPlantIn(t, t.TempDir(), 8)
}

func newPlantIn(t *testing.T, dataDir string, hour int) (*Plant, time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 30, hour, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	plant, err := NewPlant(dataDir, testLineIDs, clock)
	if err != nil {
		t.Fatalf("NewPlant: %v", err)
	}
	return plant, now
}

func newPlantAt(t *testing.T, dataDir string) (*Plant, time.Time) {
	t.Helper()
	return newPlantIn(t, dataDir, 9)
}

func uuidMust(t *testing.T) uuid.UUID {
	t.Helper()
	return uuid.New()
}

func checklistIDs(plant *Plant, deviceID string, phase maintenance.Phase) []string {
	for _, view := range plant.MaintenanceDevices() {
		if view.Device.ID != deviceID {
			continue
		}
		ids := make([]string, 0, len(view.Checks))
		for _, check := range view.Checks {
			if check.Phase == phase {
				ids = append(ids, check.ID)
			}
		}
		return ids
	}
	return nil
}

func activeLockout(plant *Plant, deviceID string) (maintenance.Lockout, bool) {
	return plant.maintenance.Get(deviceID)
}

func findLockoutAlarm(plant *Plant, lineID model.LineID) *model.Alarm {
	for _, value := range plant.Alarms() {
		if value.Code == lockoutAlarmCode && value.LineID == lineID {
			return &value
		}
	}
	return nil
}

// 封锁风机：设备停机、在途控制被拒绝、报警与线路状态可见。
func TestBlowerLockoutStopsAndRejectsCommands(t *testing.T) {
	ctx := context.Background()
	plant, _ := newTestPlant(t)
	deviceID := "blower-basin-line-1"

	stable := []byte(`[2.0,2.1,2.0,2.05,2.02]`)
	if _, command, err := plant.SubmitAeration(ctx, "line-1", "basin-line-1", stable); err != nil || command == nil {
		t.Fatalf("SubmitAeration before lockout: command=%v err=%v", command, err)
	}
	blowers := plant.StatusBlowers()
	var running bool
	for _, blower := range blowers {
		if blower.ID == deviceID {
			running = blower.Running
		}
	}
	if !running {
		t.Fatalf("expected blower running before lockout")
	}

	if _, err := plant.PlaceLockout(ctx, deviceID, "更换轴承", "张伟",
		checklistIDs(plant, deviceID, maintenance.PhasePlacement)); err != nil {
		t.Fatalf("PlaceLockout: %v", err)
	}

	for _, blower := range plant.StatusBlowers() {
		if blower.ID == deviceID && (blower.Running || blower.Frequency != 0) {
			t.Fatalf("blower must be stopped on lockout: %+v", blower)
		}
	}
	if _, _, err := plant.SubmitAeration(ctx, "line-1", "basin-line-1", stable); !errors.Is(err, maintenance.ErrDeviceLockedOut) {
		t.Fatalf("expected ErrDeviceLockedOut, got %v", err)
	}
	if _, ok := activeLockout(plant, deviceID); !ok {
		t.Fatalf("lockout record missing")
	}
	status, err := plant.Status("line-1")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(status.Lockouts) != 1 {
		t.Fatalf("expected line status to expose one lockout, got %d", len(status.Lockouts))
	}
	raised := findLockoutAlarm(plant, "line-1")
	if raised == nil || !raised.Active() {
		t.Fatalf("expected an active maintenance alarm")
	}

	// 复役缺少检查项必须被拒绝，封锁继续有效。
	if _, err := plant.ClearLockout(ctx, deviceID, nil); err == nil {
		t.Fatalf("ClearLockout without return checks must fail")
	}
	if _, ok := activeLockout(plant, deviceID); !ok {
		t.Fatalf("lockout must remain active after failed clearance")
	}

	// 复役检查齐全并重新执行运行前校验后恢复。
	result, err := plant.ClearLockout(ctx, deviceID,
		checklistIDs(plant, deviceID, maintenance.PhaseReturn))
	if err != nil {
		t.Fatalf("ClearLockout: %v", err)
	}
	if result.Verified == "" || result.FollowUp == "" {
		t.Fatalf("clear result missing verification/follow-up: %+v", result)
	}
	if _, ok := activeLockout(plant, deviceID); ok {
		t.Fatalf("lockout must be cleared")
	}
	if _, _, err := plant.SubmitAeration(ctx, "line-1", "basin-line-1", stable); err != nil {
		t.Fatalf("commands must flow again after restoration: %v", err)
	}
	if alarmValue := findLockoutAlarm(plant, "line-1"); alarmValue == nil || alarmValue.Active() {
		t.Fatalf("maintenance alarm must be recovered on clearance")
	}
}

// 封锁前安全检查缺项不得登记，设备保持在役。
func TestPlaceLockoutRejectsIncompleteChecklist(t *testing.T) {
	ctx := context.Background()
	plant, _ := newTestPlant(t)
	deviceID := "blower-basin-line-2"

	all := checklistIDs(plant, deviceID, maintenance.PhasePlacement)
	if _, err := plant.PlaceLockout(ctx, deviceID, "巡检", "李娜", all[:len(all)-1]); err == nil {
		t.Fatalf("PlaceLockout with missing check must fail")
	}
	if _, ok := activeLockout(plant, deviceID); ok {
		t.Fatalf("device must not be locked out when checklist incomplete")
	}
}

// 泵封锁：在途回流序列挂起、流量确认被拒；复役后退回确认门并可重新确认。
func TestPumpLockoutSuspendsSequence(t *testing.T) {
	ctx := context.Background()
	plant, _ := newTestPlant(t)
	deviceID := "pump-line-1-A"

	sequence, err := plant.StartSludgeSequence("line-1", deviceID, 1)
	if err != nil {
		t.Fatalf("StartSludgeSequence: %v", err)
	}
	record, err := plant.PlaceLockout(ctx, deviceID, "更换机封", "王强",
		checklistIDs(plant, deviceID, maintenance.PhasePlacement))
	if err != nil {
		t.Fatalf("PlaceLockout: %v", err)
	}
	if len(record.Suspended) == 0 || record.Suspended[0].Kind != "sludge_sequence" {
		t.Fatalf("expected sludge sequence suspension, got %+v", record.Suspended)
	}
	for _, pump := range plant.Pumps() {
		if pump.ID == deviceID && pump.Running {
			t.Fatalf("locked pump must be stopped")
		}
	}
	if current, ok := plant.sequences.Current("line-1"); !ok || string(current.Status) != "suspended" {
		t.Fatalf("sequence must be suspended, got %+v", current)
	}
	if _, err := plant.ConfirmSludgeFlow("line-1", sequence.ID, sequence.Generation, 42); !errors.Is(err, maintenance.ErrDeviceLockedOut) {
		t.Fatalf("flow confirmation under suspension must be refused, got %v", err)
	}

	result, err := plant.ClearLockout(ctx, deviceID,
		checklistIDs(plant, deviceID, maintenance.PhaseReturn))
	if err != nil {
		t.Fatalf("ClearLockout: %v", err)
	}
	if len(result.Resumed) == 0 {
		t.Fatalf("expected suspended actions to be resumed")
	}
	if current, ok := plant.sequences.Current("line-1"); !ok || string(current.Status) != "switching" {
		t.Fatalf("sequence must return to switching gate, got %+v", current)
	}
	if _, err := plant.ConfirmSludgeFlow("line-1", sequence.ID, sequence.Generation, 42); err != nil {
		t.Fatalf("flow re-confirmation after restoration: %v", err)
	}
	for _, pump := range plant.Pumps() {
		if pump.ID == deviceID && (!pump.Running || pump.Flow != 42) {
			t.Fatalf("pump should run after re-confirmation: %+v", pump)
		}
	}
}

// 主备交接涉及的泵被封锁时交接挂起，新交接申请被拒绝。
func TestHandoverRejectsLockedPump(t *testing.T) {
	ctx := context.Background()
	plant, _ := newTestPlant(t)
	if _, err := plant.StartHandover("line-1", "pump-line-1-A", "pump-line-1-B"); err != nil {
		t.Fatalf("StartHandover: %v", err)
	}
	if _, err := plant.PlaceLockout(ctx, "pump-line-1-B", "检修", "赵磊",
		checklistIDs(plant, "pump-line-1-B", maintenance.PhasePlacement)); err != nil {
		t.Fatalf("PlaceLockout: %v", err)
	}
	if handover, ok := plant.handovers.Current("line-1"); !ok || string(handover.Phase) != "suspended" {
		t.Fatalf("handover must be suspended, got %+v", handover)
	}
	if _, err := plant.StartHandover("line-2", "pump-line-2-A", "pump-line-1-B"); !errors.Is(err, maintenance.ErrDeviceLockedOut) {
		t.Fatalf("new handover to locked pump must be refused, got %v", err)
	}
}

// 排水阀封锁：阀门强制关闭、许可引用失效；开启被拒；复役须通过校验。
func TestValveLockoutClosesAndBlocksOpen(t *testing.T) {
	ctx := context.Background()
	plant, now := newTestPlant(t)
	deviceID := "valve-line-3"

	if err := plant.valves.Open("line-3", uuidMust(t), now); err != nil {
		t.Fatalf("seed open valve: %v", err)
	}
	if _, err := plant.PlaceLockout(ctx, deviceID, "阀座检修", "孙敏",
		checklistIDs(plant, deviceID, maintenance.PhasePlacement)); err != nil {
		t.Fatalf("PlaceLockout: %v", err)
	}
	for _, valve := range plant.valves.Snapshot() {
		if valve.ID == deviceID && valve.Open {
			t.Fatalf("locked valve must be forced closed")
		}
	}
	if err := plant.valves.Open("line-3", uuidMust(t), now); !errors.Is(err, maintenance.ErrDeviceLockedOut) {
		t.Fatalf("opening a locked valve must be refused, got %v", err)
	}
	if _, err := plant.ClearLockout(ctx, deviceID,
		checklistIDs(plant, deviceID, maintenance.PhaseReturn)); err != nil {
		t.Fatalf("ClearLockout: %v", err)
	}
	if _, ok := activeLockout(plant, deviceID); ok {
		t.Fatalf("valve lockout must be cleared after verification")
	}
}

// 失败序列生成的停止补偿在封锁期间挂起，复役后重新回到队列。
func TestLockoutSuspendsPendingCompensation(t *testing.T) {
	ctx := context.Background()
	plant, _ := newTestPlant(t)
	deviceID := "pump-line-1-B"

	sequence, err := plant.StartSludgeSequence("line-1", deviceID, 1)
	if err != nil {
		t.Fatalf("StartSludgeSequence: %v", err)
	}
	if _, err := plant.ConfirmSludgeFlow("line-1", sequence.ID, sequence.Generation, 33); err != nil {
		t.Fatalf("ConfirmSludgeFlow: %v", err)
	}
	if _, err := plant.FailSludgeSequence("line-1"); err != nil {
		t.Fatalf("FailSludgeSequence: %v", err)
	}
	if len(plant.compensations.Pending()) != 1 {
		t.Fatalf("expected one pending stop compensation")
	}
	if _, err := plant.PlaceLockout(ctx, deviceID, "抢修", "周涛",
		checklistIDs(plant, deviceID, maintenance.PhasePlacement)); err != nil {
		t.Fatalf("PlaceLockout: %v", err)
	}
	if len(plant.compensations.Pending()) != 0 {
		t.Fatalf("compensation must be held while the pump is locked out")
	}
	if _, err := plant.ClearLockout(ctx, deviceID,
		checklistIDs(plant, deviceID, maintenance.PhaseReturn)); err != nil {
		t.Fatalf("ClearLockout: %v", err)
	}
	pending := plant.compensations.Pending()
	if len(pending) != 1 || pending[0].DeviceID != deviceID {
		t.Fatalf("compensation must be rescheduled on restoration, got %+v", pending)
	}
}

// 重启后封锁继续有效：命令仍被拒绝、告警仍可见。
func TestLockoutSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	plant, _ := newPlantIn(t, dataDir, 8)
	deviceID := "pump-line-2-A"

	if _, err := plant.PlaceLockout(ctx, deviceID, "长期检修", "吴芳",
		checklistIDs(plant, deviceID, maintenance.PhasePlacement)); err != nil {
		t.Fatalf("PlaceLockout: %v", err)
	}

	restarted, now := newPlantAt(t, dataDir)
	for _, lineID := range []model.LineID{"line-1", "line-2", "line-3"} {
		if _, err := restarted.RecoverLine(ctx, lineID); err != nil {
			t.Fatalf("RecoverLine: %v", err)
		}
	}
	if err := restarted.RecoverLockouts(now); err != nil {
		t.Fatalf("RecoverLockouts: %v", err)
	}

	if !restarted.maintenance.Blocked(deviceID) {
		t.Fatalf("lockout must keep blocking after restart")
	}
	if _, err := restarted.StartSludgeSequence("line-2", deviceID, 1); !errors.Is(err, maintenance.ErrDeviceLockedOut) {
		t.Fatalf("locked pump must reject assignments after restart, got %v", err)
	}
	if findLockoutAlarm(restarted, "line-2") == nil {
		t.Fatalf("maintenance alarm must be visible after restart")
	}
}
