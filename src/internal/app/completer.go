package app

import (
	"context"
	"encoding/json"
)

// Completer - модель, как её видят шаги диалога: запрос в схему, ответ сырым
// content. Боевая реализация - *OpenRouter; тесты пути подставляют фейк.
type Completer interface {
	Complete(ctx context.Context, req Request) (json.RawMessage, error)
}

var _ Completer = (*OpenRouter)(nil)
