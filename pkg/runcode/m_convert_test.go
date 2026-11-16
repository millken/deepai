package runcode

import (
	"context"
	"testing"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// Review M: a handler that returns Status=failed with EMPTY Error and nil err
// (figma's failure shape, among others) must not nil-deref in
// registryHost.Call — the panic would land on the driver goroutine, where the
// qjs host recover cannot catch it.
func TestRegistryHostFailedEmptyErrorNilErr(t *testing.T) {
	reg := tools.NewRegistry()
	_ = reg.Register(models.Tool{
		Name: "figma",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			// Deliberately the reviewed shape: failed, no Error, no err.
			return models.ToolResult{CallID: c.ID, ToolName: c.Name, Status: models.CallStatusFailed}, nil
		},
	})
	host := NewRegistryHost(&tools.Dispatch{Registry: reg}, "rc_m")
	res := host.Call(context.Background(), "figma", nil)
	if res.Err == "" {
		t.Fatal("failed result must reject with a non-empty message")
	}
}
