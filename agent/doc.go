// Package agent provides the AgentRuntime interface and DefaultRuntime
// reference implementation for building A2A-compliant domain agents.
//
// Kit developers have two options:
//   - Simple: Use DefaultRuntime directly — supply a profile YAML, done.
//   - Custom: Implement AgentRuntime interface — full control over request
//     handling while satisfying the A2A protocol contract.
//
// The platform interacts with agents only via the A2A HTTP protocol.
// The sidecar container is a black box.
package agent
