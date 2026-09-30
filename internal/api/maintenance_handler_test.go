package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/wyw14/cry-97/internal/model"
	"github.com/wyw14/cry-97/internal/process"
)

func newMaintenanceTestServer(t *testing.T) *Server {
	t.Helper()
	plant, err := process.NewPlant(t.TempDir(),
		[]model.LineID{"line-1"}, func() time.Time { return time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatalf("NewPlant: %v", err)
	}
	web := fstest.MapFS{}
	server, err := NewServer(plant, web)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return server
}

func doJSON(t *testing.T, handler http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	data, _ := json.Marshal(body)
	request := httptest.NewRequest(method, path, bytes.NewReader(data))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var decoded map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &decoded)
	return response.Code, decoded
}

func TestMaintenanceLockoutHTTPLifecycle(t *testing.T) {
	server := newMaintenanceTestServer(t)
	handler := server.Handler()
	deviceID := "blower-basin-line-1"

	listCode, list := doJSON(t, handler, http.MethodGet, "/api/maintenance/devices", nil)
	if listCode != http.StatusOK || len(list["devices"].([]any)) != 4 {
		t.Fatalf("devices list: code=%d body=%v", listCode, list)
	}

	// 缺少一项封锁前检查：拒绝登记。
	placeBody := map[string]any{
		"reason": "更换轴承", "responsible": "张伟",
		"checks": []string{"isolated-power"},
	}
	code, body := doJSON(t, handler, http.MethodPost, "/api/maintenance/devices/"+deviceID+"/lockout", placeBody)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("incomplete checklist code=%d body=%v", code, body)
	}

	placeBody["checks"] = []string{"isolated-power", "zero-energy", "blower-inlet-valve"}
	code, body = doJSON(t, handler, http.MethodPost, "/api/maintenance/devices/"+deviceID+"/lockout", placeBody)
	if code != http.StatusCreated {
		t.Fatalf("place lockout code=%d body=%v", code, body)
	}

	// 封锁期间对该风机下发命令：曝气申请被拒（422，域错误映射）。
	window := []float64{2.0, 2.1, 2.0, 2.05, 2.02}
	code, body = doJSON(t, handler, http.MethodPost,
		"/api/aeration/line-1/basin-line-1/samples", window)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("locked command code=%d body=%v", code, body)
	}

	// 复役缺少检查项：拒绝。
	code, body = doJSON(t, handler, http.MethodPost,
		"/api/maintenance/devices/"+deviceID+"/restore", map[string]any{"checks": []string{}})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("restore without checks code=%d body=%v", code, body)
	}

	// 检查齐全且运行前校验通过：复役成功。
	code, body = doJSON(t, handler, http.MethodPost,
		"/api/maintenance/devices/"+deviceID+"/restore",
		map[string]any{"checks": []string{"guards-reset", "power-restored", "blower-trial-run"}})
	if code != http.StatusOK {
		t.Fatalf("restore code=%d body=%v", code, body)
	}
	if body["verification"] == "" || body["follow_up"] == "" {
		t.Fatalf("restore response missing verification: %v", body)
	}

	// 复役后命令恢复。
	code, body = doJSON(t, handler, http.MethodPost,
		"/api/aeration/line-1/basin-line-1/samples", window)
	if code != http.StatusAccepted {
		t.Fatalf("command after restore code=%d body=%v", code, body)
	}
}

func TestMaintenanceLockoutUnknownDevice(t *testing.T) {
	server := newMaintenanceTestServer(t)
	code, _ := doJSON(t, server.Handler(), http.MethodPost,
		"/api/maintenance/devices/nope/lockout",
		map[string]any{"reason": "x", "responsible": "y", "checks": []string{}})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown device code=%d, want %d", code, http.StatusUnprocessableEntity)
	}
}
