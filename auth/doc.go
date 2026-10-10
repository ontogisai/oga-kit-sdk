// Package auth provides token management and credential rotation for domain
// agent sidecars. The TokenManager handles sliding renewal at 50% TTL with
// atomic file rotation and exponential backoff retry on refresh failure.
package auth
