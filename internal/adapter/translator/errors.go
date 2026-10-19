package translator

import "fmt"

// UnsupportedContentError reports request content the translator can parse
// but cannot express in the target format. Handlers map it to
// 415 Unsupported Media Type so a client gets an explicit rejection instead
// of a request silently served with the content dropped - the defect class
// the Anthropic image path shipped with for its whole life (image blocks
// parsed, logged at Debug, discarded: HTTP 200 with the model answering a
// text-only prompt).
type UnsupportedContentError struct {
	// Detail names the offending content for the client-facing message.
	Detail string
}

func (e *UnsupportedContentError) Error() string {
	return fmt.Sprintf("unsupported content: %s", e.Detail)
}
