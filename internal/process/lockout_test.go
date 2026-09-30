package process_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wyw14/cry-97/internal/model"
	"github.com/wyw14/cry-97/internal/process"
)

func newTestPlant(t *testing.T) (*process.Plant, func() time.Time) {
	t.Helper()
	dir := t.TempDir()
	clock := func() time.Time { return time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC) }
	lines := []model.LineID{"line-1", "line-2", "line-3"}
	plant, err := process.NewPlant(dir, lines, clock)
	if err != nil {
		t.Fatalf("new plant: %v", err)
	}
	ctx := context.Background()
	for _, lineID := range lines {
		if _, err := plant.RecoverLine(ctx, lineID); err != nil {
			t.Fatalf("recover %s: %v", lineID, err)
		}
	}
	plant.ArmSafety()
	return plant, clock
}

func TestPumpLockoutBlocksCommandsAndSuspendsSequence(t *testing.T) {
	plant, _ := newTestPlant(t)
	ctx := context.Background()
	const line = model.LineID("line-1")
	const pumpB = "pump-line-1-B"

	// 在封锁前启动一个在途污泥回流序列（占用 pump B）。
	sequence, err := plant.StartSludgeSequence(line, pumpB, 7)
	if err != nil {
		t.Fatalf("start sequence: %v", err)
	}
	if _, err := plant.ConfirmSludgeFlow(line, sequence.ID, sequence.Generation, 42); err != nil {
		t.Fatalf("confirm flow: %v", err)
	}

	// 登记停用挂牌。
	tagged, err := plant.TagDevice(ctx, process.TagDeviceInput{
		DeviceID: pumpB, Reason: "机械密封更换", Owner: "zhang.wei", TaggedBy: "li.fang",
	})
	if err != nil {
		t.Fatalf("tag device: %v", err)
	}
	if !tagged.Active() {
		t.Fatalf("expected active tag")
	}
	if len(tagged.Checks) == 0 {
		t.Fatalf("expected mandatory safety checklist")
	}

	// 在途序列应被挂起，泵停机锁定。
	status, err := plant.Status(line)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Sequence == nil || status.Sequence.Status != "suspended" {
		t.Fatalf("expected suspended sequence, got %+v", status.Sequence)
	}
	for _, pump := range status.Pumps {
		if pump.ID == pumpB && (pump.Running || !pump.Locked) {
			t.Fatalf("expected locked stopped pump, got %+v", pump)
		}
	}

	// 封锁期间拒绝相关控制命令：新序列、流量确认（序列已挂起）、交接均不能落到该泵。
	if _, err := plant.StartSludgeSequence(line, pumpB, 8); !errors.Is(err, model.ErrDeviceLocked) {
		t.Fatalf("expected ErrDeviceLocked on new sequence, got %v", err)
	}
	if _, err := plant.ConfirmSludgeFlow(line, sequence.ID, sequence.Generation, 50); err == nil {
		t.Fatalf("expected flow confirmation rejected while sequence is suspended")
	}
	if _, err := plant.StartHandover(line, "pump-line-1-A", pumpB); !errors.Is(err, model.ErrDeviceLocked) {
		t.Fatalf("expected ErrDeviceLocked on handover, got %v", err)
	}
	if status.LockedCount != 1 {
		t.Fatalf("expected one active lockout on line, got %d", status.LockedCount)
	}

	// 未确认全部检查项不能运行前校验，也不能复役。
	first := tagged.Checks[0]
	if _, err := plant.ConfirmLockCheck(ctx, pumpB, first.Code, "zhang.wei"); err != nil {
		t.Fatalf("confirm check: %v", err)
	}
	if _, err := plant.VerifyPreRun(ctx, pumpB, ""); !errors.Is(err, model.ErrChecksIncomplete) {
		t.Fatalf("expected incomplete checks, got %v", err)
	}
	if _, err := plant.ReleaseDevice(ctx, pumpB, "li.fang"); !errors.Is(err, model.ErrChecksIncomplete) {
		t.Fatalf("expected release blocked by checks, got %v", err)
	}

	// 确认剩余检查项。
	for _, check := range tagged.Checks[1:] {
		if _, err := plant.ConfirmLockCheck(ctx, pumpB, check.Code, "zhang.wei"); err != nil {
			t.Fatalf("confirm check %s: %v", check.Code, err)
		}
	}

	// 全部检查项确认后执行运行前校验，再复役。
	verified, err := plant.VerifyPreRun(ctx, pumpB, "现场试运转前检查通过")
	if err != nil {
		t.Fatalf("pre-run verify: %v", err)
	}
	if !verified.PreRunVerified {
		t.Fatalf("expected pre-run verified")
	}
	released, err := plant.ReleaseDevice(ctx, pumpB, "li.fang")
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if released.Active() {
		t.Fatalf("expected released tag")
	}

	// 复役后泵解锁，序列回到切换中，等待现场重新确认流量。
	status, err = plant.Status(line)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Sequence == nil || status.Sequence.Status != "switching" {
		t.Fatalf("expected resumed switching sequence, got %+v", status.Sequence)
	}
	for _, pump := range status.Pumps {
		if pump.ID == pumpB && pump.Locked {
			t.Fatalf("expected unlocked pump after release")
		}
	}
}

func TestBlowerLockoutRejectsAerationControl(t *testing.T) {
	plant, _ := newTestPlant(t)
	ctx := context.Background()
	const blower = "blower-basin-line-2"

	if _, err := plant.TagDevice(ctx, process.TagDeviceInput{
		DeviceID: blower, Reason: "叶轮检查", Owner: "wang.min", TaggedBy: "li.fang",
	}); err != nil {
		t.Fatalf("tag blower: %v", err)
	}
	if _, _, err := plant.SubmitAeration(ctx, "line-2", "basin-line-2", nil); !errors.Is(err, model.ErrDeviceLocked) {
		t.Fatalf("expected aeration control blocked, got %v", err)
	}
	for _, blowerState := range plant.StatusBlowers() {
		if blowerState.ID == blower && (!blowerState.Locked || blowerState.Running) {
			t.Fatalf("expected locked stopped blower, got %+v", blowerState)
		}
	}
}

func TestValveLockoutClosesAndRequiresFreshPermit(t *testing.T) {
	plant, _ := newTestPlant(t)
	ctx := context.Background()
	const valve = "valve-line-3"

	tagged, err := plant.TagDevice(ctx, process.TagDeviceInput{
		DeviceID: valve, Reason: "阀座检修", Owner: "chen.gang", TaggedBy: "li.fang",
	})
	if err != nil {
		t.Fatalf("tag valve: %v", err)
	}
	var found bool
	for _, valveState := range plant.Valves() {
		if valveState.ID == valve {
			found = true
			if valveState.Open || !valveState.Locked {
				t.Fatalf("expected closed locked valve, got %+v", valveState)
			}
		}
	}
	if !found {
		t.Fatalf("valve not present")
	}
	for _, check := range tagged.Checks {
		if _, err := plant.ConfirmLockCheck(ctx, valve, check.Code, "chen.gang"); err != nil {
			t.Fatalf("confirm: %v", err)
		}
	}
	if _, err := plant.VerifyPreRun(ctx, valve, ""); err != nil {
		t.Fatalf("pre-run: %v", err)
	}
	if _, err := plant.ReleaseDevice(ctx, valve, "li.fang"); err != nil {
		t.Fatalf("release: %v", err)
	}
	// 复役后阀门仍保持关闭，须凭新的合格化验结果重新申领许可（不自动打开）。
	for _, valveState := range plant.Valves() {
		if valveState.ID == valve && (valveState.Open || valveState.Locked) {
			t.Fatalf("expected closed unlocked valve after release, got %+v", valveState)
		}
	}
}

func TestLockoutSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	clock := func() time.Time { return time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC) }
	lines := []model.LineID{"line-1"}

	boot := func() *process.Plant {
		plant, err := process.NewPlant(dir, lines, clock)
		if err != nil {
			t.Fatalf("new plant: %v", err)
		}
		for _, lineID := range lines {
			if _, err := plant.RecoverLine(context.Background(), lineID); err != nil {
				t.Fatalf("recover: %v", err)
			}
		}
		plant.ArmSafety()
		return plant
	}

	first := boot()
	const pump = "pump-line-1-A"
	tagged, err := first.TagDevice(context.Background(), process.TagDeviceInput{
		DeviceID: pump, Reason: "电机检修", Owner: "zhao.lei", TaggedBy: "li.fang",
	})
	if err != nil {
		t.Fatalf("tag: %v", err)
	}
	if _, err := first.ConfirmLockCheck(context.Background(), pump, tagged.Checks[0].Code, "zhao.lei"); err != nil {
		t.Fatalf("check: %v", err)
	}

	// 重启：封锁状态、已确认检查项和设备锁定标志必须继续有效。
	second := boot()
	records := second.Lockouts()
	if len(records) != 1 {
		t.Fatalf("expected one record after restart, got %d", len(records))
	}
	restored := records[0]
	if !restored.Active() {
		t.Fatalf("expected active tag after restart")
	}
	confirmed := 0
	for _, check := range restored.Checks {
		if check.Confirmed {
			confirmed++
		}
	}
	if confirmed != 1 {
		t.Fatalf("expected one confirmed check restored, got %d", confirmed)
	}
	if _, err := second.StartSludgeSequence("line-1", pump, 1); !errors.Is(err, model.ErrDeviceLocked) {
		t.Fatalf("expected lockout enforced after restart, got %v", err)
	}
	status, err := second.Status("line-1")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.LockedCount != 1 {
		t.Fatalf("expected lockout visible on line after restart, got %d", status.LockedCount)
	}
	if status.AlarmCount != 1 {
		t.Fatalf("expected maintenance alarm restored after restart, got %d", status.AlarmCount)
	}
}
