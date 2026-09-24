package daemon

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/grpmsoft/daemon/internal"
)

// pidStoreAdapter wraps internal.PIDFile to satisfy the PIDStore interface,
// converting between internal.PIDInfo and daemon.PIDInfo.
type pidStoreAdapter struct {
	file *internal.PIDFile
}

func newDefaultPIDStore(dataDir, name string) PIDStore {
	return &pidStoreAdapter{file: internal.NewPIDFile(dataDir, name)}
}

func (a *pidStoreAdapter) Save(pid, port int, name, binary string, startTime time.Time) error {
	return a.file.Save(pid, port, name, binary, startTime)
}

func (a *pidStoreAdapter) Load() (PIDInfo, error) {
	data, err := a.file.Load()
	if err != nil {
		return PIDInfo{}, err
	}
	return PIDInfo{
		PID:       data.PID,
		Port:      data.Port,
		Name:      data.Name,
		Binary:    data.Binary,
		StartTime: data.StartTime,
		Token:     data.Token,
	}, nil
}

func (a *pidStoreAdapter) Clear() error {
	return a.file.Clear()
}

func (a *pidStoreAdapter) IsAlive() bool {
	return a.file.IsAlive()
}

func (a *pidStoreAdapter) Path() string {
	return a.file.Path()
}

// defaultProcessManager delegates to internal platform-specific functions.
type defaultProcessManager struct{}

func (defaultProcessManager) StartDetached(binary string, args []string, logFile string, env []string) (int, error) {
	return internal.StartDetached(binary, args, logFile, env)
}

func (defaultProcessManager) KillProcess(ctx context.Context, pid int, grace time.Duration) error {
	return internal.KillProcess(ctx, pid, grace)
}

func (defaultProcessManager) IsProcessAlive(pid int) bool {
	return internal.IsProcessAlive(pid)
}

// defaultHealthChecker delegates to internal health check functions.
type defaultHealthChecker struct{}

func (defaultHealthChecker) Check(port int, healthPath string) error {
	return internal.CheckHealth(port, healthPath)
}

func (defaultHealthChecker) WaitUntilReady(ctx context.Context, port int, healthPath string, timeout time.Duration) error {
	return internal.WaitUntilReady(ctx, port, healthPath, timeout)
}

type healthResponse struct {
	Status string `json:"status"`
	Name   string `json:"name"`
	PID    int    `json:"pid"`
	Uptime string `json:"uptime"`
}

func defaultHealthHandler(name string, startTime time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		resp := healthResponse{
			Status: "ok",
			Name:   name,
			PID:    os.Getpid(),
			Uptime: time.Since(startTime).Truncate(time.Second).String(),
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.MarshalWrite(w, &resp); err != nil {
			http.Error(w, fmt.Sprintf("marshal: %v", err), http.StatusInternalServerError)
		}
	})
}
