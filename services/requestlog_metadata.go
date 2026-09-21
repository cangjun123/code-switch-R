package services

import (
	"bytes"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const requestedModelContextKey = "request_log_requested_model"

func populateRequestLogIdentity(c *gin.Context, entry *ReqeustLog) {
	if c == nil || entry == nil {
		return
	}
	entry.RequestedModel = c.GetString(requestedModelContextKey)
	if key := relayKeyFromContext(c); key != nil {
		entry.RelayKeyName = key.Name
	}
}

// Only inspect model metadata in upstream envelopes, never content/tool arguments
// or the requested model. Missing metadata must remain unknown.
func captureResponseModel(data string, entry *ReqeustLog) {
	if entry == nil {
		return
	}
	for _, path := range []string{"response.model", "message.model", "model", "modelVersion"} {
		value := gjson.Get(data, path)
		if value.Type == gjson.String {
			if model := strings.TrimSpace(value.String()); model != "" {
				if entry.ResponseModel != model {
					entry.ResponseModel = model
					defaultActiveRequestTracker.Update(entry.ActiveRequestID, entry)
				}
				return
			}
		}
	}
}

// Image events may contain large base64 payloads. Bound the observation buffer
// independently of forwarding; oversized lines are forwarded without inspection.
type responseModelObserver struct {
	entry    *ReqeustLog
	line     []byte
	skipping bool
}

func newResponseModelObserver(entry *ReqeustLog) *responseModelObserver {
	return &responseModelObserver{entry: entry}
}

func (o *responseModelObserver) Write(data []byte) {
	const maxLine = 8 << 20
	for len(data) > 0 {
		end := bytes.IndexByte(data, '\n')
		part := data
		if end >= 0 {
			part = data[:end]
		}
		if !o.skipping {
			if len(o.line)+len(part) > maxLine {
				o.line = nil
				o.skipping = true
			} else {
				o.line = append(o.line, part...)
			}
		}
		if end < 0 {
			return
		}
		o.Finish()
		data = data[end+1:]
	}
}

func (o *responseModelObserver) Finish() {
	if !o.skipping {
		parseEventPayload(string(o.line), captureResponseModel, o.entry)
	}
	o.line = o.line[:0]
	o.skipping = false
}
