package proxy

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/horunu/budget-governor/gateway/internal/budget"
	"github.com/horunu/budget-governor/gateway/internal/provider"
)

// sseChunkWriter adapts gin's ResponseWriter into a provider.StreamWriter,
// translating each normalized provider.StreamChunk into an
// OpenAI-compatible `chat.completion.chunk` SSE event as it arrives, so
// existing OpenAI-SDK streaming clients work against this gateway
// unmodified.
type sseChunkWriter struct {
	c            *gin.Context
	id           string
	model        string
	flusher      http.Flusher
	wroteHeaders bool
	finalUsage   provider.Usage
}

type sseDelta struct {
	Content string `json:"content,omitempty"`
}

type sseChoice struct {
	Index        int      `json:"index"`
	Delta        sseDelta `json:"delta"`
	FinishReason *string  `json:"finish_reason"`
}

type sseChunkPayload struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Model   string      `json:"model"`
	Choices []sseChoice `json:"choices"`
}

func (w *sseChunkWriter) ensureHeaders() {
	if w.wroteHeaders {
		return
	}
	w.c.Header("Content-Type", "text/event-stream")
	w.c.Header("Cache-Control", "no-cache")
	w.c.Header("Connection", "keep-alive")
	w.c.Status(http.StatusOK)
	w.wroteHeaders = true
}

func (w *sseChunkWriter) Write(chunk provider.StreamChunk) error {
	w.ensureHeaders()

	if chunk.Done {
		w.finalUsage = chunk.Usage
		_, err := w.c.Writer.Write([]byte("data: [DONE]\n\n"))
		w.flusher.Flush()
		return err
	}

	var finishPtr *string
	if chunk.FinishReason != "" {
		finishPtr = &chunk.FinishReason
	}
	payload := sseChunkPayload{
		ID:     w.id,
		Object: "chat.completion.chunk",
		Model:  w.model,
		Choices: []sseChoice{{
			Index:        0,
			Delta:        sseDelta{Content: chunk.DeltaContent},
			FinishReason: finishPtr,
		}},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := w.c.Writer.Write([]byte("data: ")); err != nil {
		return err
	}
	if _, err := w.c.Writer.Write(b); err != nil {
		return err
	}
	if _, err := w.c.Writer.Write([]byte("\n\n")); err != nil {
		return err
	}
	w.flusher.Flush()
	return nil
}

// streamDispatch mirrors doDispatch's non-streaming path but for SSE:
// budget was already reserved by the caller before this runs, and is
// reconciled against actual usage once the stream's terminal chunk
// (carrying Usage) arrives.
func (h *Handler) streamDispatch(c *gin.Context, req provider.Request, refs []budget.BucketRef, estTokens int64, estUSD float64, decisionLabel, advice string, start time.Time, p provider.Provider) {
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		// Should not happen with gin's default ResponseWriter, but fail
		// safely rather than panicking on an exotic transport.
		c.JSON(http.StatusInternalServerError, ErrorResponse{}.withMessage("streaming not supported by this transport", "internal_error"))
		return
	}

	writer := &sseChunkWriter{c: c, id: req.TraceID, model: req.Model, flusher: flusher}
	if advice != "" {
		c.Header("X-Budget-Advisory", advice)
	}

	err := p.Stream(c.Request.Context(), req, writer)
	if err != nil {
		if !writer.wroteHeaders {
			h.handleProviderError(c, req, refs, estTokens, estUSD, start, err)
			return
		}
		// Headers/partial body already flushed to the client; we can no
		// longer switch to a JSON error response. Log it and reconcile
		// whatever usage we do have (possibly zero).
		h.Logf.Error("stream error after headers sent", "trace_id", req.TraceID, "error", err)
	}

	actualUSD := provider.CostUSD(req.Model, writer.finalUsage.TokensIn, 0, writer.finalUsage.TokensOut)
	if rerr := h.Enforcer.Reconcile(c.Request.Context(), refs, estTokens, estUSD, int64(writer.finalUsage.TokensIn+writer.finalUsage.TokensOut), actualUSD); rerr != nil {
		h.Logf.Warn("bucket reconcile failed (stream)", "trace_id", req.TraceID, "error", rerr)
	}

	h.finish(c, req, refs, budget.Decision{Allowed: true}, decisionLabel, "", start, writer.finalUsage, actualUSD, http.StatusOK)
}
