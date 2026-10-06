//go:build eval || live

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
)

// cappedLLM ограничивает платный прогон числом вызовов модели: цену ответа код
// не считает (usage без стоимости), потолок - MAX_CALLS из make-цели. Сверх
// потолка Complete отказывает, и прогон падает, а не продолжает платить.
type cappedLLM struct {
	inner Completer
	max   int64
	calls atomic.Int64
}

// newCappedLLM читает MAX_CALLS; без него прогон не стартует.
func newCappedLLM(t *testing.T, inner Completer) *cappedLLM {
	t.Helper()
	raw := os.Getenv("MAX_CALLS")
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		t.Fatalf("paid run: set MAX_CALLS to a positive number of model calls, got %q", raw)
	}
	return &cappedLLM{inner: inner, max: n}
}

func (c *cappedLLM) Complete(ctx context.Context, req Request) (json.RawMessage, error) {
	if n := c.calls.Add(1); n > c.max {
		return nil, fmt.Errorf("paid run: MAX_CALLS=%d exceeded", c.max)
	}
	return c.inner.Complete(ctx, req)
}
