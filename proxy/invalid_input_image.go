package proxy

import (
	"errors"
	"strings"

	"github.com/tidwall/gjson"
)

// isInvalidInputImagePayload recognizes a deterministic input failure, not a
// generic invalid_value or a diagnostic mention of images. Replaying the same
// bytes on another account cannot repair them, even under catch-all retry.
func isInvalidInputImagePayload(payload []byte) bool {
	if !gjson.ValidBytes(payload) {
		return false
	}
	root := gjson.ParseBytes(payload)
	for _, path := range []string{"error", "response.error", "response.status_details.error", ""} {
		problem := root
		if path != "" {
			problem = root.Get(path)
		}
		if !problem.IsObject() ||
			problem.Get("code").String() != "invalid_value" ||
			problem.Get("type").String() != "invalid_request_error" ||
			problem.Get("param").String() != "input" {
			continue
		}
		message := strings.ToLower(strings.Join(strings.Fields(problem.Get("message").String()), " "))
		switch message {
		case "the image data you provided does not represent a valid image.",
			"the image data you provided does not represent a valid image. please check your input and try again.":
			return true
		}
	}
	return false
}

func isInvalidInputImageRequestError(err error) bool {
	if isInvalidInputImagePayload(imageResponseFailedPayload(err)) {
		return true
	}
	var upstreamErr continuousRetryHTTPError
	return errors.As(err, &upstreamErr) && isInvalidInputImagePayload(upstreamErr.UpstreamErrorBody())
}
