// Package agent — token-usage capture helpers (OGA-420).
package agent

import "github.com/ontogisai/oga-kit-sdk/gateway"

// UsageFromGateway converts a gateway.Usage into the agent-package TokenUsage
// wire type. Returns (zero, false) when the gateway returned no usage, so
// callers can label the counts as unavailable rather than as a real "0 tokens"
// (OGA-420: never fabricate usage).
func UsageFromGateway(u *gateway.Usage) (TokenUsage, bool) {
	if u == nil {
		return TokenUsage{}, false
	}
	return TokenUsage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}, true
}
